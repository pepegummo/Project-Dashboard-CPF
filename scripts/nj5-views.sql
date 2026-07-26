-- LEGACY — superseded by the canonical model (series/readings + the source registry).
-- The app no longer queries these views: the /nj5 page asks against dataset
-- "canonical", and nothing in allowedViews mentions v_nj5* any more.
--
-- Kept for ONE purpose: scripts/verify-canonical.sql compares the numbers these views
-- produce against the canonical rollups, which is the gate for retiring them
-- (scripts/nj5-retire.sql). Run this only to rebuild that comparison baseline.
-- The INDEX statements at the top are still worth having — the normalizer reads the
-- landing tables through the same expressions.
--
-- NJ5 dataset — indexes, views, and hourly rollup (run AFTER loading the dumps).
-- Every view exposes `ts timestamptz` so the Ask-Data pipeline's existing
-- time_bucket('%BUCKET%', ts) / $1..$2 window machinery works unchanged.

-- Indexes on our own imported tables (built after load so they don't slow ingest).
-- The expression index matches the views' `WHERE ts >= $1` after inlining, keeping
-- narrow (day/week) queries off a full seq scan of ~21.6M rows.
CREATE INDEX IF NOT EXISTS nj5_machines_ts     ON nj5_machines (to_timestamp(time_stamp));
CREATE INDEX IF NOT EXISTS nj5_machines_stream ON nj5_machines (machine_name, area, sku_id);
-- IMPORTANT: iqf created_at (epoch seconds) is populated for only ~3% of rows (the 2025
-- batch); created_at_ts (timestamptz, the ingest time) is on EVERY row and runs to 2026.
-- Key the freezer views on created_at_ts, and index that column directly.
CREATE INDEX IF NOT EXISTS nj5_iqf2_ts         ON nj5_iqf2 (created_at_ts);
CREATE INDEX IF NOT EXISTS nj5_iqf3_ts         ON nj5_iqf3 (created_at_ts);

-- Raw production. count_fg is a CUMULATIVE counter per (machine_name, area, sku_id):
-- production over a period is MAX(count_fg) - MIN(count_fg), never SUM.
CREATE OR REPLACE VIEW v_nj5 AS
SELECT to_timestamp(time_stamp) AS ts,
       machine_name,
       area,
       sku_id,
       count_fg
FROM nj5_machines;

-- Freezer sensors (instantaneous gauges -> AVG/MIN/MAX, never MAX-MIN).
-- Time column is created_at_ts (timestamptz, present on every row); created_at (epoch)
-- and time_stamp (epoch ms) are both NULL for most rows so they are NOT used.
-- rail_temp is NULL for every row and is dropped here.
CREATE OR REPLACE VIEW v_nj5_iqf2 AS
SELECT created_at_ts AS ts,
       'Line2'::text AS line,
       evap_temp,
       room_temp,
       air_b_fan1a,
       air_b_fan2a,
       speed_belts,
       freezing_time,
       fail_network
FROM nj5_iqf2;

CREATE OR REPLACE VIEW v_nj5_iqf3 AS
SELECT created_at_ts AS ts,
       'Line3'::text AS line,
       evap_temp,
       room_temp,
       air_b_fan1a,
       air_b_fan2a,
       speed_belts,
       freezing_time,
       fail_network
FROM nj5_iqf3;

-- Hourly rollup for wide (>= ~2 day) production questions: 21.6M rows -> ~10^5.
-- `produced` is the already-computed per-hour delta, so callers SUM it (never MAX-MIN).
DROP MATERIALIZED VIEW IF EXISTS v_nj5_hourly;
CREATE MATERIALIZED VIEW v_nj5_hourly AS
SELECT time_bucket('1 hour', to_timestamp(time_stamp)) AS ts,
       machine_name,
       area,
       sku_id,
       MAX(count_fg) - MIN(count_fg) AS produced,
       COUNT(*)                      AS readings
FROM nj5_machines
GROUP BY 1, 2, 3, 4;

CREATE UNIQUE INDEX v_nj5_hourly_pk ON v_nj5_hourly (ts, machine_name, area, sku_id);

-- Static dump -> refresh once. (For a live feed, schedule REFRESH ... CONCURRENTLY hourly.)
REFRESH MATERIALIZED VIEW v_nj5_hourly;
