# Usage Analytics plugin for CLIProxyAPI

A usage dashboard for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI): the
[9router](https://github.com/decolua/9router) **Usage & Analytics** page (`/dashboard/usage`) ported to a
plugin and restyled on the management panel's own design system.

- **Overview:** requests, estimated cost, success and failure, input, cached and output tokens; a live provider
  topology; recent requests; a token/request/cost chart; by-provider and top-model charts; and the usage
  ledger grouped by model, account, API key or endpoint (costs or tokens, sortable, expandable).
- **Details:** request history with provider, API key and date filters, pagination and a request sheet. Later
  pages stay on the history as it was when page 1 loaded, so new requests do not shift them.
- Under steady traffic the overview refreshes after each refresh finishes, resting 4x as long as it took, so
  long periods on a big history do not keep the proxy scanning.
- **Logs:** the last 200 requests, auto-refreshing.
- Periods: Today, 24h, 7D, 30D, 60D, All, bucketed in the viewer's time zone.
- Follows the panel's theme (light, dark, white) and reuses the panel's login.

Requires CLIProxyAPI v8.0.22 or later with plugins enabled. Usage is counted from the moment the plugin
loads; earlier history is not imported.

## Install

**Manually:** download `usage_<version>_<os>_<arch>.zip` from
[Releases](https://github.com/ducdao98z/cpa-plugin-usage/releases), extract `usage.so` / `.dylib` / `.dll`
into the plugins directory (`plugins/` by default) and restart.

**From source** (Go 1.26+ and a C compiler):

```bash
go build -trimpath -buildvcs=false -buildmode=c-shared -ldflags "-s -w" -o usage.so .
```

For the official Docker image (Debian bookworm), build in a matching container so glibc fits:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26-bookworm \
  sh -c 'go build -trimpath -buildvcs=false -buildmode=c-shared -ldflags "-s -w" -o usage.so . && rm -f usage.h'
```

## Configure

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    usage:
      enabled: true
      data-file: "plugins/usage.db" # optional, this is the default (relative to the working directory)
```

Versions before 0.4.0 kept the history in `plugins/usage.jsonl`. On first start, 0.4.0 and later import that
file into the database, skipping records it already has, and keep it as `usage.jsonl.imported`; a `data-file`
that still names the `.jsonl` file means the `.db` next to it.

## Open

**Usage Analytics** under PLUGINS in the management panel, or `http://<host>:8317/v0/resource/plugins/usage/ui`.
Opened from the panel, the page reuses the panel's login; opened on its own, it asks for the management
key once and keeps it in this browser until **Forget key**.

## How it works

- `usage.handle` gives one record per finished model call; `request.complete` adds the request path
  (endpoint). Every request is a row of the `requests` table in `data-file`, a SQLite database (WAL mode,
  pure-Go driver, so no C SQLite is needed). Only the newest 100 requests stay in memory, for the live panel;
  the history is kept until you delete it, and you can query it yourself, e.g.
  `sqlite3 plugins/usage.db "SELECT model, COUNT(*) FROM requests GROUP BY model"`.
- Raw client and upstream API keys are never stored, only masked (`sk-abcde***wxyz`).
- Cost is an estimate from 9router's price table (`pricing.json`, USD per 1M tokens), not billing.
- API key names: the panel lets you name client API keys (Config → Client API Keys → Edit) and keeps the
  names in that browser only. The page reads them there and shows the name, falling back to the masked key.
- Topology: CLIProxyAPI is drawn as the sun and each provider as a planet, with busier providers on the inner
  orbits. For a minute after a provider serves a request, a stream of fire in its colour flows from its planet
  into the sun. The fire is red and sputters if that request failed, and it dies down over the last 10 seconds.
  Plugins only hear about a request once it finishes, so the map cannot show requests still in progress.
  Drag to rotate and tilt, hold to pause, Shift + drag to pan, and use Ctrl/⌘ + scroll or the buttons to zoom.
  Click a planet to open its requests in Details.

### Management API

All routes need the management key; `tz` (minutes east of UTC) sets the time zone for periods and buckets.

| Route | Purpose |
|---|---|
| `GET /v0/management/usage/stats?period=` | Usage rows, topology providers, live panel |
| `GET /v0/management/usage/chart?period=` | Hourly (today, 24h) or daily buckets |
| `GET /v0/management/usage/live` | Recent requests, active providers, how long each provider's fire keeps burning, last failing provider |
| `GET /v0/management/usage/details?page&pageSize&provider&apiKey&from&to&snapshot` | Request history, newest first; `apiKey` is a masked key, `snapshot` the first page's `pagination.snapshot` |

## Release

Push a `v<version>` tag. The Build workflow tests, builds linux/darwin/windows libraries, and publishes
`usage_<version>_<os>_<arch>.zip` plus `checksums.txt` in the layout the CLIProxyAPI plugin store installs.

## License

MIT. The dashboard design, the price table (`pricing.json`) and the provider icons come from
[9router](https://github.com/decolua/9router) (MIT, © decolua and contributors).
