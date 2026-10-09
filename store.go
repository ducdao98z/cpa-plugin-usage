package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	patchWindow   = 512              // records searched when a request path arrives after its usage
	liveWindow    = 5 * time.Second  // a provider counts as active this long after a request finished
	errorWindow   = 10 * time.Second // how long the last failing provider stays highlighted (as in 9router)
	authCacheTTL  = 30 * time.Second
	maxErrorChars = 500
)

// record is one finished model execution, stored as one JSON object per line.
// A line without ts is an endpoint patch: {"trace","endpoint"} for records written earlier.
type record struct {
	ID         string  `json:"id,omitempty"`
	Trace      string  `json:"trace,omitempty"`
	TS         int64   `json:"ts,omitempty"` // request start, unix ms
	Provider   string  `json:"provider,omitempty"`
	Model      string  `json:"model,omitempty"`
	Alias      string  `json:"alias,omitempty"`
	Account    string  `json:"account,omitempty"`
	APIKey     string  `json:"api_key,omitempty"` // masked client key
	Endpoint   string  `json:"endpoint,omitempty"`
	Input      int64   `json:"input,omitempty"`  // input tokens, cache included
	Output     int64   `json:"output,omitempty"` // output tokens, reasoning included
	Cached     int64   `json:"cached,omitempty"` // cache read tokens
	CacheWrite int64   `json:"cache_write,omitempty"`
	Reasoning  int64   `json:"reasoning,omitempty"`
	LatencyMs  int64   `json:"latency_ms,omitempty"`
	TTFTMs     int64   `json:"ttft_ms,omitempty"`
	Status     int     `json:"status,omitempty"`
	Failed     bool    `json:"failed,omitempty"`
	Error      string  `json:"error,omitempty"`
	Stream     bool    `json:"stream,omitempty"`
	Cost       float64 `json:"cost,omitempty"`

	arrived int64 // unix ms the plugin received it; drives the live view and is not persisted
}

type pathNote struct{ trace, path string }

// usageStore keeps every record in memory and appends it to the data file.
// ponytail: whole history in memory (~200 B per request) and full scans per query;
// fine to a few million requests, add daily rollups + raw retention past that.
type usageStore struct {
	mu          sync.Mutex
	path        string
	file        *os.File
	records     []record
	paths       [patchWindow]pathNote // recent trace -> request path, for usage that arrives after it
	pathPos     int
	errProvider string
	errAt       int64

	authMu  sync.Mutex
	auths   []pluginapi.HostAuthFileEntry
	authsAt time.Time
}

// open loads the history at path and appends new records to it.
func (s *usageStore) open(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == path && s.file != nil {
		return nil
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	s.path = path
	records, errRead := readRecords(path)
	if errRead != nil && !errors.Is(errRead, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, errRead)
	}
	s.records = records
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o755); errMkdir != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), errMkdir)
	}
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if errOpen != nil {
		return fmt.Errorf("open %s: %w", path, errOpen)
	}
	// A crash can leave a torn last line; start on a fresh line so the next record stays readable.
	if info, errStat := file.Stat(); errStat == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, errLast := file.ReadAt(last, info.Size()-1); errLast == nil && last[0] != '\n' {
			_, _ = file.Write([]byte{'\n'})
		}
	}
	s.file = file
	return nil
}

func (s *usageStore) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

func readRecords(path string) ([]record, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return nil, errOpen
	}
	defer func() { _ = file.Close() }()
	var out []record
	reader := bufio.NewReader(file)
	for {
		line, errLine := reader.ReadBytes('\n')
		if len(line) > 0 {
			var rec record
			if json.Unmarshal(line, &rec) == nil { // skip torn lines
				if rec.TS != 0 {
					out = append(out, rec)
				} else if rec.Trace != "" && rec.Endpoint != "" {
					patchEndpoint(out, rec.Trace, rec.Endpoint)
				}
			}
		}
		if errors.Is(errLine, io.EOF) {
			return out, nil
		}
		if errLine != nil {
			return out, errLine
		}
	}
}

// patchEndpoint fills the endpoint of recent records from the same inbound request.
func patchEndpoint(records []record, trace, path string) bool {
	patched := false
	for i := len(records) - 1; i >= 0 && i >= len(records)-patchWindow; i-- {
		if records[i].Trace == trace && records[i].Endpoint == "" {
			records[i].Endpoint = path
			patched = true
		}
	}
	return patched
}

func (s *usageStore) writeLine(v any) {
	if s.file == nil {
		return
	}
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return
	}
	if _, errWrite := s.file.Write(append(raw, '\n')); errWrite != nil {
		hostLog("warn", "usage: write "+s.path+": "+errWrite.Error())
	}
}

func (s *usageStore) addUsage(u pluginapi.UsageRecord, account string, now time.Time) {
	if !u.Generate {
		return // token counting and other non-generating calls are not usage
	}
	rec := newRecord(u, account, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.Trace != "" {
		for _, note := range s.paths {
			if note.trace == rec.Trace {
				rec.Endpoint = note.path
				break
			}
		}
	}
	s.records = append(s.records, rec)
	if rec.Failed && rec.Provider != "" {
		s.errProvider, s.errAt = strings.ToLower(rec.Provider), rec.arrived
	}
	s.writeLine(rec)
}

// addEndpoint records the request path reported when an inbound request completes.
// Usage and completion events arrive independently, so either may come first.
func (s *usageStore) addEndpoint(trace, path string) {
	if trace == "" || path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths[s.pathPos%patchWindow] = pathNote{trace: trace, path: path}
	s.pathPos++
	if patchEndpoint(s.records, trace, path) {
		s.writeLine(record{Trace: trace, Endpoint: path})
	}
}

func newRecord(u pluginapi.UsageRecord, account string, now time.Time) record {
	input, cached, cacheWrite, output, reasoning := normalizeTokens(u.Detail, u.Provider, u.ExecutorType)
	started := u.RequestedAt
	if started.IsZero() {
		started = now
	}
	model := strings.TrimSpace(u.Model)
	if model == "" {
		model = "unknown"
	}
	alias := strings.TrimSpace(u.Alias)
	if alias == model {
		alias = ""
	}
	rec := record{
		ID:         strings.TrimSpace(u.RequestID),
		Trace:      strings.TrimSpace(u.TraceID),
		TS:         started.UnixMilli(),
		Provider:   strings.TrimSpace(u.Provider),
		Model:      model,
		Alias:      alias,
		Account:    account,
		APIKey:     maskKey(strings.TrimSpace(u.APIKey)),
		Input:      input,
		Output:     output,
		Cached:     cached,
		CacheWrite: cacheWrite,
		Reasoning:  reasoning,
		LatencyMs:  u.Latency.Milliseconds(),
		TTFTMs:     u.TTFT.Milliseconds(),
		Status:     http.StatusOK,
		Stream:     u.Stream,
		arrived:    now.UnixMilli(),
	}
	if u.Failed {
		rec.Failed = true
		rec.Status = u.Failure.StatusCode
		rec.Error = truncate(strings.TrimSpace(u.Failure.Body), maxErrorChars)
	}
	rec.Cost = estimateCost(model, input, cached, cacheWrite, output, reasoning)
	return rec
}

// normalizeTokens returns cache-inclusive input and reasoning-inclusive output totals.
// The provider semantics mirror tokenAccountingSemanticsFor in CLIProxyAPI sdk/cliproxy/usage/accounting.go.
func normalizeTokens(d pluginapi.UsageDetail, provider, executor string) (input, cached, cacheWrite, output, reasoning int64) {
	input, output = d.InputTokens, d.OutputTokens
	cached, cacheWrite, reasoning = d.CacheReadTokens, d.CacheCreationTokens, d.ReasoningTokens
	if cached == 0 && d.CachedTokens > 0 && input == 0 && output == 0 && reasoning == 0 && cacheWrite == 0 && d.TotalTokens == 0 {
		cached = d.CachedTokens
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	e := strings.ToLower(strings.TrimSpace(executor))
	both := p + " " + e
	switch {
	case e == "openaicompatexecutor" || p == "openai-compatibility" || strings.HasPrefix(p, "openai-compatible-"):
	case strings.Contains(both, "claude") || strings.Contains(both, "anthropic"):
		input += cached + cacheWrite // Anthropic reports uncached input and reasoning separately
		output += reasoning
	case containsAny(both, "gemini", "aistudio", "antigravity", "vertex", "interaction"):
		output += reasoning // Gemini reports thoughts separately; input already includes cache
	}
	return input, cached, cacheWrite, output, reasoning
}

//go:embed pricing.json
var pricingJSON []byte

// price is a model rate in USD per 1M tokens (table ported from 9router open-sse/providers/pricing.js).
type price struct {
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	Cached        float64 `json:"cached"`
	Reasoning     float64 `json:"reasoning"`
	CacheCreation float64 `json:"cache_creation"`
}

type pricePattern struct {
	re    *regexp.Regexp
	price price
}

type pricingTable struct {
	models   map[string]price
	patterns []pricePattern
}

var pricing = loadPricing(pricingJSON)

func loadPricing(raw []byte) pricingTable {
	var table struct {
		Models   map[string]price `json:"models"`
		Patterns []struct {
			Pattern string `json:"pattern"`
			Pricing price  `json:"pricing"`
		} `json:"patterns"`
	}
	if json.Unmarshal(raw, &table) != nil {
		return pricingTable{}
	}
	out := pricingTable{models: table.Models}
	for _, p := range table.Patterns {
		parts := strings.Split(p.Pattern, "*")
		for i := range parts {
			parts[i] = regexp.QuoteMeta(parts[i])
		}
		if re, errCompile := regexp.Compile("(?i)^" + strings.Join(parts, ".*") + "$"); errCompile == nil {
			out.patterns = append(out.patterns, pricePattern{re: re, price: p.Pricing})
		}
	}
	return out
}

// lookup follows 9router: exact model (vendor prefix stripped first), then glob patterns in order.
func (t pricingTable) lookup(model string) (price, bool) {
	if i := strings.IndexByte(model, '('); i > 0 {
		model = model[:i] // thinking suffix, e.g. gemini-2.5-pro(8192)
	}
	base := model[strings.LastIndexByte(model, '/')+1:]
	if p, ok := t.models[base]; ok {
		return p, true
	}
	if p, ok := t.models[model]; ok {
		return p, true
	}
	for _, p := range t.patterns {
		if p.re.MatchString(base) || p.re.MatchString(model) {
			return p.price, true
		}
	}
	return price{}, false
}

func estimateCost(model string, input, cached, cacheWrite, output, reasoning int64) float64 {
	p, ok := pricing.lookup(model)
	if !ok {
		return 0
	}
	uncached := max(0, input-cached-cacheWrite)
	plain := max(0, output-reasoning)
	cost := float64(uncached)*p.Input +
		float64(cached)*orRate(p.Cached, p.Input) +
		float64(cacheWrite)*orRate(p.CacheCreation, p.Input) +
		float64(plain)*p.Output +
		float64(reasoning)*orRate(p.Reasoning, p.Output)
	return cost / 1e6
}

func orRate(rate, fallback float64) float64 {
	if rate > 0 {
		return rate
	}
	return fallback
}

// accountLabel names the credential that served a request, without ever keeping a raw key.
func (s *usageStore) accountLabel(u pluginapi.UsageRecord) string {
	authID, authIndex := strings.TrimSpace(u.AuthID), strings.TrimSpace(u.AuthIndex)
	if authID != "" || authIndex != "" {
		for _, e := range s.authEntries(time.Now()) {
			if (authID != "" && e.ID == authID) || (authIndex != "" && e.AuthIndex == authIndex) {
				return entryLabel(e)
			}
		}
	}
	source := strings.TrimSpace(u.Source)
	if source == strings.TrimSpace(u.APIKey) {
		source = "" // no credential (e.g. a plugin executor): the host reports the client key here
	}
	switch {
	case strings.Contains(source, "@"):
		return source
	case source != "":
		return maskKey(source) // config credentials report their API key here
	case authID != "":
		return "Account " + truncate(authID, 8) + "..."
	}
	return ""
}

func entryLabel(e pluginapi.HostAuthFileEntry) string {
	if e.AccountType == "api_key" && e.Account != "" {
		return firstNonEmpty(e.Label, e.Provider) + " " + maskKey(e.Account)
	}
	return firstNonEmpty(e.Label, e.Email, e.Account, e.Name, e.ID)
}

// authEntries lists the host credentials, cached briefly (also after a failed call).
func (s *usageStore) authEntries(now time.Time) []pluginapi.HostAuthFileEntry {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if !s.authsAt.IsZero() && now.Sub(s.authsAt) < authCacheTTL {
		return s.auths
	}
	s.authsAt = now
	raw, errCall := callHost(pluginabi.MethodHostAuthList, struct{}{})
	if errCall != nil {
		hostLog("warn", "usage: list credentials: "+errCall.Error())
		return s.auths
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if json.Unmarshal(raw, &resp) == nil {
		s.auths = resp.Files
	}
	return s.auths
}

// maskKey keeps the head and tail of a key, as 9router does for API keys.
func maskKey(key string) string {
	switch {
	case key == "":
		return ""
	case len(key) <= 12:
		return key[:1] + "***"
	default:
		return key[:8] + "***" + key[len(key)-4:]
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
