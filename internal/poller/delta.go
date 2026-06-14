package poller

// rate computes a per-second rate for a cumulative counter (spec §8):
//
//	rate(k) = max(0, (now - prev) / seconds_elapsed)
//
// It clamps negatives to zero so a counter that appears to go backwards (e.g. a
// missed stats_reset) never produces a spurious spike. seconds must be a
// measured wall-clock interval, not the nominal cadence (spec §8 guard 4).
func rate(now, prev, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	d := (now - prev) / seconds
	if d < 0 {
		return 0
	}
	return d
}

// counters is a snapshot of the monotonic cumulative counters read in the slow
// loop (spec §6). Fields mirror the source columns.
type counters struct {
	walBytes    float64
	walRecords  float64
	walSync     float64
	walSyncTime float64 // milliseconds
	blksRead    float64
	blksHit     float64

	// Relation writes (blocks) flushed to data files, split by writer (spec §5/§6).
	writesCheckpointer float64
	writesBgwriter     float64
	writesBackend      float64
	readsBackend       float64 // disk read-in blocks (eA)

	// statsReset fingerprints let us detect a server-side reset between
	// snapshots (spec §8 guard 2). Any change => skip the interval.
	walReset string
	dbReset  string
	ioReset  string
}

func (c counters) writesTotal() float64 {
	return c.writesCheckpointer + c.writesBgwriter + c.writesBackend
}

// resetDetected reports whether any stats_reset fingerprint changed between two
// snapshots, which makes the delta garbage (spec §8 guard 2).
func resetDetected(prev, now counters) bool {
	return prev.walReset != now.walReset ||
		prev.dbReset != now.dbReset ||
		prev.ioReset != now.ioReset
}
