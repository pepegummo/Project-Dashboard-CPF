-- NJ5 source registry — describes the three upstream tables to the normalizer.
-- Run AFTER nj5-schema.sql + the dumps (the tables must exist) and after the app
-- has started once (so factories/source_tables exist from migrate.go).
--
-- Everything about NJ5's shape and semantics lives in these rows, not in code:
-- registering a fourth freezer later is one source_tables row plus its metrics.
-- Re-runnable — every statement is ON CONFLICT DO NOTHING / UPDATE.

-- ─── Plant ───────────────────────────────────────────────────────────────────
-- Fixed UUIDs (4046 = the plant_id the upstream system sends) so scripts, tests
-- and the frontend can reference this factory without a lookup.
INSERT INTO organizations (id, name, slug, plan)
VALUES ('00000000-0000-0000-0000-000000004046', 'CPF Nongjok', 'cpf-nongjok', 'enterprise')
ON CONFLICT (id) DO NOTHING;

INSERT INTO factories (id, organization_id, name, location, timezone)
VALUES ('00000000-0000-0000-0001-000000004046', '00000000-0000-0000-0000-000000004046',
        'Nongjok5', 'Nongjok, Bangkok', 'Asia/Bangkok')
ON CONFLICT (id) DO NOTHING;

INSERT INTO production_lines (id, factory_id, name, code, status)
VALUES ('00000000-0000-0000-0002-000000004046', '00000000-0000-0000-0001-000000004046',
        'NJ5 Main', 'NJ5', 'active')
ON CONFLICT (id) DO NOTHING;

-- ─── Source tables ───────────────────────────────────────────────────────────
-- machine_expr for the freezer tables is the LITERAL 'IQF2' / 'IQF3', not the
-- table's own machine_name ('IQF_2'/'IQF_3'). That is deliberate: it resolves to
-- the SAME machine row as the production counters, so "temperature vs output for
-- IQF2" is two series of one machine instead of a cross-table join recipe.
INSERT INTO source_tables (factory_id, table_name, shape, ts_expr, machine_expr, label_exprs, overlap_seconds)
VALUES
  ('00000000-0000-0000-0001-000000004046', 'nj5_machines', 'wide_counter',
   'to_timestamp(time_stamp)', 'machine_name', '{"area":"area","sku":"sku_id"}'::jsonb, 300),
  ('00000000-0000-0000-0001-000000004046', 'nj5_iqf2', 'wide_sensor',
   'created_at_ts', '''IQF2''', '{}'::jsonb, 300),
  ('00000000-0000-0000-0001-000000004046', 'nj5_iqf3', 'wide_sensor',
   'created_at_ts', '''IQF3''', '{}'::jsonb, 300)
ON CONFLICT (table_name) DO UPDATE
  SET ts_expr = EXCLUDED.ts_expr,
      machine_expr = EXCLUDED.machine_expr,
      label_exprs = EXCLUDED.label_exprs,
      overlap_seconds = EXCLUDED.overlap_seconds;

-- ─── Metrics ─────────────────────────────────────────────────────────────────
-- Production. count_ng is NOT registered: it is 0 on every row of the dump, so
-- leaving it out is how "there is no reject data" is expressed — the metric never
-- reaches the catalog, so the model cannot ask for it.
INSERT INTO source_metrics (source_table_id, column_name, field_key, kind, unit, llm_note)
SELECT id, 'count_fg', 'produced_count', 'counter', 'pieces',
       'Cumulative piece counter per (machine, area, sku). Output for a period is the delta.'
FROM source_tables WHERE table_name = 'nj5_machines'
ON CONFLICT (source_table_id, column_name) DO UPDATE
  SET kind = EXCLUDED.kind, field_key = EXCLUDED.field_key,
      llm_note = EXCLUDED.llm_note, updated_at = NOW();

-- Freezer sensors — identical metric set for both tables.
-- rail_temp is NOT registered (NULL on every row). freezing_time carries its
-- 9999 sentinel so the normalizer flags those readings instead of the prompt
-- having to remember "AND freezing_time < 9000".
INSERT INTO source_metrics (source_table_id, column_name, value_expr, field_key, kind, unit, sentinel, valid_min, valid_max, llm_note)
SELECT st.id, m.column_name, m.value_expr, m.field_key, m.kind, m.unit, m.sentinel, m.valid_min, m.valid_max, m.llm_note
FROM source_tables st
CROSS JOIN (VALUES
    ('evap_temp',     NULL::text,          'evap_temp',     'gauge', '°C',    NULL::float, -60.0, 40.0,
     'Evaporator temperature. Normal -34..-41; brief POSITIVE readings up to ~+28 are defrost cycles, not errors.'),
    ('room_temp',     NULL,                'room_temp',     'gauge', '°C',    NULL,        -60.0, 40.0,
     'Chamber temperature.'),
    ('air_b_fan1a',   NULL,                'fan1_speed',    'gauge', NULL,    NULL,        0.0,   2000.0,
     'Fan 1 reading; 0 means that fan is idle.'),
    ('air_b_fan2a',   NULL,                'fan2_speed',    'gauge', NULL,    NULL,        0.0,   2000.0,
     'Fan 2 reading; frequently 0.'),
    ('speed_belts',   NULL,                'belt_speed',    'gauge', NULL,    NULL,        0.0,   1000.0,
     'Conveyor belt speed, normally around 150.'),
    ('freezing_time', NULL,                'freezing_time', 'gauge', 's',     9999.0,      0.0,   9000.0,
     'Dwell time in seconds. 9999 is a sentinel for idle/no reading and is excluded automatically.'),
    -- boolean upstream: value_expr casts it so the reading is 1 per dropout
    ('fail_network',  'fail_network::int', 'network_drop',   'event', NULL,    NULL,        0.0,   1.0,
     'One per reading where the machine lost its network connection. Sum it to count dropouts.')
  ) AS m(column_name, value_expr, field_key, kind, unit, sentinel, valid_min, valid_max, llm_note)
WHERE st.table_name IN ('nj5_iqf2', 'nj5_iqf3')
ON CONFLICT (source_table_id, column_name) DO UPDATE
  SET kind = EXCLUDED.kind, field_key = EXCLUDED.field_key, unit = EXCLUDED.unit,
      value_expr = EXCLUDED.value_expr,
      sentinel = EXCLUDED.sentinel, valid_min = EXCLUDED.valid_min,
      valid_max = EXCLUDED.valid_max, llm_note = EXCLUDED.llm_note, updated_at = NOW();

-- ─── Worker state ────────────────────────────────────────────────────────────
INSERT INTO source_state (source_table_id)
SELECT id FROM source_tables WHERE table_name IN ('nj5_machines', 'nj5_iqf2', 'nj5_iqf3')
ON CONFLICT (source_table_id) DO NOTHING;

-- Expect 15 metric rows: 1 counter + 7 × 2 freezers.
SELECT st.table_name, st.shape, count(sm.id) AS metrics
FROM source_tables st LEFT JOIN source_metrics sm ON sm.source_table_id = st.id
GROUP BY 1, 2 ORDER BY 1;
