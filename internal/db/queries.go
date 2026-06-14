package db

// SQL used by the poller. All statements are read-only (spec §6, §15).

// fastWaitSQL is the fast-loop wait snapshot (spec §6). Rows with a NULL
// wait_event represent active-on-CPU backends; they are still active and form
// part of the denominator (spec §9.2), so they are intentionally included.
const fastWaitSQL = `
SELECT wait_event_type, wait_event, count(*) AS n
FROM pg_stat_activity
WHERE backend_type = 'client backend' AND state = 'active'
GROUP BY 1, 2;`

// walSQL reads cumulative WAL counters (spec §6).
const walSQL = `
SELECT wal_bytes, wal_records, wal_sync, wal_sync_time, stats_reset
FROM pg_stat_wal;`

// ioSQL reads I/O counters broken down by writer (spec §6). Relation writes are
// the ones that flush to data files; the Go side buckets the rows (spec §5).
const ioSQL = `
SELECT backend_type, context, COALESCE(object, '') AS object,
       COALESCE(reads, 0)  AS reads,
       COALESCE(writes, 0) AS writes,
       stats_reset
FROM pg_stat_io;`

// dbSQL reads the version-stable cache-hit + read counters (spec §6).
const dbSQL = `
SELECT blks_read, blks_hit, stats_reset
FROM pg_stat_database
WHERE datname = current_database();`

// dirtySQL is the expensive sparse-loop dirty-buffer level (spec §6). It scans
// all of shared buffers, so it runs only on the sparse cadence.
const dirtySQL = `
SELECT count(*) FILTER (WHERE isdirty) AS dirty, count(*) AS total
FROM pg_buffercache;`

// Capability probes (spec §6 version note, §16).
const versionSQL = `SELECT current_setting('server_version_num')::int;`
const hasStatIOSQL = `SELECT to_regclass('pg_catalog.pg_stat_io') IS NOT NULL;`
const hasBuffercacheSQL = `SELECT to_regclass('public.pg_buffercache') IS NOT NULL;`
