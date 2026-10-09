package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// statsRow aggregates records sharing model, provider, account, API key and endpoint.
// The page derives every breakdown (provider, model, account, key, endpoint) from these rows.
type statsRow struct {
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	Account          string  `json:"account,omitempty"`
	APIKey           string  `json:"apiKey,omitempty"`
	Endpoint         string  `json:"endpoint,omitempty"`
	Requests         int64   `json:"requests"`
	Failures         int64   `json:"failures"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	Cost             float64 `json:"cost"`
	LastUsed         int64   `json:"lastUsed"`
}

type rowKey struct{ model, provider, account, apiKey, endpoint string }

type activeRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Account  string `json:"account"`
	Count    int    `json:"count"`
}

type recentRequest struct {
	Timestamp        int64  `json:"timestamp"`
	Model            string `json:"model"`
	Provider         string `json:"provider"`
	PromptTokens     int64  `json:"promptTokens"`
	CompletionTokens int64  `json:"completionTokens"`
	CachedTokens     int64  `json:"cachedTokens"`
	Status           string `json:"status"`
}

type liveView struct {
	ActiveRequests []activeRequest `json:"activeRequests"`
	RecentRequests []recentRequest `json:"recentRequests"`
	ErrorProvider  string          `json:"errorProvider"`
	// Total counts every stored record; the page reloads its stats when it changes.
	Total int `json:"total"`
}

type chartPoint struct {
	TS       int64   `json:"ts"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	Requests int64   `json:"requests"`
}

type detailsQuery struct {
	page, pageSize int
	provider       string
	from, to       int64 // unix ms, 0 = open
}

// periodStart returns the first unix ms of a period; now carries the viewer's time zone.
func periodStart(period string, now time.Time) (int64, bool) {
	day := startOfDay(now)
	switch period {
	case "today":
		return day.UnixMilli(), true
	case "24h":
		return now.Add(-24 * time.Hour).UnixMilli(), true
	case "7d":
		return day.AddDate(0, 0, -6).UnixMilli(), true
	case "30d":
		return day.AddDate(0, 0, -29).UnixMilli(), true
	case "60d":
		return day.AddDate(0, 0, -59).UnixMilli(), true
	case "all":
		return math.MinInt64, true
	}
	return 0, false
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func (s *usageStore) stats(period string, now time.Time, authProviders []string) map[string]any {
	from, _ := periodStart(period, now)
	s.mu.Lock()
	rows := make(map[rowKey]*statsRow)
	for i := range s.records {
		r := &s.records[i]
		if r.TS < from {
			continue
		}
		key := rowKey{r.Model, r.Provider, r.Account, r.APIKey, r.Endpoint}
		row := rows[key]
		if row == nil {
			row = &statsRow{Model: r.Model, Provider: r.Provider, Account: r.Account, APIKey: r.APIKey, Endpoint: r.Endpoint}
			rows[key] = row
		}
		row.Requests++
		if r.Failed {
			row.Failures++
		}
		row.PromptTokens += r.Input
		row.CompletionTokens += r.Output
		row.CachedTokens += r.Cached
		row.Cost += r.Cost
		row.LastUsed = max(row.LastUsed, r.TS)
	}
	live := s.liveLocked(now)
	s.mu.Unlock()

	out := make([]*statsRow, 0, len(rows))
	providers := make(map[string]bool)
	for _, p := range authProviders {
		providers[strings.ToLower(p)] = true
	}
	for _, row := range rows {
		out = append(out, row)
		if row.Provider != "" {
			providers[strings.ToLower(row.Provider)] = true
		}
	}
	return map[string]any{
		"rows":           out,
		"providers":      sortedKeys(providers),
		"activeRequests": live.ActiveRequests,
		"recentRequests": live.RecentRequests,
		"errorProvider":  live.ErrorProvider,
		"total":          live.Total,
	}
}

func (s *usageStore) live(now time.Time) liveView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveLocked(now)
}

// liveLocked mirrors 9router's live panel from the newest records.
// ponytail: plugins only hear about a request when it finishes, so "active" means it finished
// within liveWindow; true in-flight tracking needs a request interceptor on the hot path.
func (s *usageStore) liveLocked(now time.Time) liveView {
	nowMs := now.UnixMilli()
	tail := append([]record(nil), s.records[max(0, len(s.records)-100):]...)
	sort.SliceStable(tail, func(i, j int) bool { return tail[i].TS > tail[j].TS })

	view := liveView{ActiveRequests: []activeRequest{}, RecentRequests: []recentRequest{}, Total: len(s.records)}
	active := make(map[[3]string]int)
	seen := make(map[string]bool)
	for _, r := range tail {
		if r.arrived >= nowMs-liveWindow.Milliseconds() {
			key := [3]string{r.Provider, r.Model, r.Account}
			if _, ok := active[key]; !ok {
				active[key] = len(view.ActiveRequests)
				view.ActiveRequests = append(view.ActiveRequests, activeRequest{Provider: r.Provider, Model: r.Model, Account: r.Account})
			}
			view.ActiveRequests[active[key]].Count++
		}
		if r.Input == 0 && r.Output == 0 || len(view.RecentRequests) == 20 {
			continue
		}
		minute := time.UnixMilli(r.TS).UTC().Format("2006-01-02T15:04")
		dedupe := strings.Join([]string{r.Model, r.Provider, strconv.FormatInt(r.Input, 10), strconv.FormatInt(r.Output, 10), minute}, "|")
		if seen[dedupe] {
			continue
		}
		seen[dedupe] = true
		status := "ok"
		if r.Failed {
			status = "error"
		}
		view.RecentRequests = append(view.RecentRequests, recentRequest{
			Timestamp: r.TS, Model: r.Model, Provider: r.Provider,
			PromptTokens: r.Input, CompletionTokens: r.Output, CachedTokens: r.Cached, Status: status,
		})
	}
	if nowMs-s.errAt < errorWindow.Milliseconds() {
		view.ErrorProvider = s.errProvider
	}
	return view
}

// chart buckets a period by hour (today, 24h) or by day, in the viewer's time zone.
func (s *usageStore) chart(period string, now time.Time) []chartPoint {
	day := startOfDay(now)
	var starts []time.Time
	end := day.AddDate(0, 0, 1)
	switch period {
	case "today":
		for i := 0; i < 24; i++ {
			starts = append(starts, day.Add(time.Duration(i)*time.Hour))
		}
	case "24h":
		first := now.Add(-24 * time.Hour)
		for i := 0; i < 24; i++ {
			starts = append(starts, first.Add(time.Duration(i)*time.Hour))
		}
		end = now.Add(time.Millisecond)
	case "7d", "30d", "60d":
		days := map[string]int{"7d": 7, "30d": 30, "60d": 60}[period]
		for i := days - 1; i >= 0; i-- {
			starts = append(starts, day.AddDate(0, 0, -i))
		}
	case "all":
		s.mu.Lock()
		earliest := int64(math.MaxInt64)
		for i := range s.records {
			earliest = min(earliest, s.records[i].TS)
		}
		s.mu.Unlock()
		if earliest == math.MaxInt64 {
			return []chartPoint{}
		}
		for d := startOfDay(time.UnixMilli(earliest).In(now.Location())); d.Before(end); d = d.AddDate(0, 0, 1) {
			starts = append(starts, d)
		}
	}

	points := make([]chartPoint, len(starts))
	bounds := make([]int64, len(starts))
	for i, start := range starts {
		points[i].TS = start.UnixMilli()
		bounds[i] = points[i].TS
	}
	if len(bounds) == 0 {
		return points
	}
	first, last := bounds[0], end.UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		r := &s.records[i]
		if r.TS < first || r.TS >= last {
			continue
		}
		idx := sort.Search(len(bounds), func(j int) bool { return bounds[j] > r.TS }) - 1
		points[idx].Tokens += r.Input + r.Output
		points[idx].Cost += r.Cost
		points[idx].Requests++
	}
	return points
}

// details pages through matching records, newest request first.
func (s *usageStore) details(q detailsQuery) map[string]any {
	s.mu.Lock()
	providers := make(map[string]bool)
	matched := make([]int, 0, 64)
	for i := range s.records {
		r := &s.records[i]
		if r.Provider != "" {
			providers[r.Provider] = true
		}
		if (q.provider != "" && r.Provider != q.provider) || (q.from != 0 && r.TS < q.from) || (q.to != 0 && r.TS > q.to) {
			continue
		}
		matched = append(matched, i)
	}
	sort.SliceStable(matched, func(a, b int) bool { return s.records[matched[a]].TS > s.records[matched[b]].TS })
	total := len(matched)
	lo := min(total, (q.page-1)*q.pageSize)
	hi := min(total, lo+q.pageSize)
	items := make([]record, 0, hi-lo)
	for _, i := range matched[lo:hi] {
		items = append(items, s.records[i])
	}
	s.mu.Unlock()

	return map[string]any{
		"details": items,
		"pagination": map[string]int{
			"page": q.page, "pageSize": q.pageSize, "totalItems": total,
			"totalPages": (total + q.pageSize - 1) / q.pageSize,
		},
		"providers": sortedKeys(providers),
	}
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
