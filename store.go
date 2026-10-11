package main

import (
	"bufio"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	_ "modernc.org/sqlite" // pure Go: every platform builds without a C SQLite
)

const (
	patchWindow   = 512              // newest records searched when a request path arrives after its usage
	tailSize      = 100              // newest records kept in memory for the live panel
	liveWindow    = 5 * time.Second  // a provider counts as active this long after a request finished
	flameWindow   = time.Minute      // the topology's fire keeps flowing this long after a provider's latest request
	errorWindow   = 10 * time.Second // how long the last failing provider stays highlighted (as in 9router)
	authCacheTTL  = 30 * time.Second
	maxErrorChars = 500
)

// record is one finished model execution: a row of the requests table. Versions before 0.4.0 kept
// one JSON object per line instead, where a line without ts patches the endpoint ({"trace","endpoint"})
// of records written earlier.
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

	arrived int64 // unix ms the plugin received it; drives the live view and is not stored
}

// recordColumns names the requests columns in the order of record.fields.
const recordColumns = "id, trace, ts, provider, model, alias, account, api_key, endpoint, input, output, cached, " +
	"cache_write, reasoning, latency_ms, ttft_ms, status, failed, error, stream, cost"

// fields points at a record's values in recordColumns order, for scanning and inserting alike.
func (r *record) fields() []any {
	return []any{&r.ID, &r.Trace, &r.TS, &r.Provider, &r.Model, &r.Alias, &r.Account, &r.APIKey, &r.Endpoint, &r.Input,
		&r.Output, &r.Cached, &r.CacheWrite, &r.Reasoning, &r.LatencyMs, &r.TTFTMs, &r.Status, &r.Failed, &r.Error, &r.Stream, &r.Cost}
}

const schema = `
CREATE TABLE IF NOT EXISTS requests (
	id          TEXT    NOT NULL DEFAULT '',
	trace       TEXT    NOT NULL DEFAULT '',
	ts          INTEGER NOT NULL,
	provider    TEXT    NOT NULL DEFAULT '',
	model       TEXT    NOT NULL DEFAULT '',
	alias       TEXT    NOT NULL DEFAULT '',
	account     TEXT    NOT NULL DEFAULT '',
	api_key     TEXT    NOT NULL DEFAULT '',
	endpoint    TEXT    NOT NULL DEFAULT '',
	input       INTEGER NOT NULL DEFAULT 0,
	output      INTEGER NOT NULL DEFAULT 0,
	cached      INTEGER NOT NULL DEFAULT 0,
	cache_write INTEGER NOT NULL DEFAULT 0,
	reasoning   INTEGER NOT NULL DEFAULT 0,
	latency_ms  INTEGER NOT NULL DEFAULT 0,
	ttft_ms     INTEGER NOT NULL DEFAULT 0,
	status      INTEGER NOT NULL DEFAULT 0,
	failed      INTEGER NOT NULL DEFAULT 0,
	error       TEXT    NOT NULL DEFAULT '',
	stream      INTEGER NOT NULL DEFAULT 0,
	cost        REAL    NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS requests_ts ON requests (ts);`

var insertRecord = "INSERT INTO requests (" + recordColumns + ") VALUES (?" + strings.Repeat(", ?", strings.Count(recordColumns, ",")) + ")"

var errNotOpen = errors.New("usage history is not open")

type pathNote struct{ trace, path string }

type lastRequest struct {
	arrived int64
	failed  bool
}

// usageStore keeps the history in a SQLite database and only the newest records in memory.
// ponytail: stats and charts scan every row of their period (~1.7 s per million rows for "all");
// add hourly rollups when long periods get slow.
type usageStore struct {
	mu          sync.Mutex // orders writes and guards the fields below
	path        string
	db          *sql.DB
	tail        []record              // newest records, oldest first
	total       int                   // records stored
	providers   map[string]bool       // every provider stored, for the Details filter
	apiKeys     map[string]bool       // every masked client key stored, for the Details filter
	paths       [patchWindow]pathNote // recent trace -> request path, for usage that arrives after it
	pathPos     int
	errProvider string
	errAt       int64
	latest      map[string]lastRequest // provider (lower case) -> its newest request, for the topology's fire

	authMu  sync.Mutex
	auths   []pluginapi.HostAuthFileEntry
	authsAt time.Time
}

// open keeps the history in the SQLite database at path. A .jsonl path (configs before 0.4.0) stands for
// the .db next to it, and a JSON lines history next to the database is imported into it once.
func (s *usageStore) open(path string) error {
	legacy := strings.TrimSuffix(path, filepath.Ext(path)) + ".jsonl"
	if path == legacy {
		path = strings.TrimSuffix(path, ".jsonl") + ".db"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == path && s.db != nil {
		return nil
	}
	s.closeLocked()
	s.path = path
	db, errOpen := openDB(path)
	if errOpen != nil {
		errOpen = fmt.Errorf("open %s: %w", path, errOpen)
		// Keep counting in memory; the page still works until the file becomes usable.
		var errMemory error
		if db, errMemory = openDB(":memory:"); errMemory != nil {
			return errors.Join(errOpen, errMemory)
		}
	} else if errImport := importJSONL(db, legacy); errImport != nil {
		hostLog("warn", "usage: import "+legacy+": "+errImport.Error())
	}
	s.db = db
	return errors.Join(errOpen, s.loadLocked())
}

func openDB(path string) (*sql.DB, error) {
	if path != ":memory:" {
		if errMkdir := os.MkdirAll(filepath.Dir(path), 0o755); errMkdir != nil {
			return nil, errMkdir
		}
	}
	// WAL lets the page read while requests are written; NORMAL skips an fsync per request.
	db, errOpen := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if errOpen != nil {
		return nil, errOpen
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1) // every connection would get its own empty database
	}
	if _, errSchema := db.Exec(schema); errSchema != nil {
		_ = db.Close()
		return nil, errSchema
	}
	return db, nil
}

// loadLocked reads what stays in memory: the record count, the providers and keys, and the newest records.
func (s *usageStore) loadLocked() error {
	s.total, s.providers, s.apiKeys, s.tail, s.latest = 0, map[string]bool{}, map[string]bool{}, nil, map[string]lastRequest{}
	if errCount := s.db.QueryRow("SELECT COUNT(*) FROM requests").Scan(&s.total); errCount != nil {
		return errCount
	}
	rows, errQuery := s.db.Query("SELECT DISTINCT provider, api_key FROM requests")
	if errQuery != nil {
		return errQuery
	}
	for rows.Next() {
		var provider, apiKey string
		if rows.Scan(&provider, &apiKey) == nil {
			s.noteLocked(provider, apiKey)
		}
	}
	errRows := rows.Err()
	_ = rows.Close()
	tail, errTail := scanRecords(s.db.Query("SELECT "+recordColumns+" FROM requests ORDER BY rowid DESC LIMIT ?", tailSize))
	slices.Reverse(tail)
	s.tail = tail
	return errors.Join(errRows, errTail)
}

// importJSONL moves the JSON lines history of versions before 0.4.0 into the database, skipping records
// it already has (a history put back after a rollback). The file is renamed before the import commits,
// so it is imported once and stays as a backup.
func importJSONL(db *sql.DB, path string) error {
	records, errRead := readRecords(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil
	}
	if errRead != nil {
		return errRead
	}
	tx, errBegin := db.Begin()
	if errBegin != nil {
		return errBegin
	}
	defer func() { _ = tx.Rollback() }()
	insert, errPrepare := tx.Prepare("INSERT INTO requests (" + recordColumns + ") SELECT ?" + strings.Repeat(", ?", strings.Count(recordColumns, ",")) +
		" WHERE NOT EXISTS (SELECT 1 FROM requests WHERE ts = ? AND id = ?)")
	if errPrepare != nil {
		return errPrepare
	}
	imported := int64(0)
	for i := range records {
		res, errInsert := insert.Exec(append(records[i].fields(), records[i].TS, records[i].ID)...)
		if errInsert != nil {
			return errInsert
		}
		n, _ := res.RowsAffected()
		imported += n
	}
	done := path + ".imported"
	if errRename := os.Rename(path, done); errRename != nil {
		return errRename
	}
	if errCommit := tx.Commit(); errCommit != nil {
		_ = os.Rename(done, path) // import it again next start
		return errCommit
	}
	hostLog("info", fmt.Sprintf("usage: imported %d of %d records from %s (kept as %s)", imported, len(records), path, done))
	return nil
}

func (s *usageStore) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}

func (s *usageStore) closeLocked() {
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

// reader returns the database for queries, which need no lock: WAL lets them run beside writes.
func (s *usageStore) reader() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, errNotOpen
	}
	return s.db, nil
}

// scanRecords reads every row of a query over recordColumns.
func scanRecords(rows *sql.Rows, errQuery error) ([]record, error) {
	if errQuery != nil {
		return nil, errQuery
	}
	defer func() { _ = rows.Close() }()
	out := []record{}
	for rows.Next() {
		var r record
		if errScan := rows.Scan(r.fields()...); errScan != nil {
			return nil, errScan
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
func patchEndpoint(records []record, trace, path string) {
	for i := len(records) - 1; i >= 0 && i >= len(records)-patchWindow; i-- {
		if records[i].Trace == trace && records[i].Endpoint == "" {
			records[i].Endpoint = path
		}
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
	if s.db == nil {
		return
	}
	if _, errInsert := s.db.Exec(insertRecord, rec.fields()...); errInsert != nil {
		hostLog("warn", "usage: save request: "+errInsert.Error())
		return
	}
	s.total++
	s.tail = append(s.tail, rec)
	if len(s.tail) > 2*tailSize {
		s.tail = slices.Clone(s.tail[len(s.tail)-tailSize:]) // trim now and then, not on every request
	}
	s.noteLocked(rec.Provider, rec.APIKey)
	if rec.Provider != "" {
		s.latest[strings.ToLower(rec.Provider)] = lastRequest{rec.arrived, rec.Failed}
	}
	if rec.Failed && rec.Provider != "" {
		s.errProvider, s.errAt = strings.ToLower(rec.Provider), rec.arrived
	}
}

// noteLocked adds a record's provider and masked key to the Details filter lists.
func (s *usageStore) noteLocked(provider, apiKey string) {
	if provider != "" {
		s.providers[provider] = true
	}
	if apiKey != "" {
		s.apiKeys[apiKey] = true
	}
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
	if s.db == nil {
		return
	}
	const patch = "UPDATE requests SET endpoint = ? WHERE trace = ? AND endpoint = '' AND rowid > (SELECT IFNULL(MAX(rowid), 0) FROM requests) - ?"
	if _, errUpdate := s.db.Exec(patch, path, trace, patchWindow); errUpdate != nil {
		hostLog("warn", "usage: save request path: "+errUpdate.Error())
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
