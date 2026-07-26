-- Which saved Ask-Data charts still depend on the legacy NJ5 views.
--
-- A board chart stores the SQL text it was created with (ai_board_charts.sql), so a
-- chart saved before the canonical model keeps querying v_nj5* forever — it does not
-- follow the pipeline forward. Canonical charts instead store a spec, which is
-- recompiled on open and therefore survives the relations changing underneath.
--
-- This is a report, not a migration: turning arbitrary SQL back into intent is
-- guesswork, and a silently mis-converted chart is worse than one that asks to be
-- re-asked. Section 3 gives the wording to re-ask each one.

\echo '── 1. Totals ─────────────────────────────────────────────────────────────'
SELECT count(*) FILTER (WHERE spec IS NOT NULL)                    AS canonical_ok,
       count(*) FILTER (WHERE spec IS NULL AND sql ILIKE '%v_nj5%') AS needs_reasking,
       count(*) FILTER (WHERE spec IS NULL AND sql NOT ILIKE '%v_nj5%') AS other_legacy_sql
FROM ai_board_charts;

\echo '── 2. Which views each stale chart depends on ────────────────────────────'
SELECT b.name AS board, ch.question,
       CASE WHEN ch.sql ILIKE '%v_nj5_hourly%' THEN 'v_nj5_hourly ' ELSE '' END ||
       CASE WHEN ch.sql ILIKE '%v_nj5_iqf%'    THEN 'v_nj5_iqf* '   ELSE '' END ||
       CASE WHEN ch.sql ILIKE '%v_nj5 %' OR ch.sql ILIKE '%v_nj5)%' THEN 'v_nj5 ' ELSE '' END AS depends_on,
       ch.created_at::date AS saved
FROM ai_board_charts ch
JOIN ai_boards b ON b.id = ch.board_id
WHERE ch.spec IS NULL AND ch.sql ILIKE '%v_nj5%'
ORDER BY b.name, ch."order";

\echo '── 3. Re-ask list (paste each question into /nj5 and save it again) ──────'
SELECT DISTINCT ch.question
FROM ai_board_charts ch
WHERE ch.spec IS NULL AND ch.sql ILIKE '%v_nj5%'
ORDER BY 1;

-- Optional: mark them so the UI can badge a chart as legacy instead of failing blind.
-- UPDATE ai_board_charts SET question = question || ' [legacy — re-ask to refresh]'
-- WHERE spec IS NULL AND sql ILIKE '%v_nj5%' AND question NOT LIKE '%[legacy%';
