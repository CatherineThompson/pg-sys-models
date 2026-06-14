-- Provisioning for the integration target (spec §15).
-- Read-only monitoring role granted pg_monitor (sees other backends' wait
-- events and full WAL/I/O stats), plus pg_buffercache for the dirty level.

CREATE EXTENSION IF NOT EXISTS pg_buffercache;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'visualizer') THEN
    CREATE ROLE visualizer LOGIN PASSWORD 'visualizer';
  END IF;
END $$;

GRANT pg_monitor TO visualizer;
GRANT CONNECT ON DATABASE appdb TO visualizer;
GRANT SELECT ON pg_buffercache TO visualizer;

-- A table to drive load with pgbench-style writes during testing.
CREATE TABLE IF NOT EXISTS churn (
  id   bigint PRIMARY KEY,
  blob jsonb
);
