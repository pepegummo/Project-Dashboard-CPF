-- Retire the legacy NJ5 views, in the order that keeps the door open.
--
-- WHY retire at all, rather than leave them alongside the canonical model:
--   * v_nj5_hourly computes output as MAX(count_fg)-MIN(count_fg); the canonical
--     rollup uses last_v-first_v. They give DIFFERENT numbers (the delta that
--     straddles a bucket edge is lost by MAX-MIN), so keeping both means the same
--     question has two answers depending on which path a caller took.
--   * v_nj5_hourly is a plain materialized view: it needs a manual REFRESH. Against a
--     live feed it silently serves stale numbers, with nothing to indicate it.
--   * every relation left in the allowlist is one more thing a generated query can
--     reach and one more branch the prompt has to explain.
--
-- The landing tables (nj5_machines, nj5_iqf2, nj5_iqf3) are NOT dropped, ever: they
-- are the record of what upstream actually sent, and the only way to rebuild the
-- canonical model if the normalizer ever turns out to have been wrong.

-- ── Step 1: verify the canonical model agrees BEFORE removing the comparison ──
--   psql -f scripts/verify-canonical.sql
-- Every difference it reports must be explainable. Sections 3 and 4 need the legacy
-- views, so this is the last moment they are useful.

-- ── Step 2: audit saved boards ───────────────────────────────────────────────
--   psql -f scripts/nj5-board-audit.sql
-- Charts saved from the legacy views store SQL text that will stop resolving. The
-- audit lists them with their questions so they can be re-asked (the new answer is
-- saved with a spec, which recompiles and does not rot).

-- ── Step 3: stop refreshing, keep readable ───────────────────────────────────
-- Renaming rather than dropping leaves a few weeks to compare numbers if someone
-- disputes a figure. Nothing in the app references these names any more.
ALTER MATERIALIZED VIEW IF EXISTS v_nj5_hourly RENAME TO v_nj5_hourly_legacy;
ALTER VIEW IF EXISTS v_nj5      RENAME TO v_nj5_legacy;
ALTER VIEW IF EXISTS v_nj5_iqf2 RENAME TO v_nj5_iqf2_legacy;
ALTER VIEW IF EXISTS v_nj5_iqf3 RENAME TO v_nj5_iqf3_legacy;

-- ── Step 4 (after the grace period): drop them ───────────────────────────────
-- Uncomment and run once nobody has asked to compare for a few weeks.
-- DROP MATERIALIZED VIEW IF EXISTS v_nj5_hourly_legacy;
-- DROP VIEW IF EXISTS v_nj5_legacy, v_nj5_iqf2_legacy, v_nj5_iqf3_legacy;

-- telemetry_aggregates is the other duplicate: hand-written rollups of the demo data,
-- superseded by the continuous aggregates. It is left alone until phase 6 folds the
-- demo dataset in — grep the backend for telemetry_aggregates before touching it.

\echo '── Remaining legacy relations (expect only *_legacy names) ───────────────'
SELECT table_name, table_type
FROM information_schema.tables
WHERE table_schema = 'public' AND table_name LIKE 'v_nj5%'
ORDER BY 1;
