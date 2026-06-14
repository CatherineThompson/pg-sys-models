// Package config holds runtime configuration for the live poller.
package config

import (
	"os"
	"strconv"
	"time"
)

// Config is loaded from environment variables with conservative defaults.
type Config struct {
	// DSN is the libpq/pgx connection string for the read-only monitoring role
	// (spec §15). Empty DSN runs the server in frontend-only mode (teaching mode
	// still works fully client-side; live mode reports unavailable).
	DSN string

	// HTTPAddr is the listen address for static files + the SSE stream.
	HTTPAddr string

	// Sampling cadences (spec §7). All three are independently configurable.
	FastInterval   time.Duration // wait-event sampling
	SlowInterval   time.Duration // counter snapshot == frame cadence
	SparseInterval time.Duration // pg_buffercache dirty level

	// Alpha is the EWMA smoothing factor for node fractions (spec §11).
	Alpha float64

	// MinSamples gates a node from showing confident red (spec §10).
	MinSamples int

	// NormPeak optionally fixes the throughput normalization reference per edge.
	// Zero means use a rolling max instead (spec §19 decision: rolling max).
	NormPeakRecordsPerSec float64
	NormPeakBlocksPerSec  float64
	NormPeakBytesPerSec   float64
	NormPeakSyncsPerSec   float64

	// EnableBuffercache toggles the expensive sparse pg_buffercache scan. When
	// false (or the extension is absent) the dirty level is derived/estimated.
	EnableBuffercache bool

	// StatementTimeout caps any monitoring query (spec §15).
	StatementTimeout time.Duration

	// AppName identifies the monitoring connection (spec §15).
	AppName string
}

// Load reads configuration from the environment.
func Load() Config {
	return Config{
		DSN:                   getenv("PG_DSN", ""),
		HTTPAddr:              getenv("HTTP_ADDR", ":8080"),
		FastInterval:          getdur("FAST_INTERVAL", 150*time.Millisecond),
		SlowInterval:          getdur("SLOW_INTERVAL", 3*time.Second),
		SparseInterval:        getdur("SPARSE_INTERVAL", 20*time.Second),
		Alpha:                 getfloat("EWMA_ALPHA", 0.2),
		MinSamples:            getint("MIN_SAMPLES", 20),
		NormPeakRecordsPerSec: getfloat("NORM_PEAK_RECORDS", 0),
		NormPeakBlocksPerSec:  getfloat("NORM_PEAK_BLOCKS", 0),
		NormPeakBytesPerSec:   getfloat("NORM_PEAK_BYTES", 0),
		NormPeakSyncsPerSec:   getfloat("NORM_PEAK_SYNCS", 0),
		EnableBuffercache:     getbool("ENABLE_BUFFERCACHE", true),
		StatementTimeout:      getdur("STATEMENT_TIMEOUT", 2*time.Second),
		AppName:               getenv("APP_NAME", "wal-buffer-visualizer"),
	}
}

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func getdur(k string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(k); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getfloat(k string, def float64) float64 {
	if v, ok := os.LookupEnv(k); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getint(k string, def int) int {
	if v, ok := os.LookupEnv(k); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getbool(k string, def bool) bool {
	if v, ok := os.LookupEnv(k); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
