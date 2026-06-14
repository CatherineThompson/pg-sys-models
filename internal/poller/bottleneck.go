package poller

import "github.com/catherinethompson/pg-sys-models/internal/frame"

// bottleneckThreshold is the floor below which no node is reported as the
// bottleneck (spec §13.4: "none yet" if max < 0.34).
const bottleneckThreshold = 0.34

// bottleneck returns the highest-contention node, or nil if the maximum is
// below the threshold. Ordering on ties follows the spec's argmax listing
// EV, WW, WI, BM (spec §13.4) so the most severe coupling wins ties.
func bottleneck(nodes map[string]frame.Node) *string {
	order := []string{frame.NodeEV, frame.NodeWW, frame.NodeWI, frame.NodeBM}
	best := ""
	bestVal := bottleneckThreshold
	for _, k := range order {
		n := nodes[k]
		// An unavailable or under-sampled node must not be declared the
		// bottleneck (spec §10 min-n honesty, §16 degradation).
		if n.Unavailable || !n.Confident {
			continue
		}
		if n.Value > bestVal {
			bestVal = n.Value
			best = k
		}
	}
	if best == "" {
		return nil
	}
	return &best
}
