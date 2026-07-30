-- MHC (Mahachai) source registry. Run AFTER scripts/mhc-schema.sql and after both
-- dumps are loaded (the tables must hold their rows), and after the app has started
-- once so factories/source_tables/source_metrics exist from migrate.RunAll().
--
--   Get-Content scripts\mhc-registry.sql -Raw | docker compose exec -T db psql -U iot_user -d iot_dashboard
--
-- Re-runnable: every statement is ON CONFLICT DO NOTHING / DO UPDATE, and the two
-- renames are guarded. Everything about MHC's shape and semantics lives in these
-- rows, not in code — no Go file knows this plant exists.

-- ─── Rename the landing tables ───────────────────────────────────────────────
-- The dumps INSERT into public.ais / public.iqf_recoards, so the tables were
-- created under those names and loaded byte-for-byte. Rename now that they are full.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='public' AND tablename='ais')
     AND NOT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='public' AND tablename='mhc_ais') THEN
    ALTER TABLE ais RENAME TO mhc_ais;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='public' AND tablename='iqf_recoards')
     AND NOT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='public' AND tablename='mhc_iqf') THEN
    ALTER TABLE iqf_recoards RENAME TO mhc_iqf;
  END IF;
END $$;

-- ─── Indexes — built after the load, matching ts_expr EXACTLY ────────────────
-- mhc_ais MUST be a functional index: the reader filters on to_timestamp(time_stamp),
-- and an index on the bare bigint would never be used.
CREATE INDEX IF NOT EXISTS mhc_ais_ts     ON mhc_ais (to_timestamp(time_stamp));
CREATE INDEX IF NOT EXISTS mhc_ais_stream ON mhc_ais (machine_name, area, sku_id);
CREATE INDEX IF NOT EXISTS mhc_iqf_ts     ON mhc_iqf (created_at_ts);

-- ─── Plant ───────────────────────────────────────────────────────────────────
-- Second factory under the existing CPF organization, so one login sees both
-- plants. The UUID is arbitrary but fixed: unlike NJ5 (whose dumps carry
-- plant_id 4046) the MHC dumps have no plant column at all.
-- slug 'mhc' is what makes the page /ask/mhc — no frontend change needed.
INSERT INTO factories (id, organization_id, name, location, timezone, slug)
VALUES ('00000000-0000-0000-0001-000000009001',
        '00000000-0000-0000-0000-000000004046',
        'Mahachai', 'Mahachai, Samut Sakhon', 'Asia/Bangkok', 'mhc')
ON CONFLICT (id) DO UPDATE SET slug = EXCLUDED.slug;

INSERT INTO production_lines (id, factory_id, name, code, status)
VALUES ('00000000-0000-0000-0002-000000009001',
        '00000000-0000-0000-0001-000000009001', 'MHC Main', 'MHC', 'active')
ON CONFLICT (id) DO NOTHING;

-- ─── Source tables ───────────────────────────────────────────────────────────
-- mhc_ais  ts_expr: to_timestamp(time_stamp), NOT created_at_ts — the latter is the
--          batch-insert time (344,925 distinct values over 791,049 rows, several
--          machines sharing one), which would stack readings onto one instant.
-- mhc_iqf  machine_expr is the LITERAL 'IQF', not the table's machine_name ('IQF1'),
--          so the sensors resolve to the SAME machines row as the 'IQF' production
--          counter in mhc_ais: "temperature vs output for IQF" is two series of one
--          machine instead of a cross-table join recipe.
--          ⚠️ If the plant confirms IQF1 is a different machine, change this to
--          machine_name and re-import (series already written keep the old name).
INSERT INTO source_tables (factory_id, table_name, shape, ts_expr, machine_expr, label_exprs, overlap_seconds)
VALUES
  ('00000000-0000-0000-0001-000000009001', 'mhc_ais', 'wide_counter',
   'to_timestamp(time_stamp)', 'machine_name',
   '{"area":"area","sku":"sku_id"}'::jsonb, 300),
  ('00000000-0000-0000-0001-000000009001', 'mhc_iqf', 'wide_sensor',
   'created_at_ts', '''IQF''', '{}'::jsonb, 300)
ON CONFLICT (table_name) DO UPDATE
  SET ts_expr = EXCLUDED.ts_expr,
      machine_expr = EXCLUDED.machine_expr,
      label_exprs = EXCLUDED.label_exprs,
      overlap_seconds = EXCLUDED.overlap_seconds;

-- ─── Metrics ─────────────────────────────────────────────────────────────────
-- Production. count_ng is NOT registered: it is 0 on all 791,049 rows, and leaving
-- it out is how "there is no reject data here" is expressed — it never reaches the
-- catalog, so the model cannot ask for it.
INSERT INTO source_metrics (source_table_id, column_name, field_key, kind, unit, llm_note)
SELECT id, 'count_fg', 'produced_count', 'counter', 'pieces',
       'Cumulative piece counter per (machine, area, sku). Output for a period is the delta, never a sum.'
FROM source_tables WHERE table_name = 'mhc_ais'
ON CONFLICT (source_table_id, column_name) DO UPDATE
  SET kind = EXCLUDED.kind, field_key = EXCLUDED.field_key,
      llm_note = EXCLUDED.llm_note, updated_at = NOW();

-- Freezer sensors — all 9 columns registered.
-- Only the first three actually move; the other six hold ONE value across every one
-- of the 258,371 rows (freezing_time 150, speed_belt 480, fan01-03 = 1, fan04 = 0).
-- They are registered anyway, by decision, so that the day a value starts moving the
-- history is already there instead of needing a re-import. Their llm_note says
-- "constant" out loud — without that the model hunts for a trend in a flat line.
-- (MHC labels freezing_time/speed_belt the opposite way round from NJ5, where
-- freezing_time is 480 and speed_belts is 150. Worth confirming with the plant.)
--
-- No sentinel column anywhere here: unlike NJ5's 9999, MHC sends NULL when a reading
-- is missing (9,133 rows), and the reader skips NULLs on its own. fail_network still
-- has a value on those rows, so dropouts stay countable through the outage.
--
-- fan01-04 are kind='state', not 'gauge': they are on/off flags that hold until they
-- change, so the compiler should take the last value in a bucket. Averaging them
-- would answer "the mean of a running fan", which reads like a number but is not one.
INSERT INTO source_metrics (source_table_id, column_name, value_expr, field_key,
                            kind, unit, sentinel, valid_min, valid_max, llm_note)
SELECT st.id, m.* FROM source_tables st, LATERAL (VALUES
  ('room_temp',     NULL::text,          'room_temp',     'gauge', '°C', NULL::float, -60.0, 40.0,
   'Chamber temperature. Normal -26..-32; positive readings up to ~+32 are defrost or door-open periods, not sensor faults.'),
  ('coil_temp',     NULL,                'coil_temp',     'gauge', '°C', NULL,        -60.0, 40.0,
   'Evaporator coil temperature, the coldest point of the circuit. Normal -29..-33; brief positive readings are defrost cycles.'),
  ('fail_network',  'fail_network::int',  'network_drop',  'event', NULL, NULL,          0.0,  1.0,
   'One per reading where the machine lost its network connection; roughly 37% of readings at this plant, so a high count is normal here. Sum it to count dropouts.'),
  -- constant across the whole dump — setpoints and config flags, not measurements
  ('freezing_time', NULL,                'freezing_time', 'gauge', 's',  NULL,          0.0, 9000.0,
   'Dwell time setpoint in seconds. Constant at 150 across the entire 2026-04..07 dump; treat as a setpoint, not a measurement. Note this plant''s 150/480 pair is the reverse of NJ5''s.'),
  ('speed_belt',    NULL,                'belt_speed',    'gauge', NULL, NULL,          0.0, 1000.0,
   'Conveyor belt speed. Constant at 480 across the entire dump; treat as a setpoint, not a measurement.'),
  ('fan01',         NULL,                'fan1_status',   'state', NULL, NULL,          0.0,    1.0,
   'Fan 1 on/off flag (1 = running). Constant at 1 across the entire dump.'),
  ('fan02',         NULL,                'fan2_status',   'state', NULL, NULL,          0.0,    1.0,
   'Fan 2 on/off flag (1 = running). Constant at 1 across the entire dump.'),
  ('fan03',         NULL,                'fan3_status',   'state', NULL, NULL,          0.0,    1.0,
   'Fan 3 on/off flag (1 = running). Constant at 1 across the entire dump.'),
  ('fan04',         NULL,                'fan4_status',   'state', NULL, NULL,          0.0,    1.0,
   'Fan 4 on/off flag (1 = running). Constant at 0 across the entire dump — this fan never ran in the imported period.')
) AS m(column_name, value_expr, field_key, kind, unit, sentinel, valid_min, valid_max, llm_note)
WHERE st.table_name = 'mhc_iqf'
ON CONFLICT (source_table_id, column_name) DO UPDATE
  SET kind = EXCLUDED.kind, field_key = EXCLUDED.field_key, unit = EXCLUDED.unit,
      value_expr = EXCLUDED.value_expr, sentinel = EXCLUDED.sentinel,
      valid_min = EXCLUDED.valid_min, valid_max = EXCLUDED.valid_max,
      llm_note = EXCLUDED.llm_note, updated_at = NOW();

-- ─── Worker state ────────────────────────────────────────────────────────────
-- Optional: watermark() inserts a missing row itself. Here for visibility.
INSERT INTO source_state (source_table_id)
SELECT id FROM source_tables WHERE table_name IN ('mhc_ais', 'mhc_iqf')
ON CONFLICT (source_table_id) DO NOTHING;

-- Expect mhc_ais → 1 metric, mhc_iqf → 9.
SELECT st.table_name, st.shape, st.ts_expr, st.machine_expr, count(sm.id) AS metrics
FROM source_tables st LEFT JOIN source_metrics sm ON sm.source_table_id = st.id
WHERE st.table_name LIKE 'mhc%'
GROUP BY 1, 2, 3, 4 ORDER BY 1;
