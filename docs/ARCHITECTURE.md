# PostgreSQL WAL & Shared-Buffer Interaction Visualizer — Implementation Spec

## 1. Summary

Build a visualization tool that shows how a PostgreSQL instance moves data between
**backends → shared buffers → WAL → disk**, and overlays a live **contention heatmap**
indicating where the instance is bottlenecked. The primary purpose is to teach a correct
*mental model* of the WAL/buffer subsystem and, in a second mode, to drive that same
visual from real sampled metrics so an operator can see a live bottleneck.

There are two operating modes that share **one frontend and one data contract**:

- **Teaching mode** — driven by a deterministic toy model (sliders for write load and
  `shared_buffers` size). No database connection. Used to build intuition.
- **Live mode** — driven by a metrics poller that samples a real PostgreSQL instance.
  Probabilistic sampling for the contention nodes is acceptable and expected.

The key design property: **both modes emit the identical frame payload** (see §14). The
frontend never knows or cares which mode produced a frame; it just renders frames. Build
the frontend against the frame schema first, validate with teaching mode, then add the
poller.

---

## 2. The conceptual model (CORRECTNESS-CRITICAL — read before coding)

The value of this tool collapses if the model is wrong, because it will then teach an
incorrect mental model. The following must be encoded faithfully.

### 2.1 There are two loops with different timing, not one pipeline

**Read / buffer loop (synchronous, latency-bound).** A backend that needs a page looks it
up in shared buffers (under a `BufferMapping` LWLock). On a miss it must find a victim
buffer to evict — and if that victim is dirty, the page must be written out *first* — then
read the requested page in. Random access; paid in full at execution time.

**Durability path (decoupled in time).** When a backend modifies a page, it writes a **WAL
record describing the change** into the in-memory WAL buffers (briefly under `WALInsert`),
and at commit that WAL is flushed to the WAL segments on disk (`WALWrite` + fsync). The
**dirty data page itself is NOT written at this time.** It stays in shared buffers and is
written to the data files *later and lazily* — by the checkpointer (in periodic bursts), by
the bgwriter (in the background), or — the pathological case — by a backend forced to evict
it.

WAL for a change reaches disk **ahead of** the corresponding data page. That ordering is
the entire durability guarantee ("write-ahead logging").

### 2.2 The eviction cross-coupling is the headline insight

When `shared_buffers` is too small for the working set **and** write load is high, the
dirty fraction climbs, and backends performing reads are forced to flush dirty victim pages
synchronously before they can reuse the buffer. A *read-side* pressure becomes a
*synchronous write under lock*. This is the mechanism behind "many more reads than writes,
and the instance stalls." Make this visible — it is the tool's main differentiator. No
existing monitoring tool surfaces this coupling directly.

### 2.3 Full-page writes (FPW)

The first modification of a page after a checkpoint logs the **entire 8 KB page image**
into WAL, not just the row delta. With large values (e.g. `jsonb`) and frequent
checkpoints, FPWs dominate WAL volume. Represent FPW as a special "fat WAL record" state,
not as routine traffic.

### 2.4 Traps to avoid (each will teach a wrong model)

- **Do NOT depict data pages "flowing into" WAL.** WAL records the *change*, not the page.
  The only exception is the FPW case. If page-cells slide into the WAL store, viewers will
  overestimate WAL volume and misunderstand the subsystem.
- **Do NOT make the data-file flush synchronous with commit.** It is lazy and bursty
  (checkpoint sawtooth). The WAL edge is steady; the flush edge pulses.
- **Do NOT conflate throughput with contention.** High throughput on an edge is frequently
  *healthy*. A fast WAL edge is good. The bottleneck is contention on a lock, not volume on
  an edge. These MUST be encoded on two independent visual channels (see §13.2).
- **Do NOT let "busier diagram = worse" creep into the visuals.** A legend fixing
  `color = contention` is load-bearing.

---

## 3. System architecture

```
┌──────────────────┐     frames (JSON)      ┌─────────────────────┐
│  Frontend widget │  ◄──────────────────── │  Frame source        │
│  (SVG + controls)│      WebSocket/SSE      │  ├─ Teaching: toy    │
└──────────────────┘                         │  │   model (no DB)   │
                                             │  └─ Live: poller     │
                                             └─────────┬───────────┘
                                                       │ SQL (read-only)
                                                       ▼
                                             ┌─────────────────────┐
                                             │  PostgreSQL instance │
                                             └─────────────────────┘
```

- **Frontend widget**: SVG schematic of the topology + HTML controls + readout cards.
  Mode-agnostic; renders whatever frames arrive. See §13.
- **Frame source**:
  - Teaching mode: pure function of slider inputs (§13.4). Runs entirely client-side; no
    transport needed if you prefer (but keeping the same frame shape is cleaner).
  - Live mode: the **metrics poller** (a small backend service) that samples Postgres and
    pushes frames. See §5–§12.
- **Transport**: WebSocket (preferred) or SSE pushing frame objects (§14).

Suggested stack: Node or Python for the poller; any frontend framework or vanilla
JS + SVG for the widget. No heavy graphics libraries required.

---

## 4. Operating modes summary

| | Teaching mode | Live mode |
|---|---|---|
| Data source | Toy model (§13.4) | Metrics poller (§5–§12) |
| DB connection | None | Read-only monitoring role |
| Contention values | Computed from sliders | Sampled wait-event fractions |
| Throughput values | Computed from sliders | Counter deltas |
| Frame schema | Identical (§14) | Identical (§14) |
| Use | Build intuition | Diagnose a live instance |

---

## 5. Data sources — what feeds each visual element

The visual has **5 edges** and **4 contention nodes**. The table maps each to its source.

| Element | Meaning | Source view | Column(s) | Derivation |
|---|---|---|---|---|
| `eD` | modify / dirty rate | `pg_stat_wal` | `wal_records` | rate = Δ/Δt |
| `eA` | read-in (disk → buffers) | `pg_stat_io` or `pg_stat_database` | `reads` / `blks_read` | rate; cache hit = `blks_hit/(blks_hit+blks_read)` |
| `eB` | flush (buffers → data files) | `pg_stat_io` | `writes` grouped by `backend_type`, `context` | rate; keep checkpointer / bgwriter / client-backend split |
| `eC1` | WAL insert volume | `pg_stat_wal` | `wal_bytes` | rate = Δ/Δt |
| `eC2` | WAL flush / fsync | `pg_stat_wal` | `wal_sync`, `wal_sync_time` | rate; mean fsync = Δtime/Δcount |
| dirty level (grid fill) | dirty-buffer fraction | `pg_buffercache` | `count(*) FILTER (WHERE isdirty)` / `count(*)` | level (sample sparsely; expensive) |
| `BM` node | BufferMapping contention | `pg_stat_activity` | LWLock `BufferMapping` (+ `BufferContent`) | sampled fraction (§9) |
| `WI` node | WALInsert contention | `pg_stat_activity` | LWLock `WALInsert` | sampled fraction; corroborate w/ `wal_buffers_full` |
| `WW` node | WALWrite / fsync contention | `pg_stat_activity` | LWLock `WALWrite`, IO `WALSync` | sampled fraction |
| `EV` node | backend dirty-eviction | `pg_stat_activity` + `pg_stat_io` | IO `DataFileWrite` on client backends + client-backend `writes` in `normal` context | sampled fraction, rate-corroborated (COMPOSITE — see §9.3) |

### Two source decisions that prevent bugs

1. **Drive `eD` (dirty/modify) from `wal_records`, NOT from `sum(shared_blks_dirtied)` in
   `pg_stat_statements`.** That sum is **not monotonic**: entries are evicted when
   `pg_stat_statements.max` is exceeded, so the sum can decrease with no stats reset,
   producing bogus negative deltas. `wal_records` is monotonic and every row change emits
   one.
2. **`EV` has no single clean wait event.** Treat it as a composite signal (§9.3) and
   render it with a visible "estimated" affordance the other three nodes don't need.

---

## 6. SQL queries

All queries are read-only. Run them as a dedicated monitoring role (§15).

**Fast loop — wait-state snapshot (every ~100–250 ms):**
```sql
SELECT wait_event_type, wait_event, count(*) AS n
FROM pg_stat_activity
WHERE backend_type = 'client backend' AND state = 'active'
GROUP BY 1, 2;
```
Also retain the total count of active client backends (including rows where `wait_event IS
NULL`, i.e. running on-CPU) — this is the denominator in §9.2.

**Slow loop — cumulative counters (every ~2–5 s):**
```sql
-- WAL
SELECT wal_bytes, wal_records, wal_sync, wal_sync_time,
       wal_buffers_full, stats_reset
FROM pg_stat_wal;

-- I/O, broken down by writer (relation writes are what flush to data files)
SELECT backend_type, context, object, reads, writes, evictions
FROM pg_stat_io;

-- cache hit ratio (rock-solid, version-stable)
SELECT blks_read, blks_hit, stats_reset
FROM pg_stat_database
WHERE datname = current_database();
```

**Sparse loop — dirty-buffer level (every ~15–30 s; EXPENSIVE, scans all buffers):**
```sql
SELECT count(*) FILTER (WHERE isdirty) AS dirty, count(*) AS total
FROM pg_buffercache;
```

> **Version sensitivity — verify against the target server.** The WAL and I/O statistics
> views were reorganized across recent PostgreSQL versions (notably the introduction of
> `pg_stat_io` in 16, the split-out of `pg_stat_checkpointer` in 17, and further movement of
> WAL write/sync I/O accounting in 18). Before finalizing the queries, confirm the exact
> view and column names against the deployed server's catalog and the matching official
> documentation. Fail gracefully (degrade the affected edge/node to "unavailable") if a
> column is missing rather than crashing the poller. Minimum supported version should be
> PostgreSQL 16 (for `pg_stat_io`); document the chosen minimum.

---

## 7. Sampling cadences

Three loops because the metric types have different statistical natures:

- **Fast loop (~100–250 ms)** — wait-event sampling. Point-in-time states; you need many
  samples per window to estimate a stable distribution. Accumulate bucket counts between
  slow-loop emits.
- **Slow loop (~2–5 s)** — cumulative counter snapshots; compute rate deltas; fold in the
  accumulated wait fractions; smooth; emit one frame. This is the frame cadence.
- **Sparse loop (~15–30 s)** — `pg_buffercache` for the dirty level only.

Make all three intervals configurable.

---

## 8. Delta computation and guards

Rate for any cumulative counter `k`:
```
rate(k) = max(0, (now[k] - prev[k]) / seconds_elapsed)
```

Required guards:

1. **First tick** has no `prev` — seed state and emit nothing (or emit zeros flagged
   "warming up").
2. **Stats reset** — each relevant view exposes a `stats_reset` timestamp. If it changes
   between snapshots, `now < prev` and the delta is garbage. **Skip the interval** (don't
   emit a spike) and reseed.
3. **Counter monotonicity** — only delta counters known to be monotonic. (This is why `eD`
   uses `wal_records`, not the `pg_stat_statements` dirtied sum; see §5.)
4. `seconds_elapsed` must come from a measured wall clock at snapshot time, not the nominal
   interval, because GC/scheduling jitter makes the real interval vary.

---

## 9. Wait-event sampling → contention nodes (the probabilistic part)

### 9.1 Bucketing

Map each sampled `(wait_event_type, wait_event)` to a node bucket:

```
BM:  LWLock/BufferMapping, LWLock/BufferContent*
WI:  LWLock/WALInsert
WW:  LWLock/WALWrite, IO/WALSync, IO/WALWrite
EV:  IO/DataFileWrite   (composite — see 9.3)
other: everything else (ignored for node color)
```

### 9.2 Fraction = node contention level

Over the window of fast-loop samples accumulated since the last emit:
```
contention[node] = samples_in_bucket[node] / total_active_samples
```
**Denominator choice matters and defines the meaning.** Use *total active client-backend
samples*, including on-CPU (non-waiting) ones. Then a node reads `1.0` only when essentially
all active backend-time is blocked on that resource — i.e. "this lock is where the work is
going," not merely "this lock was observed." Document this choice in the UI/help.

### 9.3 EV is a composite (be honest about it)

Backend dirty-eviction surfaces as an `IO/DataFileWrite` wait on a client backend — but so
does an ordinary backend write. So compute EV as the sampled `IO/DataFileWrite` fraction
**scaled by corroboration** from `pg_stat_io`: the share of relation `writes` attributed to
`backend_type = 'client backend'`, `context = 'normal'` (as opposed to `checkpointer` /
`bgwriter`). High backend-write share ⇒ the wait really is forced eviction. Render EV with a
visible "estimated" marker.

---

## 10. Statistical confidence

A bucket fraction `p` from `n` samples has binomial standard error:
```
SE = sqrt(p * (1 - p) / n)
```
Consequence: at low load, few backends are active, `n` per window is small, and the estimate
is noisy — precisely the regime where nothing is wrong. Therefore:

- **Gate red on a minimum sample count** (e.g. `n >= 20`). Below it, a node may not show
  red regardless of `p`.
- Optionally surface `SE` visually (a ring/band around the node) so the UI is honest about
  uncertainty rather than rendering a confident red from three samples.

---

## 11. Smoothing (apply selectively — do not over-smooth)

- **Node fractions → EWMA.** `s = α·x + (1-α)·s_prev`, `α ≈ 0.15–0.25`. Lower α is smoother
  but lags; expose α (or a "responsiveness" setting). Smoother for a 24/7 dashboard,
  snappier for interactive use.
- **Color thresholds → hysteresis** to prevent strobing at a boundary: require crossing
  `0.67` to *enter* red, but dropping below `0.60` to *leave* red. Same trick at the
  green/amber line (`0.34` / `0.30`).
- **DO NOT EWMA the flush edge `eB`.** Checkpoint flushing is genuinely bursty and that
  sawtooth is a thing the tool teaches (steady WAL vs pulsing data flush). Keep `eB`
  throughput raw or only lightly smoothed. Smooth the contention colors, not the lesson.

---

## 12. Severity → color mapping

```
color(c):  c < 0.34 -> green  (#1D9E75)   "ok"
           c < 0.67 -> amber  (#EF9F27)    "strained"
           else     -> red    (#E24B4A)    "bottleneck"
```
Apply hysteresis (§11) at the boundaries. Throughput (edge animation speed) is normalized
separately against a configurable peak or a rolling max — it is NOT colored by severity.

---

## 13. Frontend specification

### 13.1 Topology (SVG schematic)

Five components and five edges:

- **Backends** (top) — issue reads + modifications.
- **Shared buffers** (center) — a grid of page cells; dirty cells colored amber, clean cells
  neutral gray. Grid fill fraction = dirty level.
- **WAL buffers** (right) — in-memory ring.
- **Data files** (bottom-left) — heap + indexes on disk.
- **WAL segments** (bottom-right) — sequential WAL on disk.

Edges (each animated; arrowhead shows direction):
- `eD` Backends → Shared buffers (modify/dirty)
- `eA` Data files → Shared buffers (read-in on miss)
- `eB` Shared buffers → Data files (flush: checkpoint/bgwriter/eviction)
- `eC1` Backends → WAL buffers (WAL record insertion)
- `eC2` WAL buffers → WAL segments (fsync)

Contention nodes (small circles with 2-letter codes, placed on the relevant edge):
`BM` on `eA`, `EV` on `eB`, `WI` on `eC1`, `WW` on `eC2`.

### 13.2 The two visual encodings (MUST be independent)

- **Edge animation speed = throughput.** Implement with an animated `stroke-dashoffset`;
  faster motion = higher rate. Fast can be healthy.
- **Node color = contention** (§12). Independent of edge speed.

A legend must state this explicitly. This separation is the core anti-trap from §2.4.

### 13.3 Controls, readouts, accessibility

- Teaching-mode controls: **write load** slider (1–100) and **shared_buffers** size
  (small / medium / large).
- Readout cards: WAL throughput, dirty-buffer %, cache-hit %, current bottleneck (name of
  highest node, or "none yet" below threshold).
- Legend mapping `BM/WI/WW/EV` to BufferMapping / WALInsert / WALWrite·fsync / backend
  eviction, plus the speed=throughput / color=contention key.
- **Accessibility**: provide a screen-reader summary; do not rely on color alone (pair color
  with the textual bottleneck readout).
- **Dark mode**: all colors must work on light and dark backgrounds.
- **Reduced motion**: gate edge animation behind `prefers-reduced-motion: no-preference`;
  when reduced, convey throughput with a static numeric label instead of motion.

### 13.4 Teaching-mode toy model (reference implementation)

Inputs: `L = load/100` (0–1); `B ∈ {small:0.3, medium:0.65, large:1.0}`.

```
miss   = clamp((1 - B) * (0.3 + 0.7*L) + 0.05, 0, 1)
hit    = 1 - miss
dirty  = clamp(L * (1.0 - 0.45*B), 0, 1)
walMBs = round(L * 120)

BM = clamp(miss * (0.4 + 0.6*L), 0, 1)
WI = clamp(L * 0.7, 0, 1)
WW = clamp(L * 0.85 + 0.05, 0, 1)
EV = clamp(max(0, dirty - 0.45) * 2.0 * (1.3 - B) * (0.5 + 0.5*L), 0, 1)

bottleneck = argmax(EV, WW, WI, BM)   // "none yet" if max < 0.34
gridDirtyCells = round(20 * dirty)    // for a 20-cell grid
edge speed: dur(intensity) = clamp(1.9 - 1.5*intensity, 0.4, 1.9) seconds
            eA←miss, eB←dirty, eC1/eC2/eD←L
```
Validation target: with `B = small` and `L` high, `EV` goes red while others lag — this
demonstrates the eviction cross-coupling (§2.2). With `B = large`, `EV` stays low even at
high load.

### 13.5 Frontend visual development (close the perception loop)

The schematic is a hand-placed SVG with absolute coordinates (`web/index.html`,
plus the dirty-cell grid built in `web/render.js`). Editing those coordinates
"blind" is how blocks drift, edges cross, and the page outgrows the viewport.
**Verify visual changes by rendering, not by reading numbers.**

`cmd/uishoot` is the feedback loop. It serves the real `web/` assets through
`server.New` (the production path), drives headless Chrome (chromedp) across a
matrix of viewports × themes in a deterministic state (animations frozen, a fixed
teaching fixture, theme pinned), and does two things per state:

1. **Screenshots** → `web/testdata/screens/<viewport>-<theme>.png` (gitignored;
   read them back to *see* the result).
2. **Layout invariants** (the cheap, build-checkable half), exiting non-zero on
   failure:
   - no vertical page scroll at desktop viewports (`scrollHeight ≤ innerHeight`);
   - no two component boxes overlap, and no contention node sits inside a box;
   - every dirty-cell grid rect stays within the Shared-buffers box.

```sh
go run ./cmd/uishoot          # screenshots + invariants
go run ./cmd/uishoot -check    # invariants only
```

Loop: **edit → `go run ./cmd/uishoot` → read the PNGs + the report → iterate.**
When adding a component, edge, or node, extend the invariants in `cmd/uishoot`
so the new element is covered. The page must stay scroll-free on desktop; the
SVG is capped with `max-height` and letterboxes via `preserveAspectRatio` rather
than growing past the viewport, and the dense sidebar scrolls independently
(`web/style.css`). Needs a Chrome/Chromium binary (auto-detected; `CHROME_PATH`
overrides).

---

## 14. Frame schema (the contract between source and frontend)

Both modes emit this object per frame:

```json
{
  "ts": 1718300000000,
  "mode": "teaching | live",
  "edges": {
    "eD":  { "rate": 0.0, "unit": "records/s" },
    "eA":  { "rate": 0.0, "unit": "blocks/s" },
    "eB":  { "rate": 0.0, "unit": "blocks/s",
             "split": { "checkpointer": 0.0, "bgwriter": 0.0, "backend": 0.0 } },
    "eC1": { "rate": 0.0, "unit": "bytes/s" },
    "eC2": { "rate": 0.0, "unit": "syncs/s", "meanFsyncMs": 0.0 }
  },
  "nodes": {
    "BM": { "value": 0.0, "se": 0.0, "confident": true },
    "WI": { "value": 0.0, "se": 0.0, "confident": true },
    "WW": { "value": 0.0, "se": 0.0, "confident": true },
    "EV": { "value": 0.0, "se": 0.0, "confident": true, "estimated": true }
  },
  "levels": { "dirtyFraction": 0.0, "cacheHit": 0.0 },
  "bottleneck": "EV | WW | WI | BM | null",
  "warmingUp": false
}
```

Frontend rendering rules: edge animation speed from `edges.*.rate` (normalized against a
configurable peak/rolling max); node color from `nodes.*.value` via §12 with §11 hysteresis;
a node may not show red when `confident === false`; grid fill from `levels.dirtyFraction`;
readout cards from `levels` + `edges.eC1.rate` + `bottleneck`.

---

## 15. PostgreSQL access & permissions

- Create a **dedicated read-only monitoring role** granted `pg_monitor` (required to see
  other backends' `wait_event` / query state, and full I/O and WAL stats).
- `pg_buffercache` requires `CREATE EXTENSION pg_buffercache;` and SELECT on the view.
- Optionally support **`pg_wait_sampling`** as an alternative to the in-poller fast loop: if
  installed, read its aggregated histogram in the slow loop instead of self-sampling
  `pg_stat_activity`. Same downstream math (§9–§11); the only difference is an extension
  dependency vs. a few queries/second. Make this a configuration switch.
- Set `application_name` on the monitoring connection for identifiability, and a
  conservative `statement_timeout` so no monitoring query can run away.

---

## 16. Performance & safety constraints

- The fast loop hits `pg_stat_activity` several times per second — light but nonzero. Use a
  **dedicated low-priority connection**, ideally not co-located with the workload being
  measured (a separate pooled connection or a sidecar).
- `pg_buffercache` scans all shared buffers; keep it on the sparse loop and make its interval
  configurable (or allow disabling it, falling back to a derived estimate with a visible
  "estimated" marker).
- Never run monitoring queries inside a long-lived transaction that could hold snapshots.
- Degrade gracefully: if any single source is unavailable (permissions, version, extension
  missing), mark the dependent edge/node "unavailable" and keep the rest live.

---

## 17. Suggested build order

1. **Frontend against the frame schema (§14) with a static/hand-fed frame.** Get the
   topology, the two independent encodings (§13.2), legend, dark mode, reduced motion right.
2. **Teaching mode (§13.4).** Wire sliders to the toy model emitting frames. Validate the
   eviction-coupling behavior. This delivers standalone value with no DB.
3. **Counter poller (slow loop, §6–§8).** Edges + cache hit + dirty level from real
   counters, with reset/first-tick guards. Nodes still flat.
4. **Wait sampler (fast loop, §9).** Add node contention with bucketing, denominator, EV
   composite.
5. **Statistics + smoothing (§10–§11).** SE, min-n gating, EWMA, hysteresis, sawtooth
   preservation on `eB`.
6. **Hardening (§15–§16).** Permissions, version checks, graceful degradation, optional
   `pg_wait_sampling` path.

---

## 18. Acceptance criteria

- Frontend renders frames from either mode with no code differences between modes.
- Edge animation speed reflects throughput; node color reflects contention; the two are
  visibly independent (a fast edge can have a green node and vice versa).
- Teaching mode: small buffers + high load drives `EV` red while other nodes lag; large
  buffers keeps `EV` low at the same load.
- Live mode: a stats reset on the server does not produce a throughput spike (interval is
  skipped).
- Live mode: at very low load, nodes do not show confident red (min-n gating works); SE is
  larger than at high load.
- `eB` shows a checkpoint sawtooth (not smoothed flat).
- Missing extension/column degrades the affected element to "unavailable" without crashing.
- No data page is ever depicted entering the WAL store except in the FPW state.

---

## 19. Open decisions for the implementer

- Frontend framework (vanilla JS + SVG is sufficient; no heavy libs needed).
- Poller language/runtime (Node or Python both fine).
- Transport (WebSocket vs SSE).
- Whether to ship `pg_wait_sampling` support in v1 or self-sample only.
- Throughput normalization reference (fixed configured peak vs. rolling max).
- Exact minimum supported PostgreSQL version (recommend 16; confirm column names per §6).
- Whether `pg_buffercache` is required or optional with a derived fallback.

---

## Appendix A — Glossary

- **WAL** — Write-Ahead Log; durable record of changes, written before data pages.
- **Shared buffers** — PostgreSQL's in-process page cache (`shared_buffers`).
- **Dirty page** — a buffer modified in memory but not yet written to the data files.
- **HOT** — Heap-Only Tuple update; an update that avoids index maintenance (not modeled
  directly here, but relevant context: non-HOT updates inflate WAL and index buffer churn).
- **FPW** — Full-Page Write; the full 8 KB image logged on first change after a checkpoint.
- **Checkpoint** — periodic flush of all dirty buffers to data files; allows WAL recycling.
- **LWLock** — lightweight lock protecting internal shared-memory structures (e.g.
  `BufferMapping`, `WALInsert`).
- **Eviction** — reusing a buffer for a different page; if the victim is dirty it must be
  written out first.
