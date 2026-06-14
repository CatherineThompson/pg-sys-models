package poller

import (
	"math"
	"testing"

	"github.com/catherinethompson/pg-sys-models/internal/frame"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestRateGuards(t *testing.T) {
	// Normal forward delta.
	if got := rate(100, 40, 2); !almost(got, 30) {
		t.Fatalf("rate forward = %v, want 30", got)
	}
	// Counter going backwards (e.g. missed reset) clamps to 0, no spike (§8).
	if got := rate(40, 100, 2); got != 0 {
		t.Fatalf("rate backward = %v, want 0", got)
	}
	// Zero / negative elapsed is guarded.
	if got := rate(100, 0, 0); got != 0 {
		t.Fatalf("rate zero-seconds = %v, want 0", got)
	}
}

func TestResetDetected(t *testing.T) {
	a := counters{walReset: "t1", dbReset: "d1", ioReset: "i1"}
	b := counters{walReset: "t1", dbReset: "d1", ioReset: "i1"}
	if resetDetected(a, b) {
		t.Fatal("identical fingerprints should not be a reset")
	}
	b.walReset = "t2"
	if !resetDetected(a, b) {
		t.Fatal("changed wal stats_reset should be detected (§8 guard 2)")
	}
}

func TestBucketOf(t *testing.T) {
	cases := []struct {
		typ, ev, want string
	}{
		{"LWLock", "BufferMapping", frame.NodeBM},
		{"LWLock", "BufferContent", frame.NodeBM},
		{"LWLock", "WALInsert", frame.NodeWI},
		{"LWLock", "WALWrite", frame.NodeWW},
		{"IO", "WALSync", frame.NodeWW},
		{"IO", "DataFileWrite", frame.NodeEV},
		{"LWLock", "SomethingElse", ""},
		{"Lock", "relation", ""},
	}
	for _, c := range cases {
		if got := bucketOf(c.typ, c.ev); got != c.want {
			t.Errorf("bucketOf(%q,%q) = %q, want %q", c.typ, c.ev, got, c.want)
		}
	}
}

func TestFractionsDenominatorAndEVScaling(t *testing.T) {
	w := newWaitWindow()
	// 100 active backends total; 40 of them blocked on WALInsert, 20 on
	// DataFileWrite (eviction candidate).
	w.addSnapshot([]waitSample{
		{Type: "LWLock", Event: "WALInsert", N: 40},
		{Type: "IO", Event: "DataFileWrite", N: 20},
	}, 100)

	// EV corroboration 0.5 halves the raw DataFileWrite fraction (§9.3).
	got := w.fractions(0.5)
	if !almost(got[frame.NodeWI].value, 0.40) {
		t.Errorf("WI fraction = %v, want 0.40 (denominator incl. on-CPU, §9.2)", got[frame.NodeWI].value)
	}
	if !almost(got[frame.NodeEV].value, 0.10) {
		t.Errorf("EV fraction = %v, want 0.10 (0.20 scaled by 0.5)", got[frame.NodeEV].value)
	}
	if got[frame.NodeWI].n != 100 {
		t.Errorf("denominator = %d, want 100", got[frame.NodeWI].n)
	}
}

func TestStandardError(t *testing.T) {
	// SE = sqrt(p(1-p)/n) (§10). p=0.5, n=100 -> 0.05.
	if got := standardError(0.5, 100); !almost(got, 0.05) {
		t.Fatalf("SE = %v, want 0.05", got)
	}
	if got := standardError(0.5, 0); got != 0 {
		t.Fatalf("SE with n=0 = %v, want 0", got)
	}
}

func TestConfidentGating(t *testing.T) {
	if confident(19, 20) {
		t.Error("n=19 below min 20 must not be confident (§10)")
	}
	if !confident(20, 20) {
		t.Error("n=20 at min must be confident")
	}
}

func TestEWMASeedsThenSmooths(t *testing.T) {
	e := newEWMA(0.5)
	if got := e.update("BM", 1.0); !almost(got, 1.0) {
		t.Fatalf("first update should seed to x, got %v", got)
	}
	// s = 0.5*0 + 0.5*1 = 0.5
	if got := e.update("BM", 0.0); !almost(got, 0.5) {
		t.Fatalf("EWMA = %v, want 0.5", got)
	}
}

func TestBottleneckThresholdAndGating(t *testing.T) {
	// All below threshold -> nil ("none yet", §13.4).
	low := map[string]frame.Node{
		frame.NodeBM: {Value: 0.1, Confident: true},
		frame.NodeEV: {Value: 0.2, Confident: true},
	}
	if b := bottleneck(low); b != nil {
		t.Fatalf("expected nil bottleneck, got %v", *b)
	}
	// EV highest and confident -> EV.
	hi := map[string]frame.Node{
		frame.NodeBM: {Value: 0.5, Confident: true},
		frame.NodeEV: {Value: 0.8, Confident: true},
	}
	if b := bottleneck(hi); b == nil || *b != frame.NodeEV {
		t.Fatalf("expected EV, got %v", b)
	}
	// Highest value but not confident -> skipped (§10).
	gated := map[string]frame.Node{
		frame.NodeEV: {Value: 0.9, Confident: false},
		frame.NodeWW: {Value: 0.5, Confident: true},
	}
	if b := bottleneck(gated); b == nil || *b != frame.NodeWW {
		t.Fatalf("expected WW (EV gated by confidence), got %v", b)
	}
}

func TestCacheHitAndFsyncDeltas(t *testing.T) {
	prev := counters{blksHit: 100, blksRead: 100, walSync: 10, walSyncTime: 50}
	cur := counters{blksHit: 190, blksRead: 110, walSync: 20, walSyncTime: 90}
	// dHit=90, dRead=10 -> 0.9
	if got := cacheHit(prev, cur); !almost(got, 0.9) {
		t.Errorf("cacheHit = %v, want 0.9", got)
	}
	// dTime=40 over dCount=10 -> 4ms
	if got := meanFsyncMs(prev, cur); !almost(got, 4) {
		t.Errorf("meanFsyncMs = %v, want 4", got)
	}
}

func TestDeltaBackendWriteShare(t *testing.T) {
	prev := counters{writesCheckpointer: 100, writesBackend: 0}
	cur := counters{writesCheckpointer: 100, writesBackend: 50} // all new writes are backend
	if got := deltaBackendWriteShare(prev, cur); !almost(got, 1.0) {
		t.Errorf("share = %v, want 1.0 (all delta from backend, §9.3)", got)
	}
	// No new writes -> 0 share.
	if got := deltaBackendWriteShare(cur, cur); got != 0 {
		t.Errorf("share with no delta = %v, want 0", got)
	}
}
