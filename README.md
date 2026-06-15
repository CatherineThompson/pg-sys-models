# PostgreSQL WAL & Shared-Buffer Interaction Visualizer

Visualizes how a PostgreSQL instance moves data between **backends → shared
buffers → WAL → disk**, with a live **contention heatmap**. Two modes share one
frontend and one frame contract (`internal/frame` ≡ `web/frame.js`, spec §14):

- **Teaching mode** — deterministic client-side toy model (sliders, no DB).
- **Live mode** — a Go poller samples a real instance and streams frames over SSE.

The two visual encodings are **independent** (spec §2.4): edge *motion* = throughput,
node *color* = contention. A fast edge can sit beside a green node.

Full design: [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md).

## Layout

```
cmd/poller        entrypoint: serves web/ + the live SSE stream
cmd/uishoot        headless-Chrome visual check: screenshots + layout invariants
internal/frame    frame schema (§14) — the wire contract
internal/config   env-driven configuration
internal/db       read-only queries (§6), capability probe, graceful degradation
internal/poller   loops (§7), deltas+guards (§8), waits (§9), stats/smoothing (§10–§11)
internal/server   SSE fan-out + static file server
web/              vanilla JS + SVG frontend (topology, toy model, renderer)
```

## Run

### Frontend + teaching mode (no database)

```sh
go run ./cmd/poller          # serves http://localhost:8080
```

Open the page, use the **Write load** slider and **shared_buffers** selector.
Validation (spec §13.4/§18): `small` buffers + high load drives **EV** red while
the other nodes lag; `large` buffers keeps EV low at the same load.

### Live mode (against a real instance)

```sh
docker compose up -d                       # PG 16 + pg_buffercache + monitoring role
export PG_DSN='postgres://visualizer:visualizer@localhost:5432/appdb'
go run ./cmd/poller
```

Switch the page to **Live**. Drive write load (e.g. `pgbench`) against the small
`shared_buffers` instance to provoke backend eviction.

## Configuration (environment variables)

| Var | Default | Meaning |
|---|---|---|
| `PG_DSN` | _(empty)_ | Monitoring DSN. Empty ⇒ frontend + teaching only. |
| `HTTP_ADDR` | `:8080` | Listen address. |
| `WEB_DIR` | `web` | Static asset directory. |
| `FAST_INTERVAL` | `150ms` | Wait-event sampling cadence (§7). |
| `SLOW_INTERVAL` | `3s` | Frame cadence / counter snapshots (§7). |
| `SPARSE_INTERVAL` | `20s` | `pg_buffercache` dirty-level scan (§7). |
| `EWMA_ALPHA` | `0.2` | Node smoothing factor (§11). |
| `MIN_SAMPLES` | `20` | Min samples before a node may show confident red (§10). |
| `ENABLE_BUFFERCACHE` | `true` | Disable to skip the expensive dirty scan (§16). |
| `STATEMENT_TIMEOUT` | `2s` | Caps every monitoring query (§15). |

## Test

```sh
go test ./...        # delta guards, bucketing, EV composite, SE, EWMA, hysteresis, bottleneck
go vet ./...
```

### Frontend visual check

The SVG diagram is hand-placed, so visual changes are verified by *rendering*,
not by reading coordinates. `cmd/uishoot` drives headless Chrome over the real
`web/` assets and both screenshots the page and asserts layout invariants:

```sh
go run ./cmd/uishoot          # PNGs → web/testdata/screens/ + PASS/FAIL report
go run ./cmd/uishoot -check    # invariants only (CI-friendly; exits non-zero on fail)
```

Needs a Chrome/Chromium binary (auto-detected; override with `CHROME_PATH`). See
[`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) → *Frontend visual development*.

## Notes & decisions (spec §19)

- **Transport:** SSE (stdlib-only). **Min PostgreSQL:** 16 (needs `pg_stat_io`);
  startup probes columns and degrades missing sources to "unavailable" (§6, §16).
- **Throughput normalization:** per-edge rolling max with a floor of 1.0, done in
  the renderer — lets teaching intensities and live rates share one render path.
- **`pg_buffercache`:** optional; absent ⇒ dirty level marked estimated.
- **Fast loop:** self-samples `pg_stat_activity`. `pg_wait_sampling` is a documented
  future switch, not wired in this version.
