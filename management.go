package main

import (
	"embed"
	"encoding/json"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ui is the usage dashboard, served as a plugin resource. It holds no data itself:
// every call it makes is authenticated with the management key.
//
//go:embed ui.html
var ui []byte

//go:embed icons/*.png
var icons embed.FS

const (
	apiBase      = "/v0/management/usage"
	resourceBase = "/v0/resource/plugins/" + pluginIdentifier
)

type managementRoute struct {
	Method      string
	Path        string
	Description string
}

type resourceRoute struct {
	Path        string
	Menu        string
	Description string
}

func managementRegistration() map[string]any {
	resources := []resourceRoute{{Path: "/ui", Menu: displayName, Description: "Token usage, estimated cost and traffic for each provider, with a live provider map, request history and logs."}}
	entries, _ := icons.ReadDir("icons")
	for _, e := range entries {
		resources = append(resources, resourceRoute{Path: "/icons/" + e.Name()})
	}
	return map[string]any{
		"routes": []managementRoute{
			{Method: http.MethodGet, Path: apiBase + "/stats", Description: "Usage totals for a period (today, 24h, 7d, 30d, 60d or all) and the live data shown on the page. tz is the viewer's offset from UTC in minutes."},
			{Method: http.MethodGet, Path: apiBase + "/chart", Description: "Tokens, requests and cost for a period, grouped by hour for today and 24h and by day otherwise."},
			{Method: http.MethodGet, Path: apiBase + "/live", Description: "The latest requests, the providers that served a request recently, and the last provider that failed."},
			{Method: http.MethodGet, Path: apiBase + "/details", Description: "Request history, newest first. Filter by provider, apiKey (masked) and from/to (RFC 3339). Page with page and pageSize (up to 200), and send back the snapshot from page 1 so later pages do not shift when new requests arrive."},
		},
		"resources": resources,
	}
}

func handleManagement(req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	reqPath := strings.TrimRight(req.Path, "/")
	switch {
	case reqPath == resourceBase+"/ui":
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
			Body:       ui,
		}
	case strings.HasPrefix(reqPath, resourceBase+"/icons/"):
		data, errRead := icons.ReadFile("icons/" + path.Base(reqPath))
		if errRead != nil {
			return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown icon"})
		}
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"image/png"}, "Cache-Control": []string{"public, max-age=86400"}},
			Body:       data,
		}
	}

	q := req.Query
	now = now.In(viewerZone(q.Get("tz")))
	period := q.Get("period")
	if period == "" {
		period = "today"
	}
	switch reqPath {
	case apiBase + "/stats", apiBase + "/chart":
		if _, ok := periodStart(period, now); !ok {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid period"})
		}
		if reqPath == apiBase+"/chart" {
			return queryResponse(store.chart(period, now))
		}
		return queryResponse(store.stats(period, now, authProviders(now)))
	case apiBase + "/live":
		return jsonResponse(http.StatusOK, store.live(now))
	case apiBase + "/details":
		dq, errQuery := parseDetailsQuery(q.Get("page"), q.Get("pageSize"), q.Get("snapshot"), q.Get("from"), q.Get("to"))
		if errQuery != "" {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": errQuery})
		}
		dq.provider = strings.TrimSpace(q.Get("provider"))
		dq.apiKey = strings.TrimSpace(q.Get("apiKey"))
		return queryResponse(store.details(dq))
	}
	return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown route"})
}

// queryResponse answers with a query result, or a 500 when the history could not be read.
func queryResponse(v any, errQuery error) pluginapi.ManagementResponse {
	if errQuery != nil {
		hostLog("warn", "usage: query: "+errQuery.Error())
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "read usage history: " + errQuery.Error()})
	}
	return jsonResponse(http.StatusOK, v)
}

func parseDetailsQuery(page, pageSize, snapshot, from, to string) (detailsQuery, string) {
	dq := detailsQuery{page: 1, pageSize: 20}
	if page != "" {
		n, errAtoi := strconv.Atoi(page)
		if errAtoi != nil || n < 1 {
			return dq, "page must be >= 1"
		}
		dq.page = n
	}
	if pageSize != "" {
		n, errAtoi := strconv.Atoi(pageSize)
		if errAtoi != nil || n < 1 || n > 200 {
			return dq, "pageSize must be between 1 and 200"
		}
		dq.pageSize = n
	}
	if snapshot != "" {
		n, errAtoi := strconv.Atoi(snapshot)
		if errAtoi != nil || n < 1 {
			return dq, "snapshot must be >= 1"
		}
		dq.snapshot = n
	}
	for _, bound := range []struct {
		raw string
		dst *int64
	}{{from, &dq.from}, {to, &dq.to}} {
		if bound.raw == "" {
			continue
		}
		t, errParse := time.Parse(time.RFC3339, bound.raw)
		if errParse != nil {
			return dq, "from and to must be RFC 3339 timestamps"
		}
		*bound.dst = t.UnixMilli()
	}
	return dq, ""
}

// viewerZone uses the browser's UTC offset so days and hours match the viewer, not the server.
// ponytail: a fixed offset, so buckets before a DST change shift by an hour; pass an IANA zone if that matters.
func viewerZone(offsetMinutes string) *time.Location {
	minutes, errAtoi := strconv.Atoi(offsetMinutes)
	if errAtoi != nil || minutes < -14*60 || minutes > 14*60 {
		return time.Local
	}
	return time.FixedZone("viewer", minutes*60)
}

// authProviders lists providers with an enabled credential, for the topology.
func authProviders(now time.Time) []string {
	var out []string
	for _, e := range store.authEntries(now) {
		if !e.Disabled && e.Provider != "" {
			out = append(out, e.Provider)
		}
	}
	return out
}

func jsonResponse(status int, v any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		status, raw = http.StatusInternalServerError, []byte(`{"error":"encode response"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}, "Cache-Control": []string{"no-store"}},
		Body:       raw,
	}
}
