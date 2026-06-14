// Package frame defines the wire contract (spec §14) shared by both the
// teaching toy model (client-side JS) and the live Go poller. The frontend
// renders whatever frame arrives and never inspects which source produced it.
package frame

// Edge is one of the five animated edges. Rate drives animation speed
// (throughput); it is never colored by severity.
type Edge struct {
	Rate float64 `json:"rate"`
	Unit string  `json:"unit"`
	// Split is populated only for eB (flush), keeping the
	// checkpointer/bgwriter/backend breakdown. Nil for other edges.
	Split *FlushSplit `json:"split,omitempty"`
	// MeanFsyncMs is populated only for eC2 (WAL fsync). Nil otherwise.
	MeanFsyncMs *float64 `json:"meanFsyncMs,omitempty"`
	// Unavailable marks an edge whose source could not be read (missing
	// view/column/permission). The frontend renders it greyed-out (spec §16).
	Unavailable bool `json:"unavailable,omitempty"`
}

// FlushSplit breaks eB throughput down by writer.
type FlushSplit struct {
	Checkpointer float64 `json:"checkpointer"`
	Bgwriter     float64 `json:"bgwriter"`
	Backend      float64 `json:"backend"`
}

// Node is a contention node. Value (0..1) drives color (spec §12); it is
// independent of edge speed (spec §2.4).
type Node struct {
	Value     float64 `json:"value"`
	SE        float64 `json:"se"`
	Confident bool    `json:"confident"`
	// Estimated is set true only for EV, which is a composite signal (spec §9.3).
	Estimated bool `json:"estimated,omitempty"`
	// Unavailable marks a node whose source could not be sampled (spec §16).
	Unavailable bool `json:"unavailable,omitempty"`
}

// Levels carries the scalar readouts.
type Levels struct {
	DirtyFraction float64 `json:"dirtyFraction"`
	CacheHit      float64 `json:"cacheHit"`
	// DirtyEstimated marks dirtyFraction as derived rather than measured from
	// pg_buffercache (spec §16).
	DirtyEstimated bool `json:"dirtyEstimated,omitempty"`
}

// Frame is the per-tick payload (spec §14). Both modes emit this exact shape.
type Frame struct {
	TS         int64           `json:"ts"`
	Mode       string          `json:"mode"` // "teaching" | "live"
	Edges      map[string]Edge `json:"edges"`
	Nodes      map[string]Node `json:"nodes"`
	Levels     Levels          `json:"levels"`
	Bottleneck *string         `json:"bottleneck"` // "EV"|"WW"|"WI"|"BM"|null
	WarmingUp  bool            `json:"warmingUp"`
}

// Edge keys.
const (
	EdgeED  = "eD"  // backends -> shared buffers (modify/dirty)
	EdgeEA  = "eA"  // data files -> shared buffers (read-in on miss)
	EdgeEB  = "eB"  // shared buffers -> data files (flush)
	EdgeEC1 = "eC1" // backends -> WAL buffers (WAL insert)
	EdgeEC2 = "eC2" // WAL buffers -> WAL segments (fsync)
)

// Node keys.
const (
	NodeBM = "BM" // BufferMapping
	NodeWI = "WI" // WALInsert
	NodeWW = "WW" // WALWrite / fsync
	NodeEV = "EV" // backend dirty-eviction (composite)
)
