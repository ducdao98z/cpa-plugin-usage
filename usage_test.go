package main

import (
	"math"
	"os"
	"path/filepath"
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
	path := filepath.Join(t.TempDir(), "usage.jsonl")
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

	// Simulate a crash mid-write, then reload.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"id":"torn","ts":12`)
	_ = f.Close()
	reloaded := &usageStore{}
	if errOpen := reloaded.open(path); errOpen != nil {
		t.Fatal(errOpen)
	}
	reloaded.addUsage(pluginapi.UsageRecord{TraceID: "t5", Provider: "codex", Model: "gpt-5", RequestedAt: now, Generate: true}, "", now)
	reloaded.close()
	s = &usageStore{}
	_ = s.open(path)
	if len(s.records) != 4 {
		t.Fatalf("records after reload = %d, want 4", len(s.records))
	}
	if s.records[0].Endpoint != "/v1/chat/completions" || s.records[1].Endpoint != "/v1/messages" || s.records[2].Endpoint != "" {
		t.Fatalf("endpoints = %q %q %q", s.records[0].Endpoint, s.records[1].Endpoint, s.records[2].Endpoint)
	}
	if s.records[0].APIKey != "sk-12345***cdef" || !s.records[1].Failed {
		t.Fatalf("record = %+v", s.records[1])
	}

	for period, want := range map[string][2]int64{"today": {2, 0}, "7d": {3, 1}, "all": {4, 1}} {
		var requests, failures int64
		for _, row := range s.stats(period, now, nil)["rows"].([]*statsRow) {
			requests += row.Requests
			failures += row.Failures
		}
		if requests != want[0] || failures != want[1] {
			t.Errorf("%s requests/failures = %d/%d, want %d/%d", period, requests, failures, want[0], want[1])
		}
	}
	week := s.chart("7d", now)
	if len(week) != 7 || week[6].Requests != 2 || week[3].Requests != 1 || week[6].TS != startOfDay(now).UnixMilli() {
		t.Fatalf("7d chart = %+v", week)
	}
	if hours := s.chart("today", now); len(hours) != 24 || hours[11].Requests != 1 || hours[12].Requests != 1 {
		t.Fatalf("today chart = %+v", hours)
	}
	page := s.details(detailsQuery{page: 1, pageSize: 2, provider: "codex"})
	items := page["details"].([]record)
	if len(items) != 2 || items[0].Trace != "t5" || page["pagination"].(map[string]int)["totalItems"] != 3 {
		t.Fatalf("details = %+v", page)
	}
}
