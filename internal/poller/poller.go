// Package poller drives live mode: it samples a real PostgreSQL instance on
// three cadences (spec §7), computes rates with reset/first-tick guards
// (spec §8), turns wait-event samples into contention nodes (spec §9–§11), and
// emits frames on the slow cadence (spec §14). The pure transformation logic
// (delta.go, waits.go, stats.go, bottleneck.go) carries no DB dependency and is
// unit-tested directly.
package poller

import (
	"context"
	"sync"
	"time"

	"github.com/catherinethompson/pg-sys-models/internal/config"
	"github.com/catherinethompson/pg-sys-models/internal/db"
	"github.com/catherinethompson/pg-sys-models/internal/frame"
)

// Source is the subset of *db.DB the poller needs. Defining it here keeps the
// transformation testable with a fake.
type Source interface {
	SampleWaits(ctx context.Context) ([]db.WaitRow, int, error)
	ReadCounters(ctx context.Context) (db.CounterSnapshot, error)
	ReadDirtyLevel(ctx context.Context) (float64, bool, error)
	Caps() db.Capabilities
}

// Poller owns the loops and the smoothing/rolling state.
type Poller struct {
	cfg config.Config
	src Source

	mu     sync.Mutex
	window *waitWindow

	smoother *ewma

	prev     *counters
	prevTime time.Time

	dirtyFraction  float64
	dirtyEstimated bool

	out chan frame.Frame
}

// New builds a poller over the given source.
func New(cfg config.Config, src Source) *Poller {
	return &Poller{
		cfg:            cfg,
		src:            src,
		window:         newWaitWindow(),
		smoother:       newEWMA(cfg.Alpha),
		dirtyEstimated: !src.Caps().HasBuffercache,
		out:            make(chan frame.Frame, 8),
	}
}

// Frames is the stream of emitted frames (slow cadence).
func (p *Poller) Frames() <-chan frame.Frame { return p.out }

// Run starts the three loops and blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	go p.fastLoop(ctx)
	go p.sparseLoop(ctx)
	p.slowLoop(ctx) // frame cadence; runs on the caller's goroutine
}

// fastLoop accumulates wait-event samples between slow emits (spec §7/§9).
func (p *Poller) fastLoop(ctx context.Context) {
	t := time.NewTicker(p.cfg.FastInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rows, total, err := p.src.SampleWaits(ctx)
			if err != nil {
				continue // transient; next tick retries
			}
			samples := make([]waitSample, len(rows))
			for i, r := range rows {
				samples[i] = waitSample{Type: r.Type, Event: r.Event, N: r.N}
			}
			p.mu.Lock()
			p.window.addSnapshot(samples, total)
			p.mu.Unlock()
		}
	}
}

// sparseLoop refreshes the dirty-buffer level (spec §7); expensive, so rare.
func (p *Poller) sparseLoop(ctx context.Context) {
	if !p.cfg.EnableBuffercache || !p.src.Caps().HasBuffercache {
		p.mu.Lock()
		p.dirtyEstimated = true // derived/unavailable (spec §16)
		p.mu.Unlock()
		return
	}
	t := time.NewTicker(p.cfg.SparseInterval)
	defer t.Stop()
	// Prime once so the first frames aren't blank.
	p.refreshDirty(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refreshDirty(ctx)
		}
	}
}

func (p *Poller) refreshDirty(ctx context.Context) {
	frac, ok, err := p.src.ReadDirtyLevel(ctx)
	if err != nil || !ok {
		return
	}
	p.mu.Lock()
	p.dirtyFraction = frac
	p.dirtyEstimated = false
	p.mu.Unlock()
}

// slowLoop is the frame cadence (spec §7): snapshot counters, compute deltas,
// fold accumulated wait fractions, smooth, emit one frame.
func (p *Poller) slowLoop(ctx context.Context) {
	t := time.NewTicker(p.cfg.SlowInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			close(p.out)
			return
		case <-t.C:
			if f, ok := p.buildFrame(ctx); ok {
				select {
				case p.out <- f:
				default: // drop if no consumer is keeping up
				}
			}
		}
	}
}

// buildFrame produces one frame. ok=false means the interval was skipped (a
// stats reset was detected, spec §8 guard 2) and nothing should be emitted.
func (p *Poller) buildFrame(ctx context.Context) (frame.Frame, bool) {
	now := time.Now()
	snap, err := p.src.ReadCounters(ctx)
	if err != nil {
		return frame.Frame{}, false
	}
	cur := toCounters(snap)
	caps := p.src.Caps()

	// First tick: seed and emit a warming-up frame (spec §8 guard 1).
	if p.prev == nil {
		p.prev = &cur
		p.prevTime = now
		return p.warmingFrame(now), true
	}

	// Stats reset between snapshots makes the delta garbage: skip + reseed
	// (spec §8 guard 2). Emit a warming-up frame to keep the stream alive
	// without a spurious spike.
	if resetDetected(*p.prev, cur) {
		p.prev = &cur
		p.prevTime = now
		return p.warmingFrame(now), true
	}

	secs := now.Sub(p.prevTime).Seconds() // measured wall clock (spec §8 guard 4)
	prev := *p.prev

	edges := p.buildEdges(prev, cur, secs, caps)
	nodes := p.buildNodes(prev, cur, caps)

	p.mu.Lock()
	dirty := p.dirtyFraction
	dirtyEst := p.dirtyEstimated
	p.mu.Unlock()

	f := frame.Frame{
		TS:    now.UnixMilli(),
		Mode:  "live",
		Edges: edges,
		Nodes: nodes,
		Levels: frame.Levels{
			DirtyFraction:  dirty,
			CacheHit:       cacheHit(prev, cur),
			DirtyEstimated: dirtyEst,
		},
		Bottleneck: bottleneck(nodes),
		WarmingUp:  false,
	}

	p.prev = &cur
	p.prevTime = now
	return f, true
}

func (p *Poller) buildEdges(prev, cur counters, secs float64, caps db.Capabilities) map[string]frame.Edge {
	eb := frame.Edge{
		Rate: rate(cur.writesTotal(), prev.writesTotal(), secs),
		Unit: "blocks/s",
		// eB is intentionally NOT smoothed: the checkpoint sawtooth is a lesson
		// the tool teaches (spec §11, §2.4).
		Split: &frame.FlushSplit{
			Checkpointer: rate(cur.writesCheckpointer, prev.writesCheckpointer, secs),
			Bgwriter:     rate(cur.writesBgwriter, prev.writesBgwriter, secs),
			Backend:      rate(cur.writesBackend, prev.writesBackend, secs),
		},
	}
	if !caps.HasStatIO {
		eb = frame.Edge{Unit: "blocks/s", Unavailable: true}
	}

	mean := meanFsyncMs(prev, cur)
	return map[string]frame.Edge{
		frame.EdgeED:  {Rate: rate(cur.walRecords, prev.walRecords, secs), Unit: "records/s"},
		frame.EdgeEA:  {Rate: rate(cur.blksRead, prev.blksRead, secs), Unit: "blocks/s"},
		frame.EdgeEB:  eb,
		frame.EdgeEC1: {Rate: rate(cur.walBytes, prev.walBytes, secs), Unit: "bytes/s"},
		frame.EdgeEC2: {Rate: rate(cur.walSync, prev.walSync, secs), Unit: "syncs/s", MeanFsyncMs: &mean},
	}
}

func (p *Poller) buildNodes(prev, cur counters, caps db.Capabilities) map[string]frame.Node {
	// EV corroboration: recent backend-write share (spec §9.3). Without
	// pg_stat_io we cannot corroborate, so leave the raw fraction unscaled.
	evCorr := 1.0
	if caps.HasStatIO {
		evCorr = deltaBackendWriteShare(prev, cur)
	}

	p.mu.Lock()
	raw := p.window.fractions(evCorr)
	p.window.reset()
	p.mu.Unlock()

	nodes := map[string]frame.Node{}
	for _, key := range []string{frame.NodeBM, frame.NodeWI, frame.NodeWW, frame.NodeEV} {
		st := raw[key]
		smoothed := p.smoother.update(key, st.value) // EWMA node fractions (spec §11)
		nodes[key] = frame.Node{
			Value:     smoothed,
			SE:        standardError(st.value, st.n),
			Confident: confident(st.n, p.cfg.MinSamples),
			Estimated: key == frame.NodeEV, // composite signal (spec §9.3)
		}
	}
	return nodes
}

// warmingFrame is emitted before the first real delta and on skipped intervals
// (spec §8). Rates are zero; nodes are flat and not confident.
func (p *Poller) warmingFrame(now time.Time) frame.Frame {
	zero := func(unit string) frame.Edge { return frame.Edge{Unit: unit} }
	mean := 0.0
	nodes := map[string]frame.Node{}
	for _, k := range []string{frame.NodeBM, frame.NodeWI, frame.NodeWW, frame.NodeEV} {
		nodes[k] = frame.Node{Estimated: k == frame.NodeEV}
	}
	return frame.Frame{
		TS:   now.UnixMilli(),
		Mode: "live",
		Edges: map[string]frame.Edge{
			frame.EdgeED:  zero("records/s"),
			frame.EdgeEA:  zero("blocks/s"),
			frame.EdgeEB:  {Unit: "blocks/s", Split: &frame.FlushSplit{}},
			frame.EdgeEC1: zero("bytes/s"),
			frame.EdgeEC2: {Unit: "syncs/s", MeanFsyncMs: &mean},
		},
		Nodes:      nodes,
		Levels:     frame.Levels{DirtyEstimated: p.dirtyEstimated},
		Bottleneck: nil,
		WarmingUp:  true,
	}
}

// toCounters adapts a DB snapshot into the internal counters type.
func toCounters(s db.CounterSnapshot) counters {
	return counters{
		walBytes:           s.WalBytes,
		walRecords:         s.WalRecords,
		walSync:            s.WalSync,
		walSyncTime:        s.WalSyncTime,
		blksRead:           s.BlksRead,
		blksHit:            s.BlksHit,
		writesCheckpointer: s.WritesCheckpointer,
		writesBgwriter:     s.WritesBgwriter,
		writesBackend:      s.WritesBackend,
		walReset:           s.WalReset,
		dbReset:            s.DBReset,
		ioReset:            s.IOReset,
	}
}

// cacheHit computes the delta cache-hit ratio over the interval (spec §5).
func cacheHit(prev, cur counters) float64 {
	dHit := cur.blksHit - prev.blksHit
	dRead := cur.blksRead - prev.blksRead
	denom := dHit + dRead
	if denom <= 0 {
		return 0
	}
	return clamp01(dHit / denom)
}

// meanFsyncMs is the mean fsync time over the interval (spec §5/§6).
func meanFsyncMs(prev, cur counters) float64 {
	dCount := cur.walSync - prev.walSync
	if dCount <= 0 {
		return 0
	}
	return (cur.walSyncTime - prev.walSyncTime) / dCount
}

// deltaBackendWriteShare is the EV corroboration over the interval (spec §9.3):
// the share of relation writes done by client backends in normal context.
func deltaBackendWriteShare(prev, cur counters) float64 {
	dBackend := cur.writesBackend - prev.writesBackend
	dTotal := cur.writesTotal() - prev.writesTotal()
	if dTotal <= 0 {
		return 0
	}
	return clamp01(dBackend / dTotal)
}
