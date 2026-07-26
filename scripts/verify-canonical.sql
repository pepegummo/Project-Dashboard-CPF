-- Compare the legacy NJ5 views against the canonical model, per stream per day.
-- Run after cmd/normalize-backfill. This is the gate for retiring v_nj5* later:
-- every difference must be explainable before anything is dropped.
--
-- STATUS: the gate was passed and nj5-retire.sql has run, so sections 3 and 4 now
-- fail with "relation v_nj5_hourly does not exist" — that is the expected state,
-- not a regression. To compare again, rebuild the baseline with nj5-views.sql
-- (the legacy names are kept as *_legacy until the grace period ends).
--
-- series/readings are FORCE RLS on app.factory, so scope the session first —
-- without this every query below returns zero rows (failing closed by design).
SET app.factory = '00000000-0000-0000-0001-000000004046';

\echo '── 1. Series inventory (expect a few hundred, NOT thousands) ─────────────'
SELECT s.field_key, count(*) AS series, min(s.first_seen)::date AS first_seen
FROM series s
GROUP BY 1 ORDER BY 2 DESC;

\echo '── 2. Reading counts and quality mix ────────────────────────────────────'
SELECT s.field_key, r.quality, count(*) AS readings
FROM readings r JOIN series s ON s.id = r.series_id
GROUP BY 1, 2 ORDER BY 1, 2;

\echo '── 3. Production per stream per day: legacy MAX-MIN vs canonical last-first'
-- The canonical number should be >= the legacy one: MAX-MIN loses the piece of a
-- counter that straddles a bucket edge, last_v-first_v does not. A canonical
-- number that is SMALLER, or a stream missing on one side, is a real problem.
WITH legacy AS (
  SELECT date_trunc('day', ts) AS day, machine_name, area, sku_id,
         SUM(produced) AS produced
  FROM v_nj5_hourly
  GROUP BY 1, 2, 3, 4
), canon AS (
  SELECT date_trunc('day', h.bucket) AS day,
         m.name                      AS machine_name,
         s.labels->>'area'           AS area,
         s.labels->>'sku'            AS sku_id,
         SUM(GREATEST(h.last_v - h.first_v, 0)) AS produced
  FROM readings_1h h
  JOIN series s   ON s.id = h.series_id
  JOIN machines m ON m.id = s.machine_id
  WHERE s.field_key = 'produced_count'
  GROUP BY 1, 2, 3, 4
)
SELECT COALESCE(l.day, c.day)::date AS day,
       COALESCE(l.machine_name, c.machine_name) AS machine,
       COALESCE(l.area, c.area)                 AS area,
       COALESCE(l.sku_id, c.sku_id)             AS sku,
       l.produced AS legacy,
       c.produced AS canonical,
       c.produced - l.produced AS diff
FROM legacy l
FULL OUTER JOIN canon c
  ON  c.day = l.day AND c.machine_name = l.machine_name
  AND c.area = l.area AND c.sku_id IS NOT DISTINCT FROM l.sku_id
WHERE l.produced IS NULL
   OR c.produced IS NULL
   OR c.produced < l.produced                      -- canonical must not undercount
   OR abs(c.produced - l.produced) > l.produced * 0.02   -- or differ by > 2%
ORDER BY abs(COALESCE(c.produced, 0) - COALESCE(l.produced, 0)) DESC
LIMIT 50;

\echo '── 4. Freezer gauges: legacy view vs canonical, per day ─────────────────'
WITH legacy AS (
  SELECT date_trunc('day', ts) AS day, 'IQF2' AS machine,
         avg(evap_temp) AS evap_temp
  FROM v_nj5_iqf2 GROUP BY 1
  UNION ALL
  SELECT date_trunc('day', ts), 'IQF3', avg(evap_temp) FROM v_nj5_iqf3 GROUP BY 1
), canon AS (
  SELECT date_trunc('day', h.bucket) AS day, m.name AS machine,
         SUM(h.sum_v) / NULLIF(SUM(h.n), 0) AS evap_temp   -- weighted, never avg-of-avg
  FROM readings_1h h
  JOIN series s   ON s.id = h.series_id
  JOIN machines m ON m.id = s.machine_id
  WHERE s.field_key = 'evap_temp'
  GROUP BY 1, 2
)
SELECT COALESCE(l.day, c.day)::date AS day,
       COALESCE(l.machine, c.machine) AS machine,
       round(l.evap_temp::numeric, 2) AS legacy,
       round(c.evap_temp::numeric, 2) AS canonical,
       round((c.evap_temp - l.evap_temp)::numeric, 3) AS diff
FROM legacy l
FULL OUTER JOIN canon c ON c.day = l.day AND c.machine = l.machine
WHERE l.evap_temp IS NULL OR c.evap_temp IS NULL
   OR abs(c.evap_temp - l.evap_temp) > 0.5
ORDER BY 1, 2
LIMIT 50;

\echo '── 5. Ingest state (last_error must be NULL on every source) ───────────'
SELECT st.table_name, ss.last_watermark, ss.rows_ingested, ss.last_error
FROM source_tables st JOIN source_state ss ON ss.source_table_id = st.id
ORDER BY 1;
