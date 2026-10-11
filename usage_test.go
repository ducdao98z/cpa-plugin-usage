package main

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestNormalizeTokens(t *testing.T) {
	d := pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 10, CacheReadTokens: 30, CacheCreationTokens: 5}
	for _, tc := range []struct {
		provider        string
		input, output   int64
		cached, written int64
	}{
		{"claude", 135, 60, 30, 5}, // uncached input, reasoning reported apart
		{"gemini-cli", 100, 60, 30, 5},
		{"codex", 100, 50, 30, 5}, // OpenAI style: totals already include both
	} {
		in, cached, written, out, _ := normalizeTokens(d, tc.provider, "")
		if in != tc.input || out != tc.output || cached != tc.cached || written != tc.written {
			t.Errorf("%s: got in=%d out=%d cached=%d written=%d", tc.provider, in, out, cached, written)
		}
	}
}

func TestEstimateCost(t *testing.T) {
	// claude-sonnet-4-5-20250929: input 3, output 15, cached 0.3, cache_creation 3.75 per 1M.
	got := estimateCost("claude-sonnet-4-5-20250929", 1_000_000, 200_000, 100_000, 1_000_000, 0)
	want := 0.7*3 + 0.2*0.3 + 0.1*3.75 + 15
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("exact price: got %v want %v", got, want)
	}
	if _, ok := pricing.lookup("openrouter/claude-opus-4-6"); !ok {
		t.Fatal("vendor prefix not stripped")
	}
	if p, ok := pricing.lookup("gpt-5.4-codex-high"); !ok || p.Input == 0 {
		t.Fatal("glob pattern not matched")
	}
	if estimateCost("my-local-model", 1000, 0, 0, 1000, 0) != 0 {
		t.Fatal("unknown model should cost nothing")
	}
}

func TestAccountLabel(t *testing.T) {
	s := &usageStore{authsAt: time.Now()} // empty, fresh credential cache: no host call
	for _, tc := range []struct {
		u    pluginapi.UsageRecord
		want string
	}{
		{pluginapi.UsageRecord{Source: "dev@acme.io", APIKey: "client-key-123456"}, "dev@acme.io"},
		{pluginapi.UsageRecord{Source: "sk-upstream-1234567890", APIKey: "client-key-123456"}, "sk-upstr***7890"},
		{pluginapi.UsageRecord{Source: "client-key-123456", APIKey: "client-key-123456"}, ""},
		{pluginapi.UsageRecord{AuthID: "codex-abcdef123", AuthIndex: "9"}, "Account codex-ab..."},
	} {
		if got := s.accountLabel(tc.u); got != tc.want {
			t.Errorf("accountLabel(%+v) = %q, want %q", tc.u, got, tc.want)
		}
	}
}

func TestStoreRoundTripAndQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s := &usageStore{}
	if errOpen := s.open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	zone := time.FixedZone("viewer", 7*3600)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, zone)
	add := func(trace, provider string, started time.Time, failed, generate bool) {
		s.addUsage(pluginapi.UsageRecord{
			TraceID: trace, Provider: provider, Model: "gpt-5", RequestedAt: started, Generate: generate, Failed: failed,
			APIKey: "sk-1234567890abcdef", Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5},
		}, "user@example.com", now)
	}
	s.addEndpoint("t1", "/v1/chat/completions") // completion first
	add("t1", "codex", now.Add(-time.Hour), false, true)
	add("t2", "codex", now.AddDate(0, 0, -3), true, true) // usage first, patched below
	add("t3", "xai", now.AddDate(0, 0, -40), false, true)
	add("t4", "xai", now, false, false) // count_tokens: ignored
	s.addEndpoint("t2", "/v1/messages")
	s.close()

	reloaded := &usageStore{}
	if errOpen := reloaded.open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	reloaded.addUsage(pluginapi.UsageRecord{TraceID: "t5", Provider: "codex", Model: "gpt-5", RequestedAt: now, Generate: true}, "", now)
	reloaded.close()
	s = &usageStore{}
	if errOpen := s.open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	defer s.close()

	all := must(s.details(detailsQuery{page: 1, pageSize: 10}))
	items := all["details"].([]record) // newest first: t5, t1, t2, t3
	if len(items) != 4 || items[0].Trace != "t5" || items[3].Trace != "t3" {
		t.Fatalf("records after reload = %+v", items)
	}
	if items[1].Endpoint != "/v1/chat/completions" || items[2].Endpoint != "/v1/messages" || items[3].Endpoint != "" {
		t.Fatalf("endpoints = %q %q %q", items[1].Endpoint, items[2].Endpoint, items[3].Endpoint)
	}
	if items[1].APIKey != "sk-12345***cdef" || items[1].Input != 10 || items[1].Output != 5 || !items[2].Failed {
		t.Fatalf("records = %+v", items)
	}
	if providers := all["providers"].([]string); len(providers) != 2 || providers[0] != "codex" || providers[1] != "xai" {
		t.Fatalf("providers = %v", providers)
	}
	if live := s.live(now); live.Total != 4 || len(live.RecentRequests) != 3 || len(live.ActiveRequests) != 0 {
		t.Fatalf("live after reload = %+v", live)
	}

	for period, want := range map[string][2]int64{"today": {2, 0}, "7d": {3, 1}, "all": {4, 1}} {
		var requests, failures int64
		for _, row := range must(s.stats(period, now, nil))["rows"].([]*statsRow) {
			requests += row.Requests
			failures += row.Failures
		}
		if requests != want[0] || failures != want[1] {
			t.Errorf("%s requests/failures = %d/%d, want %d/%d", period, requests, failures, want[0], want[1])
		}
	}
	week := must(s.chart("7d", now))
	if len(week) != 7 || week[6].Requests != 2 || week[3].Requests != 1 || week[6].TS != startOfDay(now).UnixMilli() || week[6].Tokens != 15 {
		t.Fatalf("7d chart = %+v", week)
	}
	if hours := must(s.chart("today", now)); len(hours) != 24 || hours[11].Requests != 1 || hours[12].Requests != 1 {
		t.Fatalf("today chart = %+v", hours)
	}
	var sum int64
	days := must(s.chart("all", now))
	for _, p := range days {
		sum += p.Requests
	}
	if len(days) != 41 || sum != 4 {
		t.Fatalf("all chart: %d days, %d requests", len(days), sum)
	}
	page := must(s.details(detailsQuery{page: 1, pageSize: 2, provider: "codex"}))
	items = page["details"].([]record)
	if len(items) != 2 || items[0].Trace != "t5" || page["pagination"].(map[string]int)["totalItems"] != 3 {
		t.Fatalf("details = %+v", page)
	}
	page = must(s.details(detailsQuery{page: 1, pageSize: 10, from: now.AddDate(0, 0, -7).UnixMilli(), to: now.Add(-time.Minute).UnixMilli()}))
	if items = page["details"].([]record); len(items) != 2 || items[0].Trace != "t1" || items[1].Trace != "t2" {
		t.Fatalf("details from/to = %+v", items)
	}
}

func TestImportJSONL(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "usage.jsonl")
	history := `{"id":"a","trace":"t1","ts":1000,"provider":"codex","model":"gpt-5","input":10,"output":5}
{"trace":"t1","endpoint":"/v1/responses"}
{"id":"b","trace":"t2","ts":2000,"provider":"xai","model":"grok-4","failed":true,"status":500}
{"id":"torn","ts":12`
	if errWrite := os.WriteFile(legacy, []byte(history), 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	s := &usageStore{}
	if errOpen := s.open(legacy); errOpen != nil { // configs before 0.4.0 name the .jsonl file
		t.Fatal(errOpen)
	}
	s.close()
	if _, errStat := os.Stat(legacy + ".imported"); errStat != nil {
		t.Fatalf("history not kept as a backup: %v", errStat)
	}
	if _, errStat := os.Stat(legacy); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatal("history left in place, so it would be imported again")
	}
	// A rollback puts the history back and adds to it; upgrading again imports only the new record.
	if errWrite := os.WriteFile(legacy, []byte(history+"\n"+`{"id":"c","ts":3000,"provider":"codex","model":"gpt-5"}`+"\n"), 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	s = &usageStore{}
	if errOpen := s.open(filepath.Join(dir, "usage.db")); errOpen != nil {
		t.Fatal(errOpen)
	}
	defer s.close()
	items := must(s.details(detailsQuery{page: 1, pageSize: 10}))["details"].([]record)
	if len(items) != 3 || items[0].ID != "c" || items[1].ID != "b" || !items[1].Failed || items[1].Status != 500 || items[2].Endpoint != "/v1/responses" || items[2].Input != 10 {
		t.Fatalf("imported = %+v", items)
	}
}

func TestOpenKeepsCountingWithoutTheDatabase(t *testing.T) {
	s := &usageStore{}
	if errOpen := s.open(t.TempDir()); errOpen == nil { // a directory, not a database
		t.Fatal("open succeeded on a directory")
	}
	defer s.close()
	s.addUsage(pluginapi.UsageRecord{Provider: "codex", Model: "gpt-5", Generate: true}, "", time.Now())
	if page := must(s.details(detailsQuery{page: 1, pageSize: 10})); page["pagination"].(map[string]int)["totalItems"] != 1 {
		t.Fatalf("in-memory history = %+v", page)
	}
}

func TestDetailsSnapshotAndKeyFilter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s := &usageStore{}
	if errOpen := s.open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	defer s.close()
	now := time.Now()
	add := func(i int, key string) {
		s.addUsage(pluginapi.UsageRecord{RequestID: strconv.Itoa(i), Provider: "codex", Model: "gpt-5", APIKey: key, Generate: true,
			RequestedAt: now.Add(time.Duration(i) * time.Second)}, "", now)
	}
	for i := 0; i < 30; i++ {
		add(i, "sk-aaaaaaaaaaaaaaaa")
	}
	first := must(s.details(detailsQuery{page: 1, pageSize: 10})) // 29..20
	for i := 30; i < 45; i++ {
		add(i, "sk-bbbbbbbbbbbbbbbb") // arrive while page 1 is read
	}
	second := must(s.details(detailsQuery{page: 2, pageSize: 10, snapshot: first["pagination"].(map[string]int)["snapshot"]}))
	if items := second["details"].([]record); len(items) != 10 || items[0].ID != "19" || items[9].ID != "10" ||
		second["pagination"].(map[string]int)["totalItems"] != 30 {
		t.Fatalf("page 2 of the first page's snapshot = %+v", second)
	}
	if fresh := must(s.details(detailsQuery{page: 1, pageSize: 10})); fresh["details"].([]record)[0].ID != "44" {
		t.Fatalf("page 1 without a snapshot = %+v", fresh["details"])
	}

	s.close()
	if errOpen := s.open(path); errOpen != nil { // the key list is rebuilt from the database
		t.Fatal(errOpen)
	}
	keyed := must(s.details(detailsQuery{page: 1, pageSize: 50, apiKey: maskKey("sk-bbbbbbbbbbbbbbbb")}))
	if n := keyed["pagination"].(map[string]int)["totalItems"]; n != 15 {
		t.Fatalf("apiKey filter matched %d records, want 15", n)
	}
	if keys := keyed["apiKeys"].([]string); len(keys) != 2 || keys[0] != "sk-aaaaa***aaaa" || keys[1] != "sk-bbbbb***bbbb" {
		t.Fatalf("apiKeys = %v", keys)
	}
}

func TestFlamesLastAMinute(t *testing.T) {
	s := &usageStore{}
	if errOpen := s.open(filepath.Join(t.TempDir(), "usage.db")); errOpen != nil {
		t.Fatal(errOpen)
	}
	defer s.close()
	now := time.Now()
	add := func(provider string, failed bool, at time.Time) {
		s.addUsage(pluginapi.UsageRecord{Provider: provider, Model: "gpt-5", Generate: true, Failed: failed, RequestedAt: at}, "", at)
	}
	add("xai", false, now.Add(-50*time.Second))
	add("xai", true, now.Add(-40*time.Second)) // the latest request decides the colour
	add("Codex", false, now)
	flames := s.live(now).Flames
	if len(flames) != 2 || flames["codex"].Left != 60_000 || flames["codex"].Failed || flames["xai"].Left != 20_000 || !flames["xai"].Failed {
		t.Fatalf("flames = %+v", flames)
	}
	if flames = s.live(now.Add(time.Minute)).Flames; len(flames) != 0 {
		t.Fatalf("flames a minute later = %+v", flames)
	}
}

// must unwraps a query result.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
