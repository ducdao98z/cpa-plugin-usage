package main

import (
	"database/sql"
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

// flame is the fire a provider sends into the sun on the topology after it served a request.
type flame struct {
	Left   int64 `json:"left"`   // ms the fire keeps flowing
	Failed bool  `json:"failed"` // the provider's latest request failed
}

type liveView struct {
	ActiveRequests []activeRequest  `json:"activeRequests"`
	RecentRequests []recentRequest  `json:"recentRequests"`
	ErrorProvider  string           `json:"errorProvider"`
	Flames         map[string]flame `json:"flames"`
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
	page, pageSize   int
	provider, apiKey string // apiKey is a masked client key
	from, to         int64  // unix ms, 0 = open
	snapshot         int    // newest rowid the pages show, 0 = the newest now
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

func (s *usageStore) stats(period string, now time.Time, authProviders []string) (map[string]any, error) {
	db, errDB := s.reader()
	if errDB != nil {
		return nil, errDB
	}
	from, _ := periodStart(period, now)
	rows, errQuery := db.Query(`SELECT model, provider, account, api_key, endpoint, COUNT(*), SUM(failed),
		SUM(input), SUM(output), SUM(cached), TOTAL(cost), MAX(ts)
		FROM requests WHERE ts >= ? GROUP BY model, provider, account, api_key, endpoint`, from)
	if errQuery != nil {
		return nil, errQuery
	}
	defer func() { _ = rows.Close() }()
	out := []*statsRow{}
	providers := make(map[string]bool)
	for _, p := range authProviders {
		providers[strings.ToLower(p)] = true
	}
	for rows.Next() {
		row := &statsRow{}
		if errScan := rows.Scan(&row.Model, &row.Provider, &row.Account, &row.APIKey, &row.Endpoint, &row.Requests, &row.Failures,
			&row.PromptTokens, &row.CompletionTokens, &row.CachedTokens, &row.Cost, &row.LastUsed); errScan != nil {
			return nil, errScan
		}
		out = append(out, row)
		if row.Provider != "" {
			providers[strings.ToLower(row.Provider)] = true
		}
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, errRows
	}
	live := s.live(now)
	return map[string]any{
		"rows":           out,
		"providers":      sortedKeys(providers),
		"activeRequests": live.ActiveRequests,
		"recentRequests": live.RecentRequests,
		"errorProvider":  live.ErrorProvider,
		"flames":         live.Flames,
		"total":          live.Total,
	}, nil
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
	tail := append([]record(nil), s.tail[max(0, len(s.tail)-tailSize):]...)
	sort.SliceStable(tail, func(i, j int) bool { return tail[i].TS > tail[j].TS })

	view := liveView{ActiveRequests: []activeRequest{}, RecentRequests: []recentRequest{}, Flames: map[string]flame{}, Total: s.total}
	for provider, last := range s.latest {
		if left := last.arrived + flameWindow.Milliseconds() - nowMs; left > 0 {
			view.Flames[provider] = flame{Left: left, Failed: last.failed}
		}
	}
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
func (s *usageStore) chart(period string, now time.Time) ([]chartPoint, error) {
	db, errDB := s.reader()
	if errDB != nil {
		return nil, errDB
	}
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
		var earliest sql.NullInt64
		if errMin := db.QueryRow("SELECT MIN(ts) FROM requests").Scan(&earliest); errMin != nil {
			return nil, errMin
		}
		if !earliest.Valid {
			return []chartPoint{}, nil
		}
		for d := startOfDay(time.UnixMilli(earliest.Int64).In(now.Location())); d.Before(end); d = d.AddDate(0, 0, 1) {
			starts = append(starts, d)
		}
	}

	points := make([]chartPoint, len(starts))
	if len(points) == 0 {
		return points, nil
	}
	bucket, errPrepare := db.Prepare("SELECT COUNT(*), IFNULL(SUM(input + output), 0), TOTAL(cost) FROM requests WHERE ts >= ? AND ts < ?")
	if errPrepare != nil {
		return nil, errPrepare
	}
	defer func() { _ = bucket.Close() }()
	for i, start := range starts {
		next := end
		if i+1 < len(starts) {
			next = starts[i+1]
		}
		points[i].TS = start.UnixMilli()
		if errScan := bucket.QueryRow(points[i].TS, next.UnixMilli()).Scan(&points[i].Requests, &points[i].Tokens, &points[i].Cost); errScan != nil {
			return nil, errScan
		}
	}
	return points, nil
}

// details pages through matching records, newest request first. Later pages pass the snapshot the first
// one returned, so requests arriving in between do not shift what they show.
func (s *usageStore) details(q detailsQuery) (map[string]any, error) {
	db, errDB := s.reader()
	if errDB != nil {
		return nil, errDB
	}
	// One read transaction: the snapshot, the count and the page see the same rows.
	tx, errBegin := db.Begin()
	if errBegin != nil {
		return nil, errBegin
	}
	defer func() { _ = tx.Rollback() }()
	var conds []string
	var args []any
	if q.snapshot > 0 {
		// The unary + keeps SQLite on the ts index, which already gives the order.
		conds, args = append(conds, "+rowid <= ?"), append(args, q.snapshot)
	} else if errMax := tx.QueryRow("SELECT IFNULL(MAX(rowid), 0) FROM requests").Scan(&q.snapshot); errMax != nil {
		return nil, errMax
	}
	if q.provider != "" {
		conds, args = append(conds, "provider = ?"), append(args, q.provider)
	}
	if q.apiKey != "" {
		conds, args = append(conds, "api_key = ?"), append(args, q.apiKey)
	}
	if q.from != 0 {
		conds, args = append(conds, "ts >= ?"), append(args, q.from)
	}
	if q.to != 0 {
		conds, args = append(conds, "ts <= ?"), append(args, q.to)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	var total int
	if errCount := tx.QueryRow("SELECT COUNT(*) FROM requests"+where, args...).Scan(&total); errCount != nil {
		return nil, errCount
	}
	items, errItems := scanRecords(tx.Query("SELECT "+recordColumns+" FROM requests"+where+" ORDER BY ts DESC, rowid DESC LIMIT ? OFFSET ?",
		append(args, q.pageSize, (q.page-1)*q.pageSize)...))
	if errItems != nil {
		return nil, errItems
	}
	s.mu.Lock()
	providers, apiKeys := sortedKeys(s.providers), sortedKeys(s.apiKeys)
	s.mu.Unlock()

	return map[string]any{
		"details": items,
		"pagination": map[string]int{
			"page": q.page, "pageSize": q.pageSize, "totalItems": total,
			"totalPages": (total + q.pageSize - 1) / q.pageSize, "snapshot": q.snapshot,
		},
		"providers": providers,
		"apiKeys":   apiKeys,
	}, nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
