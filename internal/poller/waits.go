package poller

import "github.com/catherinethompson/pg-sys-models/internal/frame"

// waitSample is one row of the fast-loop wait snapshot (spec §6): a
// (type, event) pair and how many active client backends were observed in it.
type waitSample struct {
	Type  string
	Event string
	N     int
}

// bucketOf maps a (wait_event_type, wait_event) pair to a contention node
// bucket (spec §9.1). It returns "" for events that do not color any node.
func bucketOf(waitType, event string) string {
	switch waitType {
	case "LWLock":
		switch event {
		case "BufferMapping", "BufferContent":
			return frame.NodeBM
		case "WALInsert":
			return frame.NodeWI
		case "WALWrite":
			return frame.NodeWW
		}
	case "IO":
		switch event {
		case "WALSync", "WALWrite":
			return frame.NodeWW
		case "DataFileWrite":
			return frame.NodeEV // composite, see waitWindow.fractions / §9.3
		}
	}
	return ""
}

// waitWindow accumulates fast-loop samples between slow-loop emits (spec §7/§9).
type waitWindow struct {
	buckets     map[string]int // node -> sampled count
	totalActive int            // denominator: all active client backends incl. on-CPU (§9.2)
}

func newWaitWindow() *waitWindow {
	return &waitWindow{buckets: map[string]int{}}
}

// addSnapshot folds one fast-loop snapshot into the window. totalActive is the
// count of active client backends including non-waiting (on-CPU) ones, which is
// the denominator that gives a node its "this is where the work is" meaning
// (spec §9.2).
func (w *waitWindow) addSnapshot(samples []waitSample, totalActive int) {
	w.totalActive += totalActive
	for _, s := range samples {
		if b := bucketOf(s.Type, s.Event); b != "" {
			w.buckets[b] += s.N
		}
	}
}

// reset clears the window after a frame is emitted.
func (w *waitWindow) reset() {
	w.buckets = map[string]int{}
	w.totalActive = 0
}

// nodeStat is the raw (pre-smoothing) result for one node over a window.
type nodeStat struct {
	value float64 // fraction p
	n     int     // denominator (total active samples)
}

// fractions computes each node's raw contention fraction p = bucket/total over
// the accumulated window (spec §9.2). evCorroboration in [0,1] scales the EV
// fraction by the backend-write share from pg_stat_io (spec §9.3); pass 1.0 to
// leave EV unscaled (e.g. when pg_stat_io is unavailable).
func (w *waitWindow) fractions(evCorroboration float64) map[string]nodeStat {
	out := map[string]nodeStat{}
	denom := w.totalActive
	for _, node := range []string{frame.NodeBM, frame.NodeWI, frame.NodeWW, frame.NodeEV} {
		var p float64
		if denom > 0 {
			p = float64(w.buckets[node]) / float64(denom)
		}
		if node == frame.NodeEV {
			p *= clamp01(evCorroboration)
		}
		out[node] = nodeStat{value: p, n: denom}
	}
	return out
}

// backendWriteShare returns the share of relation writes attributed to client
// backends in the normal context (spec §9.3): high share corroborates that an
// IO/DataFileWrite wait is a forced eviction rather than an ordinary write.
func backendWriteShare(c counters) float64 {
	total := c.writesTotal()
	if total <= 0 {
		return 0
	}
	return clamp01(c.writesBackend / total)
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
