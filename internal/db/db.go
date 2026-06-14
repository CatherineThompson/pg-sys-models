// Package db wraps the read-only monitoring queries (spec §6) behind a small
// API, probes server capabilities at startup, and degrades gracefully when a
// source is unavailable (spec §16).
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MinServerVersionNum is the minimum supported PostgreSQL version: 16, required
// for pg_stat_io (spec §6 version note).
const MinServerVersionNum = 160000

// Capabilities records which optional sources are usable on the target server.
type Capabilities struct {
	ServerVersionNum int
	HasStatIO        bool
	HasBuffercache   bool
}

// WaitRow is one grouped wait-state row from the fast loop.
type WaitRow struct {
	Type  string
	Event string
	N     int
}

// CounterSnapshot is the raw result of the slow-loop counter reads. The poller
// converts these into rates; this layer only fetches and lightly normalizes.
type CounterSnapshot struct {
	WalBytes    float64
	WalRecords  float64
	WalSync     float64
	WalSyncTime float64
	WalReset    string

	BlksRead float64
	BlksHit  float64
	DBReset  string

	WritesCheckpointer float64
	WritesBgwriter     float64
	WritesBackend      float64
	IOReset            string
}

// DB is a pooled connection to the monitoring target.
type DB struct {
	pool    *pgxpool.Pool
	caps    Capabilities
	timeout time.Duration
}

// Open connects, applies the monitoring session settings (spec §15), and probes
// capabilities. A non-nil error means live mode is unavailable; the server can
// still serve the frontend and teaching mode.
func Open(ctx context.Context, dsn, appName string, stmtTimeout time.Duration) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	// Identify the connection and bound every query so monitoring can never run
	// away (spec §15).
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", stmtTimeout.Milliseconds())

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	d := &DB{pool: pool, timeout: stmtTimeout}
	if err := d.probe(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if d.caps.ServerVersionNum < MinServerVersionNum {
		pool.Close()
		return nil, fmt.Errorf("server version %d < minimum %d (need pg_stat_io)",
			d.caps.ServerVersionNum, MinServerVersionNum)
	}
	return d, nil
}

func (d *DB) probe(ctx context.Context) error {
	row := d.pool.QueryRow(ctx, versionSQL)
	if err := row.Scan(&d.caps.ServerVersionNum); err != nil {
		return fmt.Errorf("probe version: %w", err)
	}
	// Missing optional sources degrade features, they are not fatal (spec §16).
	_ = d.pool.QueryRow(ctx, hasStatIOSQL).Scan(&d.caps.HasStatIO)
	_ = d.pool.QueryRow(ctx, hasBuffercacheSQL).Scan(&d.caps.HasBuffercache)
	return nil
}

// Caps returns the probed server capabilities.
func (d *DB) Caps() Capabilities { return d.caps }

// Close releases the pool.
func (d *DB) Close() {
	if d.pool != nil {
		d.pool.Close()
	}
}

// SampleWaits runs the fast-loop snapshot (spec §6). It returns the grouped
// rows and the total count of active client backends (the §9.2 denominator),
// which is the sum over all rows including NULL-wait (on-CPU) ones.
func (d *DB) SampleWaits(ctx context.Context) (rows []WaitRow, totalActive int, err error) {
	r, err := d.pool.Query(ctx, fastWaitSQL)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	for r.Next() {
		var wt, we *string
		var n int
		if err := r.Scan(&wt, &we, &n); err != nil {
			return nil, 0, err
		}
		totalActive += n
		typ, ev := "", ""
		if wt != nil {
			typ = *wt
		}
		if we != nil {
			ev = *we
		}
		rows = append(rows, WaitRow{Type: typ, Event: ev, N: n})
	}
	return rows, totalActive, r.Err()
}

// ReadCounters runs the slow-loop counter reads (spec §6) and folds the I/O rows
// into the by-writer write buckets (spec §5). If pg_stat_io is unavailable the
// write split is left zero and the caller marks eB unavailable (spec §16).
func (d *DB) ReadCounters(ctx context.Context) (CounterSnapshot, error) {
	var s CounterSnapshot

	if err := d.pool.QueryRow(ctx, walSQL).Scan(
		&s.WalBytes, &s.WalRecords, &s.WalSync, &s.WalSyncTime, resetDest(&s.WalReset),
	); err != nil {
		return s, fmt.Errorf("pg_stat_wal: %w", err)
	}

	if err := d.pool.QueryRow(ctx, dbSQL).Scan(
		&s.BlksRead, &s.BlksHit, resetDest(&s.DBReset),
	); err != nil {
		return s, fmt.Errorf("pg_stat_database: %w", err)
	}

	if d.caps.HasStatIO {
		if err := d.readIO(ctx, &s); err != nil {
			return s, fmt.Errorf("pg_stat_io: %w", err)
		}
	}
	return s, nil
}

func (d *DB) readIO(ctx context.Context, s *CounterSnapshot) error {
	r, err := d.pool.Query(ctx, ioSQL)
	if err != nil {
		return err
	}
	defer r.Close()
	for r.Next() {
		var backendType, ctxName, object string
		var reads, writes float64
		var reset *time.Time
		if err := r.Scan(&backendType, &ctxName, &object, &reads, &writes, &reset); err != nil {
			return err
		}
		if reset != nil && reset.Format(time.RFC3339Nano) > s.IOReset {
			s.IOReset = reset.Format(time.RFC3339Nano)
		}
		// Only relation writes flush to data files (spec §5).
		if object != "relation" {
			continue
		}
		switch backendType {
		case "checkpointer":
			s.WritesCheckpointer += writes
		case "background writer":
			s.WritesBgwriter += writes
		case "client backend":
			// Backend writes in the normal context are forced evictions (spec §9.3).
			if ctxName == "normal" {
				s.WritesBackend += writes
			}
		}
	}
	return r.Err()
}

// ReadDirtyLevel runs the expensive sparse-loop dirty-buffer scan (spec §6).
// Returns ok=false when pg_buffercache is unavailable so the caller can fall
// back to an estimate (spec §16).
func (d *DB) ReadDirtyLevel(ctx context.Context) (fraction float64, ok bool, err error) {
	if !d.caps.HasBuffercache {
		return 0, false, nil
	}
	var dirty, total float64
	if err := d.pool.QueryRow(ctx, dirtySQL).Scan(&dirty, &total); err != nil {
		return 0, false, err
	}
	if total <= 0 {
		return 0, true, nil
	}
	return dirty / total, true, nil
}

// resetDest scans a possibly-NULL stats_reset timestamp into a stable string
// fingerprint used to detect resets between snapshots (spec §8 guard 2).
func resetDest(dst *string) any { return &resetScan{dst} }

type resetScan struct{ dst *string }

func (r *resetScan) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*r.dst = ""
	case time.Time:
		*r.dst = v.Format(time.RFC3339Nano)
	case string:
		*r.dst = v
	default:
		*r.dst = fmt.Sprintf("%v", v)
	}
	return nil
}
