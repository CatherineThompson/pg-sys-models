package poller

import "math"

// standardError is the binomial standard error of a fraction p from n samples
// (spec §10): SE = sqrt(p*(1-p)/n). Returns 0 for n<=0.
func standardError(p float64, n int) float64 {
	if n <= 0 {
		return 0
	}
	return math.Sqrt(p * (1 - p) / float64(n))
}

// ewma is a stateful exponential moving average per node (spec §11):
// s = α·x + (1-α)·s_prev. The first observation seeds the state.
type ewma struct {
	alpha float64
	state map[string]float64
}

func newEWMA(alpha float64) *ewma {
	return &ewma{alpha: alpha, state: map[string]float64{}}
}

// update folds x into the smoothed series for key and returns the new value.
func (e *ewma) update(key string, x float64) float64 {
	prev, ok := e.state[key]
	if !ok {
		e.state[key] = x
		return x
	}
	s := e.alpha*x + (1-e.alpha)*prev
	e.state[key] = s
	return s
}

// confident reports whether a node has enough samples to be allowed to show a
// confident (e.g. red) state (spec §10 min-n gating).
func confident(n, minSamples int) bool {
	return n >= minSamples
}
