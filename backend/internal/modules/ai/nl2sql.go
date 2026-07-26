package ai

// Ask-Data feature: natural language → hardened SQL → rows → LLM-authored ECharts
// option. Separate from the structured-tool chat path. Security model (see the plan
// and migrate.go views): the LLM may only reference org-scoped views; every query
// runs in a read-only tx with statement_timeout and the app.current_org GUC set from
// the caller's JWT, so a generated query can neither write nor escape its org.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"iot-dashboard/internal/database"
	"iot-dashboard/internal/middleware"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// allowedViews are the only relations a generated query may read. The v_nj5* views
// belong to the "nj5" dataset (see schemaFor); they're a shared allowlist here — the
// per-question schema context is what steers the model to its dataset's views.
// ponytail: shared union, not a per-dataset allowlist — least-privilege per dataset is
// the upgrade path if a dataset ever holds data the others' pages shouldn't reach.
// Longest names first: the validator scrubs these out of the query before
// scanning for denied tables, and "readings_1h" contains "readings".
//
// Two datasets, six relations, and this list does NOT grow when a plant, machine,
// metric or upstream table is added — a new source is a registry row. If you are
// about to append a per-plant view here, that is the signal you are on the old path.
// The rollups (readings_1h/_1d) are deliberately NOT here. A continuous aggregate is
// materialized separately, so row-level security on `readings` does not filter it —
// reading one directly would cross factories. Compiled queries reach them safely
// because they always join v_series (which IS filtered) and because the compiler's
// SQL is server-authored and never passes through this validator at all.
var allowedViews = []string{
	// demo/telemetry dataset (mock data, kept for demos)
	"v_telemetry", "v_machines", "v_machine_fields",
	// canonical dataset (real plants) — both enforce RLS through series
	"v_readings", "v_series",
}

// sqlForbidden fast-fails on write/DDL keywords. The read-only tx is the true guard;
// this is defense-in-depth + a clearer error than a Postgres failure.
// ponytail: whole-word scan, not a parser. Add pg_query_go only if we allow richer SQL.
var sqlForbidden = regexp.MustCompile(`(?i)\b(insert|update|delete|drop|alter|create|grant|revoke|truncate|copy|merge|into|call|do)\b`)

// deniedTables blocks any reference to a real base table (or system catalog) — the
// only way to read cross-org data. Allowed v_ views are scrubbed out before scanning
// so "v_machines" doesn't trip the "machines" rule.
// Landing tables (nj5_*, src_*) and the canonical base tables (series, readings)
// are denied too: a generated query reaches them only through the allowed views,
// which is what keeps row-level security and the quality filter in the path.
var deniedTables = regexp.MustCompile(`(?i)\b(telemetry_raw|telemetry_aggregates|machines|machine_fields|nj5_machines|nj5_iqf2|nj5_iqf3|v_nj5[a-z0-9_]*|src_[a-z0-9_]+|series|readings|readings_1[hd]|source_tables|source_metrics|source_state|users|organizations|factories|production_lines|dashboards|dashboard_widgets|alerts|alert_events|ai_boards|ai_board_charts|ai_messages|ai_conversations|ai_preview_drafts|user_organizations|audit_logs|information_schema|pg_[a-z_]+)\b`)

// validateSQL enforces: single SELECT, no write keywords, no base-table access.
// Returns the trimmed, semicolon-free query on success.
func validateSQL(sqlText string) (string, error) {
	s := strings.TrimSpace(sqlText)
	s = strings.TrimSuffix(strings.TrimSpace(s), ";")
	if s == "" {
		return "", errors.New("empty SQL")
	}
	if strings.Contains(s, ";") {
		return "", errors.New("only a single statement is allowed")
	}
	low := strings.ToLower(s)
	if !strings.HasPrefix(low, "select") {
		return "", errors.New("only SELECT queries are allowed")
	}
	if sqlForbidden.MatchString(low) {
		return "", errors.New("query contains a disallowed keyword")
	}
	scrub := low
	for _, v := range allowedViews {
		scrub = strings.ReplaceAll(scrub, v, " ")
	}
	if m := deniedTables.FindString(scrub); m != "" {
		return "", fmt.Errorf("relation %q is not queryable — use the v_ views", m)
	}
	// ponytail: the bucket is ALWAYS the server's call. A model that ignores the
	// prompt and writes a literal interval (a user insisting "give me real 1-minute
	// data over a year" pushes it there) would emit 525k rows and get silently
	// truncated at LIMIT 5000 — a wrong answer that looks fine. Force the token back.
	s = literalBucket.ReplaceAllString(s, "time_bucket('"+bucketToken+"',")
	return s, nil
}

// literalBucket matches time_bucket('<any interval>', — the first arg only.
var literalBucket = regexp.MustCompile(`(?i)time_bucket\(\s*'[^']*'\s*,`)

// ── Zoomable time windows ────────────────────────────────────────────────────
//
// A generated time-series query carries its window as bind params ($1 = from,
// $2 = to) and its bucket as the literal token '%BUCKET%'. That makes the SAME
// stored SQL re-runnable over a narrower range at a finer resolution when the
// user zooms — no second LLM call, no SQL rewriting. Queries with no time filter
// (listings) carry neither, and resolveSQL leaves them untouched.
const bucketToken = "%BUCKET%"

// targetPoints is what autoBucket aims for. maxRows is the hard ceiling, but a
// chart stops being readable — and the JSON payload stops being small — long
// before that, so the bucket is chosen to land well under it.
const targetPoints = 400

// bucketLadder is ordered finest-first: the first interval that keeps the window
// under targetPoints wins, so zooming from a year down to an hour walks it
// automatically (1 day → 1 hour → 5 minutes → 1 minute).
var bucketLadder = []struct {
	step  time.Duration
	label string
}{
	{time.Minute, "1 minute"},
	{5 * time.Minute, "5 minutes"},
	{15 * time.Minute, "15 minutes"},
	{time.Hour, "1 hour"},
	{6 * time.Hour, "6 hours"},
	{24 * time.Hour, "1 day"},
	{7 * 24 * time.Hour, "7 days"},
}

// chartBucket is the resolution the rows were actually aggregated at, for the
// chart's title — empty when the query has no bucket (a listing).
func chartBucket(sqlText string, from, to time.Time) string {
	if !strings.Contains(sqlText, bucketToken) {
		return ""
	}
	return autoBucket(to.Sub(from))
}

func autoBucket(window time.Duration) string {
	for _, b := range bucketLadder {
		if window/b.step <= targetPoints {
			return b.label
		}
	}
	return bucketLadder[len(bucketLadder)-1].label
}

// defaultWindowHours is the lookback for a question that names no time range,
// and the fallback when the model reports a nonsensical one.
const defaultWindowHours = 24

// windowFor turns the model's reported lookback into a concrete [from, to).
// Clamped to (0, 5 years] so a hallucinated window_hours can't scan the whole
// hypertable.
func windowFor(hours float64) (from, to time.Time) {
	if hours <= 0 || hours > 5*365*24 {
		hours = defaultWindowHours
	}
	to = time.Now()
	return to.Add(-time.Duration(hours * float64(time.Hour))), to
}

// resolveSQL substitutes the bucket token for the resolution this window deserves
// and returns the bind args the query needs — none when it has no $1, so a
// listing query runs unchanged.
func resolveSQL(sqlText string, from, to time.Time) (string, []any) {
	resolved := strings.ReplaceAll(sqlText, bucketToken, autoBucket(to.Sub(from)))
	if !strings.Contains(resolved, "$1") {
		return resolved, nil
	}
	return resolved, []any{from, to}
}

// needsBucketing spots a raw, unbucketed windowed time-series that got truncated
// at the row cap — the shape "give me real 1-minute data over a year" produces:
// windowed ($1) but no time_bucket, so ORDER BY ts + LIMIT returns only the first
// maxRows rows (the window's opening slice), silently dropping the rest. When true,
// AskData re-emits the query forcing %BUCKET% aggregation so it covers the whole
// window. DISTINCT/GROUP BY queries are listings/aggregates, not raw series — excluded.
func needsBucketing(sqlLower, runText string, rowCount int) bool {
	return rowCount >= maxRows &&
		strings.Contains(runText, "$1") &&
		!strings.Contains(sqlLower, "time_bucket") &&
		!strings.Contains(sqlLower, "distinct") &&
		!strings.Contains(sqlLower, "group by")
}

// runScoped executes a validated SELECT for one org inside a read-only transaction.
// Org isolation comes from the app.current_org GUC (the views filter on it); writes
// are blocked by the read-only tx; runaway queries by statement_timeout + a row cap.
// maxRows is the hard row ceiling for any scoped query. A time-series that hits it
// was truncated — AskData detects that (needsBucketing) and forces aggregation.
const maxRows = 5000

func runScoped(ctx context.Context, orgID, sqlText string, args ...any) (cols []string, rows [][]any, err error) {
	return runScopedIn(ctx, orgID, "", sqlText, args...)
}

// runScopedIn additionally scopes the canonical tables to one factory. series and
// readings are FORCE row-level-security on app.factory, so a query reaching them
// without a factory returns nothing rather than everything — an unset scope fails
// closed. The telemetry dataset passes "" because its own views filter by org.
func runScopedIn(ctx context.Context, orgID, factoryID, sqlText string, args ...any) (cols []string, rows [][]any, err error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Release()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	if _, err = tx.Exec(ctx, "SET LOCAL statement_timeout = '5s'"); err != nil {
		return nil, nil, err
	}
	// is_local=true → scoped to this tx, cleared on rollback.
	if _, err = tx.Exec(ctx, "SELECT set_config('app.current_org', $1, true)", orgID); err != nil {
		return nil, nil, err
	}
	if _, err = tx.Exec(ctx, "SELECT set_config('app.factory', $1, true)", factoryID); err != nil {
		return nil, nil, err
	}

	r, err := tx.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, nil, err
	}
	defer r.Close()

	for _, fd := range r.FieldDescriptions() {
		cols = append(cols, string(fd.Name))
	}
	for r.Next() {
		if len(rows) >= maxRows {
			break
		}
		vals, verr := r.Values()
		if verr != nil {
			return nil, nil, verr
		}
		rows = append(rows, vals)
	}
	if err = r.Err(); err != nil {
		return nil, nil, err
	}
	return cols, rows, nil
}

// ── LLM calls ────────────────────────────────────────────────────────────────

var emitSQLTool = map[string]any{
	"name":        "emit_sql",
	"description": "Return a single read-only Postgres SELECT that answers the user's question.",
	"input_schema": map[string]any{
		"type":     "object",
		"required": []string{"answerable", "sql"},
		"properties": map[string]any{
			"answerable":    map[string]any{"type": "boolean", "description": "false ONLY for a greeting, chit-chat, clearly non-factory input, or a question about a previous chart/result itself (how it was computed, its interval) — then leave sql empty. A data-listing question ('which SKUs', 'what machines', 'list values') is answerable=true."},
			"sql":           map[string]any{"type": "string", "description": "One SELECT over the allowed v_ views. No semicolons, no CTEs, no writes. Always include a LIMIT."},
			"window_hours":  map[string]any{"type": "number", "description": "For a RELATIVE lookback: the span the SQL's $1/$2 window should cover, in hours ending now: last 24h → 24, last 7 days → 168, last month → 720, last year → 8760. Omit (or 0) when the query has no time filter, names no range, or when you set from/to instead — the server then uses 24."},
			"from":          map[string]any{"type": "string", "description": "For an ABSOLUTE calendar range (a named month/year like 'May 2026' or 'August 2025', or explicit dates) set the range START as an ISO date 'YYYY-MM-DD'. The SQL still uses $1/$2 — the server binds them to from/to. Set together with to, and leave window_hours out."},
			"to":            map[string]any{"type": "string", "description": "The range END as an ISO date 'YYYY-MM-DD', EXCLUSIVE (the first day AFTER the range). For 'May 2026' use from=2026-05-01, to=2026-06-01. Set only together with from."},
			"clarification": map[string]any{"type": "string", "description": "Set ONLY when the question IS about factory data but you cannot determine WHAT to query — no identifiable metric/machine/dimension. ONE short question in the user's language, offering concrete choices from the schema. Leave empty when a sensible default exists (no time range → assume last 24h). Never set together with sql."},
		},
	},
}

// errNotDataQuestion signals the input wasn't a factory-data question (greeting, etc.).
var errNotDataQuestion = errors.New("that doesn't look like a question about your factory data — try asking about machine speed, counts, or trends")

var emitEChartTool = map[string]any{
	"name":        "emit_echart_option",
	"description": "Return an ECharts option object (no data — a dataset is injected at render time) that best visualizes the result.",
	"input_schema": map[string]any{
		"type":     "object",
		"required": []string{"option", "caption"},
		"properties": map[string]any{
			"option":       map[string]any{"type": "object", "description": "A complete ECharts option: title, tooltip, xAxis, yAxis, legend, series[] with type and encode. Reference result columns by name via encode; do NOT embed data or dataset."},
			"caption":      map[string]any{"type": "string", "description": "ONE short sentence, in the user's language, stating what the chart shows and at what resolution — and, when the given bucket is coarser than the user asked for, that it was aggregated to fit the window and zooming in gives finer detail."},
			"analysis":     map[string]any{"type": "string", "description": "A SHORT analysis (1-2 sentences, same language as the question) of what the data shows — the trend, the peak/low and when, or a notable difference between machines. Ground EVERY number in the provided summary/rows; never invent values. Different from caption: caption says what the chart IS, analysis says what it MEANS."},
			"nextQuestion": map[string]any{"type": "string", "description": "ONE natural follow-up question the user might ask next, in their language, concrete and answerable from this data (e.g. compare another machine, zoom a narrower range, look at a related metric). Phrase it as the user would type it."},
		},
	},
}

// buildSchemaContext describes the queryable views plus this org's machine names and
// metric keys, so the SQL model targets real fields. Reuses runScoped for org scoping.
func buildSchemaContext(ctx context.Context, orgID string) string {
	var b strings.Builder
	b.WriteString(`You may ONLY query these read-only Postgres + TimescaleDB views (org data is already filtered):

v_telemetry(machine_id uuid, machine_name text, ts timestamptz, data jsonb)
  - one row per reading; metric values live in `)
	b.WriteString("`data`")
	b.WriteString(` JSONB. Read a metric as (data->>'<key>')::float.
v_machines(id uuid, name text, type text, status text)
v_machine_fields(machine_id uuid, machine_name text, key text, label text, unit text)

`)

	if _, rows, err := runScoped(ctx, orgID, "SELECT name FROM v_machines ORDER BY name LIMIT 200"); err == nil && len(rows) > 0 {
		names := make([]string, 0, len(rows))
		for _, r := range rows {
			names = append(names, fmt.Sprint(r[0]))
		}
		b.WriteString("Machines: " + strings.Join(names, ", ") + "\n")
	}
	if _, rows, err := runScoped(ctx, orgID, "SELECT DISTINCT key, label, COALESCE(unit,'') FROM v_machine_fields ORDER BY key LIMIT 200"); err == nil && len(rows) > 0 {
		keys := make([]string, 0, len(rows))
		for _, r := range rows {
			k, l, u := fmt.Sprint(r[0]), fmt.Sprint(r[1]), fmt.Sprint(r[2])
			sameLabel := strings.EqualFold(l, k)
			switch {
			case sameLabel && u == "":
				keys = append(keys, k)
			case u == "":
				keys = append(keys, fmt.Sprintf("%s (%s)", k, l))
			case sameLabel:
				keys = append(keys, fmt.Sprintf("%s (%s)", k, u))
			default:
				keys = append(keys, fmt.Sprintf("%s (%s %s)", k, l, u))
			}
		}
		b.WriteString("Metric keys (data->>'key'): " + strings.Join(keys, ", ") + "\n")
	}
	b.WriteString("The data JSONB also holds TEXT dimensions (not numeric), notably `sku` (product/SKU code) — read as data->>'sku'. List available values with SELECT DISTINCT data->>'sku'.\n")

	b.WriteString(`
Rules:
- Exactly ONE SELECT. No semicolons, no CTEs, no INSERT/UPDATE/DELETE/DDL.
- Any question about a time range or trend ("last N hours/days", "over time", "per hour", "trend", "history") MUST return a time series: GROUP BY time_bucket('%BUCKET%', ts) AS bucket and ORDER BY bucket, giving many rows — never a single scalar. Write the interval as the LITERAL token '%BUCKET%', exactly like that: the server substitutes the resolution the window deserves (a year gets days, an hour gets minutes) and re-substitutes it when the user zooms in. NEVER write a real interval like '1 hour' there.
- A window ("past/last N units", "recent", "latest", or the same in other languages — e.g. Thai "ย้อนหลัง N", "ล่าสุด") MUST be bounded with WHERE ts >= $1 AND ts < $2 — the server binds both ends. NEVER write now(), NEVER hardcode a date, NEVER an interval literal, and never leave a time query unbounded. Report the span you mean in window_hours (last 24h → 24, 7 days → 168, a month → 720, a year → 8760); no range named → omit it and the server uses 24h. Questions in any language map to this same SQL.
- Numeric metric: (data->>'speed')::float. When reading a metric, ALWAYS filter AND data->>'<key>' IS NOT NULL — machines that never report that metric must not appear in the result.
- A "which/what <dimension> are available / exist / does X run" question (e.g. SKUs) is a listing query, NOT a time series: SELECT DISTINCT data->>'sku' AS sku FROM v_telemetry WHERE data->>'sku' IS NOT NULL ORDER BY sku (filter by machine with machine_name ILIKE '%<code>%').
- Match a machine by name with ILIKE '%<code>%' (e.g. machine_name ILIKE '%CW-01%'), NEVER exact =. Names include a descriptive prefix, so the user's "CW-01" is stored as "Checkweigher CW-01". Same for v_machines.name.
- Give columns clear aliases (bucket, machine_name, avg_speed, ...). Always add LIMIT (<= 5000). Aggregate raw readings into buckets or groups rather than returning every row.`)
	return b.String()
}

// askScope is what a question is asked against: which dataset, and for the
// canonical dataset which factory (one factory per question keeps the generated
// prompt bounded no matter how many plants exist).
type askScope struct {
	Dataset   string // "" / "telemetry" (demo org views) | "canonical" (a real factory)
	FactoryID string
}

func (s askScope) canonical() bool { return s.Dataset == "canonical" && s.FactoryID != "" }

// schemaFor returns the schema-context prompt for a scope.
//   - "" / "telemetry" — per-org context assembled from the DB (mock/demo data)
//   - "canonical"      — generated from the source registry for one factory
//
// Adding a plant, machine, metric or upstream table changes rows, not this function.
// The retired "nj5" dataset answers with an error rather than silently falling through
// to the demo prompt, which would return confident answers about the wrong data.
func schemaFor(ctx context.Context, sc askScope, orgID string) (string, error) {
	switch {
	case sc.canonical():
		cat, err := loadCatalog(ctx, sc.FactoryID)
		if err != nil {
			return "", err
		}
		return cat.promptContext(), nil
	case sc.Dataset == "nj5":
		return "", errors.New(`the "nj5" dataset was replaced by dataset "canonical" with the Nongjok5 factoryId`)
	default:
		return buildSchemaContext(ctx, orgID), nil
	}
}

// The NJ5 dataset used to live here as a ~30-line hand-written prompt over four
// v_nj5* views, with every semantic rule ("count_fg is cumulative, use MAX-MIN",
// "freezing_time 9999 is a sentinel", "rail_temp is always NULL") stated as prose the
// model was trusted to follow. All of it is now data: see scripts/nj5-registry.sql for
// the same facts as source_metrics rows, and compiler.go for the rules being enforced
// rather than requested. scripts/nj5-retire.sql drops the views themselves.

// prevTurn carries the immediately-previous Ask-Data turn so a follow-up ("make it a
// bar chart", "group by day", "เอาเป็นกราฟแท่ง") can refine it instead of being rejected.
// SQL and Clarification are mutually exclusive: a data turn sets SQL, a clarification
// turn (B3) sets Clarification — never both.
type prevTurn struct {
	Question      string
	SQL           string
	Clarification string
	// Spec is the canonical path's equivalent of SQL: the JSON query spec the
	// previous turn compiled. Unlike SQL text it survives changes to the relations
	// underneath, so a follow-up refines the intent rather than patching a string.
	Spec string
	// Window the previous SQL's $1/$2 were bound to, so re-running it for a prose
	// follow-up ("what does this chart show") reads the same range the user saw.
	WindowHours float64
}

// sqlFixup carries a failed SQL attempt and its error for the retry loop in AskData.
type sqlFixup struct {
	SQL string
	Err string
}

// sqlEmission is emit_sql's parsed result: either SQL (answerable) or Clarification
// (answerable but under-specified — B3), never both. See parseSQLEmission.
type sqlEmission struct {
	SQL           string
	Clarification string
	WindowHours   float64
	// From/To are set when the question names an absolute calendar range ("May 2026",
	// "August 2025", explicit dates) instead of a relative lookback. Both set → the
	// server binds $1/$2 to them instead of windowFor(WindowHours). To is exclusive.
	From *time.Time
	To   *time.Time
}

// parseEmissionTime accepts an ISO date ("2026-05-01") or RFC3339 datetime from the
// model and returns it as UTC. ponytail: dates land at UTC midnight, not the user's
// +07 — immaterial for month-scale aggregates; revisit if we ever need day-exact edges.
func parseEmissionTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// askCallTimeout bounds ONE generation call (emitSQL / emitProse / emitEChart).
// Raised 60s -> 90s because the slow KKU reasoning provider spends most of a call
// on hidden thinking: the analyze path runs TWO such calls back-to-back (emitSQL
// classifier, then emitProse) and 60s each no longer left the second one room.
//
// This is the innermost rung of a timeout ladder that must stay strictly
// increasing, or an outer layer silently becomes the real limit (a 30s nginx
// once cut a successful 32s /ask, see frontend/nginx.conf):
//
//	90s  per generation call   (here)
//	100s HTTP client           (controller.go callAIModel)
//	200s AskData handler       (askHandlerTimeout below)
//	210s nginx proxy_read      (frontend/nginx.conf)
//	210s browser axios         (api.service.ts askData)
//
// The judge calls stay at 6s — they run on the small router model, and a slow
// verdict must never delay an answer that is already correct.
const askCallTimeout = 90 * time.Second

// askHandlerTimeout is the whole-request ceiling: emitSQL + the prev-SQL re-run
// + emitProse/emitEChart + judge, plus one repair round. Sized so the analyze
// path's two sequential 90s reasoning calls fit inside it (emitSQL 90 + rerun 8
// + emitProse 90 + judge 6 ≈ 194s); the optional repair round degrades gracefully
// if it can't fit before the deadline (the caller keeps the answer already made).
const askHandlerTimeout = 200 * time.Second

func emitSQL(ctx context.Context, question, schema string, prev *prevTurn, fixup *sqlFixup) (sqlEmission, error) {
	ctx, cancel := context.WithTimeout(ctx, askCallTimeout)
	defer cancel()
	today := time.Now().Format("2006-01-02")
	sp := "You translate a factory-analytics question into ONE read-only Postgres SELECT by calling emit_sql. Never reply in prose.\n\n" +
		"Today is " + today + ". Resolve every named or relative date against it.\n\n" +
		"Example — \"avg speed last 24h for CW-01\" (window_hours 24):\n" +
		"SELECT time_bucket('%BUCKET%', ts) AS bucket, avg((data->>'speed')::float) AS avg_speed " +
		"FROM v_telemetry WHERE machine_name ILIKE '%CW-01%' AND ts >= $1 AND ts < $2 " +
		"AND data->>'speed' IS NOT NULL GROUP BY bucket ORDER BY bucket LIMIT 5000\n\n" +
		"Example — \"which SKUs does CW-01 run\" (a listing question, answerable=true):\n" +
		"SELECT DISTINCT data->>'sku' AS sku FROM v_telemetry WHERE machine_name ILIKE '%CW-01%' " +
		"AND data->>'sku' IS NOT NULL ORDER BY sku LIMIT 100\n\n" +
		"A \"which/what values are available\" listing question IS answerable — return the distinct values; " +
		"set answerable=false ONLY for a greeting or chit-chat.\n\n" +
		"A question asking to EXPLAIN or DEFINE a metric or term (\"what does X mean\", \"how do X and Y " +
		"differ\", Thai \"อธิบาย\", \"คืออะไร\", \"ต่างกันยังไง\") is answered in prose, not SQL — set " +
		"answerable=false and NEVER set clarification for it.\n" +
		"When a reasonable default interpretation exists, answer with the default instead of asking: no time " +
		"range → last 24h; a fuzzy condition like \"drops/low\" → below that metric's average over the window. " +
		"Set clarification ONLY when no metric, machine, or dimension is identifiable at all.\n\n" +
		"CALENDAR RANGE vs LOOKBACK — get this right or the window is wrong:\n" +
		"- A named calendar range (explicit dates, or named months/quarters — \"March to April\", " +
		"\"เดือนมีนาถึงเมษา\", \"Q1\", \"last month\", \"เดือนที่แล้ว\") → set from AND to as ISO 'YYYY-MM-DD' and OMIT " +
		"window_hours. to is EXCLUSIVE, the first day AFTER the range (March–April → from=<year>-03-01, " +
		"to=<year>-05-01). If the year is unstated, use the most recent occurrence already ended on or before today.\n" +
		"- Use window_hours ONLY for a relative lookback ending now (\"last 7 days\", \"ย้อนหลัง 7 วัน\", \"recent\"). " +
		"NEVER answer a named calendar range with window_hours — that measures back from today and misses the range.\n\n" +
		"A time-series over a window MUST aggregate with time_bucket('%BUCKET%', ts) even when the user " +
		"explicitly demands raw / per-reading / every-minute data (\"ข้อมูลจริง\", \"ทุก 1 นาที\", \"every row\") — " +
		"NEVER return raw ungrouped rows for a window, or a long window is silently truncated to its first rows. " +
		"The server picks the finest resolution that fits and the user can zoom in for finer detail.\n\n"
	switch {
	case prev != nil && prev.SQL != "":
		sp += "The user previously asked: \"" + prev.Question + "\"\nwhich ran this SQL:\n" + prev.SQL +
			"\nIf the new message refines or restyles that chart (a different chart type, grouping, interval, " +
			"filter, or metric) rather than starting a new topic, adapt the previous SQL to answer it and set " +
			"answerable=true — for a pure chart-type change ('make it a bar chart') return the SAME SQL unchanged. " +
			"Set answerable=false for a greeting or chit-chat, and ALSO for a question ABOUT the previous " +
			"chart/result itself (how it was computed, what the bucket interval is, what a point means) rather " +
			"than a request for different data — that is answered in prose, not SQL.\n\n"
	case prev != nil && prev.Clarification != "":
		sp += "The user originally asked: \"" + prev.Question + "\", and you asked them a clarifying question: \"" +
			prev.Clarification + "\". The current message is their reply to that question — combine the original " +
			"question and this reply into ONE SQL query that answers it. Do not set clarification again; never ask " +
			"for clarification a second time in a row.\n\n"
	}
	if fixup != nil {
		sp += "Your previous attempt:\n" + fixup.SQL + "\nfailed with this Postgres/validation error:\n" +
			fixup.Err + "\nReturn a corrected query.\n\n"
	}
	sp += schema
	msgs := []aiMessage{{Role: "system", Content: &sp}, {Role: "user", Content: strPtr(question)}}
	tools := []map[string]any{toAITool(emitSQLTool)}
	resp, _, err := callAIModel(ctx, aiModel(), msgs, tools, forceFunc("emit_sql"))
	// The model declining the forced call (prose instead of a tool call, or the provider's
	// "tool choice" validator error) means it judged the question un-SQL-able — a meta
	// question about the previous chart, say. Degrade to the prose path, not a 502;
	// same stance as Chat's fallback (controller.go): forced is an optimization.
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "tool choice") {
			return sqlEmission{}, errNotDataQuestion
		}
		return sqlEmission{}, err
	}
	if len(resp.Choices) == 0 || len(resp.Choices[0].Message.ToolCalls) == 0 {
		return sqlEmission{}, errNotDataQuestion
	}
	return parseSQLEmission(resp.Choices[0].Message.ToolCalls[0].Function.Arguments)
}

// parseSQLEmission is separated from the HTTP call so it's unit-testable without the
// network. Clarification wins over sql when both happen to be set (the model was told
// never to do this, but the parse stays defensive). !answerable with no clarification
// -> errNotDataQuestion (the pre-B3 contract, unchanged for the 36/36 live suite).
func parseSQLEmission(rawJSON string) (sqlEmission, error) {
	var a struct {
		Answerable    bool    `json:"answerable"`
		SQL           string  `json:"sql"`
		Clarification string  `json:"clarification"`
		WindowHours   float64 `json:"window_hours"`
		From          string  `json:"from"`
		To            string  `json:"to"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &a); err != nil {
		return sqlEmission{}, err
	}
	if c := strings.TrimSpace(a.Clarification); c != "" {
		return sqlEmission{Clarification: c}, nil
	}
	if !a.Answerable || strings.TrimSpace(a.SQL) == "" {
		return sqlEmission{}, errNotDataQuestion
	}
	e := sqlEmission{SQL: a.SQL, WindowHours: a.WindowHours}
	// An absolute range needs BOTH ends to parse and be ordered; otherwise ignore it
	// and fall back to the relative window (windowFor).
	if f, okF := parseEmissionTime(a.From); okF {
		if t, okT := parseEmissionTime(a.To); okT && t.After(f) {
			e.From, e.To = &f, &t
		}
	}
	return e, nil
}

// emitProse answers a question that isn't a SQL query (an explanation or follow-up like
// "how do they differ", "analyze this chart") in plain text — the fallback for emitSQL's
// answerable=false branch. Grounded in the same schema context; a plain completion (no
// tools). cols/rows, when non-empty, are the ACTUAL re-executed result of prev.SQL —
// without them the model invents numbers for "analyze the chart" questions.
//
// Runs on aiModel() (the main model): analysis quality is the whole point of "analyze
// this", so it wants the strong model, not the small router one. This was briefly moved
// to routerModel() when the main model was kimi-k3 — a reasoning model that burned ~77s +
// ~5k tokens per prose call and blew the timeout — but under a fast main model (sonnet)
// that isn't a problem, and the grounded summary keeps the numbers correct regardless.
func emitProse(ctx context.Context, question, schema string, prev *prevTurn, cols []string, rows [][]any, summary, fixup string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, askCallTimeout)
	defer cancel()
	sp := "You are a factory-data assistant for an IoT dashboard. Answer the user's question directly and " +
		"concisely in prose, in the SAME language as the question (Thai or English). Use the schema below to " +
		"ground your answer in the real machines, metrics, and units. Do not output SQL or code unless asked.\n\n"
	if fixup != "" {
		sp += "Your previous answer was judged as not answering the question:\n" + fixup +
			"\nWrite a corrected answer that addresses the question directly.\n\n"
	}
	if prev != nil && prev.SQL != "" {
		sp += "For context, the user's previous question was: \"" + prev.Question + "\" (it ran SQL: " + prev.SQL + ").\n\n"
		if len(cols) > 0 {
			sp += "That SQL was just re-executed. " + summary +
				"A representative sample of the rows follows (" + fmt.Sprint(len(rows)) +
				" rows, evenly spaced across the full range). Ground EVERY number, time range, and machine name " +
				"in the summary above and these rows — never invent or estimate values not present. If they don't " +
				"cover what's asked, say so.\n" +
				serializeRows(cols, rows) + "\n"
		}
	} else if prev != nil && prev.Clarification != "" {
		sp += "For context, the user's previous question was: \"" + prev.Question + "\" and you asked them: \"" + prev.Clarification + "\".\n\n"
	}
	sp += schema
	msgs := []aiMessage{{Role: "system", Content: &sp}, {Role: "user", Content: strPtr(question)}}
	resp, _, err := callAIModel(ctx, aiModel(), msgs, nil, "")
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == nil {
		return "", errors.New("no answer generated")
	}
	return strings.TrimSpace(*resp.Choices[0].Message.Content), nil
}

const echartSystemPrompt = `You turn a SQL result into an ECharts option that answers the user's question, by calling emit_echart_option. Never reply in prose.
- Pick the chart type — use ONLY 'line', 'bar', 'pie', 'scatter', or 'heatmap': a time-bucket column → line; a category comparison → bar; parts-of-a-whole → pie; two categorical dimensions plus one numeric value → heatmap.
- A dataset with the result rows is injected AT RENDER TIME. Reference result columns BY NAME using encode (e.g. series:[{type:'line', encode:{x:'bucket', y:'avg_speed'}}]). Do NOT include any data arrays or a dataset field yourself.
- Emit exactly ONE series even when the rows contain a category column (e.g. machine_name) — the renderer automatically splits one series into one line per category value. NEVER emit one series per machine/category: without per-series filters they would all draw identical data.
- EXCEPTION — two DIFFERENT numeric columns (e.g. produced_count and evap_temp) are two different quantities, so emit one series each and give them separate axes: yAxis:[{},{}] with the second series set to yAxisIndex:1. Draw the counted quantity as 'bar' and the measured one as 'line'. Sharing one axis here makes the smaller quantity a flat line at the bottom.
- Set xAxis.type: 'time' for a timestamp/bucket column, 'category' for names. Add a short title, tooltip{trigger:'axis'}, and a legend when there are multiple series.
- Style variants stay on their base type (still ONE series): for an area chart set areaStyle:{} on a 'line' series; for a smoothed line set smooth:true; for a horizontal bar keep type:'bar' but set yAxis.type:'category' and xAxis.type:'value' (encode x=value, y=category).
- For a 'heatmap' series use encode:{x:<category>, y:<category>, value:<numeric>}, set BOTH xAxis.type and yAxis.type to 'category', tooltip{trigger:'item'}, and add a visualMap with inRange.color as a low→high scale (e.g. ['#22c55e','#eab308','#ef4444']). Do NOT set visualMap min/max — the renderer fills them from the real data.
- If the user's message explicitly names a chart type or style (bar/line/pie/scatter/heatmap/area/horizontal, or the same in another language e.g. Thai "กราฟแท่ง"=bar, "กราฟเส้น"=line, "วงกลม"=pie, "ฮีตแมป"/"แผนที่ความร้อน"=heatmap, "กราฟพื้นที่"=area, "แนวนอน"=horizontal), use THAT even if another would be more typical.
- Column names and a few sample rows are given below for type inference only.
- Always write the caption too: one sentence, same language as the question, about what the chart shows — not a restatement of the title.
- Also write a SHORT analysis (1-2 sentences) and ONE suggested nextQuestion, both in the user's language. Ground the analysis in the provided "summary" (per-machine min/max/avg over ALL rows) and sample rows — state the trend, the peak/low and roughly when, or a notable machine difference; never invent a number not in the summary/rows. The nextQuestion is a natural follow-up the user would type next (compare another machine, zoom a range, a related metric).
- When a "bucket" field is given, that is the ACTUAL resolution the rows were aggregated at — the server picks it from the window, so it may be coarser than the user asked for. Say it in the title (e.g. "Speed over 1 year (1 day avg)"). NEVER title a chart with a resolution the user requested but the data does not have.`

// chartEmission is emitEChart's parsed result: the ECharts option plus the short
// texts folded into the same tool call — caption (what the chart is), analysis (what
// it means, grounded in the summary), and a suggested nextQuestion. analysis and
// nextQuestion are optional (the schema doesn't require them): an older/terser model
// may omit them, and the frontend just hides what's empty.
type chartEmission struct {
	Option       json.RawMessage
	Caption      string
	Analysis     string
	NextQuestion string
}

// leakedNextQ recovers a nextQuestion that some models bleed into another text field as
// an XML-ish tool tag (<parameter name="nextQuestion">…) instead of a clean JSON value.
var leakedNextQ = regexp.MustCompile(`(?is)<\s*parameter\s+name\s*=\s*"?nextquestion"?\s*>(.*?)(?:<\s*/\s*parameter\s*>|$)`)

// anyTag strips a stray XML/HTML tag. Claude via some proxies emits tool arguments in an
// XML format whose tags leak into the JSON string VALUES (a trailing </analysis>, a
// <parameter …> starting the next field) — the user must never see raw markup.
var anyTag = regexp.MustCompile(`(?s)<[^>]*>`)

// cleanChartTexts scrubs the folded caption/analysis/nextQuestion of leaked tool-format
// tags and recovers a nextQuestion that bled into another field. Defensive: a clean JSON
// tool call passes through untouched.
func cleanChartTexts(ce chartEmission) chartEmission {
	if strings.TrimSpace(ce.NextQuestion) == "" {
		for _, field := range []string{ce.Analysis, ce.Caption} {
			if m := leakedNextQ.FindStringSubmatch(field); m != nil {
				ce.NextQuestion = m[1]
				break
			}
		}
	}
	ce.Caption = cleanLeak(ce.Caption)
	ce.Analysis = cleanLeak(ce.Analysis)
	ce.NextQuestion = cleanLeak(ce.NextQuestion)
	return ce
}

// cleanLeak cuts a string at the first leaked tag boundary (the real value ends there),
// strips any residual tags, and trims. Case-insensitive on the markers; Thai/ASCII byte
// indices align under ToLower so slicing the original by the found index is safe.
func cleanLeak(s string) string {
	low := strings.ToLower(s)
	for _, marker := range []string{"</analysis>", "</parameter>", "<parameter", "<function", "</function", "</caption>"} {
		if i := strings.Index(low, marker); i >= 0 {
			s, low = s[:i], low[:i]
		}
	}
	return strings.TrimSpace(anyTag.ReplaceAllString(s, ""))
}

// emitEChart generates an ECharts option plus the folded caption/analysis/nextQuestion.
// prevErr, when non-empty, is the previous attempt's error text — passed back to the
// model as one retry (B2); pass "" for a fresh (non-retry) call. summary, when non-empty,
// is the per-machine stats block (summarizeRows) that grounds the analysis; pass "" to
// skip it (the analysis then leans on the sample rows alone).
func emitEChart(ctx context.Context, question string, cols []string, sample [][]any, prevErr, bucket, summary string) (chartEmission, error) {
	ctx, cancel := context.WithTimeout(ctx, askCallTimeout)
	defer cancel()
	payloadMap := map[string]any{"question": question, "columns": cols, "sampleRows": sample}
	if bucket != "" {
		payloadMap["bucket"] = bucket
	}
	if summary != "" {
		payloadMap["summary"] = summary
	}
	if prevErr != "" {
		payloadMap["previousError"] = prevErr + " — return a corrected option"
	}
	payload, _ := json.Marshal(payloadMap)
	sp := echartSystemPrompt
	uc := string(payload)
	msgs := []aiMessage{{Role: "system", Content: &sp}, {Role: "user", Content: &uc}}
	tools := []map[string]any{toAITool(emitEChartTool)}
	resp, _, err := callAIModel(ctx, aiModel(), msgs, tools, forceFunc("emit_echart_option"))
	if err != nil {
		return chartEmission{}, err
	}
	if len(resp.Choices) == 0 || len(resp.Choices[0].Message.ToolCalls) == 0 {
		return chartEmission{}, errors.New("no chart generated")
	}
	var a struct {
		Option       json.RawMessage `json:"option"`
		Caption      string          `json:"caption"`
		Analysis     string          `json:"analysis"`
		NextQuestion string          `json:"nextQuestion"`
	}
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.ToolCalls[0].Function.Arguments), &a); err != nil {
		return chartEmission{}, err
	}
	ce := cleanChartTexts(chartEmission{Option: a.Option, Caption: a.Caption, Analysis: a.Analysis, NextQuestion: a.NextQuestion})
	ce.Caption = truncateRunes(ce.Caption, 300)
	ce.Analysis = truncateRunes(ce.Analysis, 500)
	ce.NextQuestion = truncateRunes(ce.NextQuestion, 200)
	return ce, nil
}

// askVerifyPrompt is a small, static prompt (~200 tok) so the provider can prompt-cache it where supported
// across verify calls, mirroring verifySystemPrompt (router.go) but scoped to
// Ask-Data's SQL+chart turns instead of the chat tool-call path.
const askVerifyPrompt = `You check whether a generated SQL query — and its chart, when one is present — actually answers a factory-data question, by calling verify_answer. Always call the tool — never reply in prose.

The option field may be an empty object {} — that means the result is delivered as a plain table with no chart. In that case judge only whether the SQL and sample rows answer the question; chart-type rules do not apply.

MISMATCH (matches_intent: false) only when the SQL or chart targets a DIFFERENT metric, machine, or time window than the question asked, or (chart present only) the chart type contradicts a chart type the user explicitly requested (e.g. asked for a bar chart but got a pie chart).

A chart type or style the user explicitly requested (pie/bar/line/scatter/heatmap/area/horizontal, in any language — e.g. Thai "กราฟแท่ง"=bar, "วงกลม"=pie, "ฮีตแมป"/"แผนที่ความร้อน"=heatmap, "กราฟพื้นที่"=area, "แนวนอน"=horizontal) is correct BY DEFINITION, even if another type would visualize the data better — judge only the DATA (metric, machine, time window), never the style the user chose.

MATCH (matches_intent: true) otherwise — including a result that is imperfect but honestly answers what was asked (fewer points than ideal, a slightly different aggregation, a reasonable default time window when none was specified, an empty result when the data may genuinely contain no matching rows).

If mismatch, set problem to a short specific reason (e.g. "answered temperature, user asked speed"). Leave clarifying_question empty — Ask-Data repairs automatically rather than asking the user.`

// askProseVerifyPrompt mirrors askVerifyPrompt but for prose turns: small and static
// so the provider can prompt-cache it across verify calls.
const askProseVerifyPrompt = `You check whether a prose answer actually addresses a factory-data question, by calling verify_answer. Always call the tool — never reply in prose.

sampleRows, when non-empty, are the ACTUAL rows the answer was grounded in.

MISMATCH (matches_intent: false) only when the answer is about a DIFFERENT topic than the question asked, or states a number that CONTRADICTS the sample rows.

MATCH (matches_intent: true) otherwise — including an imperfect but on-topic answer, a definition/explanation for an explain question, a polite reply to a greeting, or an honest "the data doesn't show this". When sampleRows is empty, judge topicality only — never flag missing numbers.

If mismatch, set problem to a short specific reason. Leave clarifying_question empty — the answer is regenerated automatically rather than asking the user.`

// verifyAskProse judges whether a prose answer addresses question, grounded by the
// same rows emitProse saw. Mirrors verifyAskAnswer exactly: 6s bound, routerModel(),
// forced verify_answer; (zero, false) on ANY failure = no verdict, deliver as-is.
func verifyAskProse(ctx context.Context, question, answer string, cols []string, sample [][]any) (VerifyResult, bool) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	payload, _ := json.Marshal(map[string]any{
		"question":   question,
		"answer":     truncateRunes(answer, 1500),
		"columns":    cols,
		"sampleRows": sample,
	})

	sp := askProseVerifyPrompt
	uc := string(payload)
	msgs := []aiMessage{{Role: "system", Content: &sp}, {Role: "user", Content: &uc}}

	tools := []map[string]any{toAITool(VerifyAnswerTool)}
	resp, _, err := callAIModel(ctx, routerModel(), msgs, tools, forceFunc("verify_answer"))
	if err != nil || len(resp.Choices) == 0 || len(resp.Choices[0].Message.ToolCalls) == 0 {
		return VerifyResult{}, false
	}
	return parseVerifyResult(resp.Choices[0].Message.ToolCalls[0].Function.Arguments)
}

// verifyAskAnswer judges whether sqlText (+ option, which may be the empty table
// signal "{}") actually answer question. Mirrors VerifyAnswer (router.go) exactly:
// 6s bounded timeout, routerModel(), forced verify_answer tool call. Returns (zero,
// false) on ANY error, timeout, or malformed JSON — callers MUST treat false as "no
// verdict" (deliver as-is), never as a mismatch; the verifier's own infrastructure
// failing must never block or repair an otherwise-fine chart.
func verifyAskAnswer(ctx context.Context, question, sqlText string, cols []string, sample [][]any, option json.RawMessage) (VerifyResult, bool) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	var optM map[string]any
	_ = json.Unmarshal(option, &optM)
	payload, _ := json.Marshal(map[string]any{
		"question":   question,
		"sql":        truncateRunes(sqlText, 1500),
		"columns":    cols,
		"sampleRows": sample,
		"option":     optM,
	})

	sp := askVerifyPrompt
	uc := string(payload)
	msgs := []aiMessage{{Role: "system", Content: &sp}, {Role: "user", Content: &uc}}

	tools := []map[string]any{toAITool(VerifyAnswerTool)}
	resp, _, err := callAIModel(ctx, routerModel(), msgs, tools, forceFunc("verify_answer"))
	if err != nil {
		return VerifyResult{}, false
	}
	if len(resp.Choices) == 0 {
		return VerifyResult{}, false
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) == 0 {
		return VerifyResult{}, false
	}
	return parseVerifyResult(calls[0].Function.Arguments)
}

// hasNumericColumn reports whether any column holds numeric values — the signal that
// a result is chartable. A column is classified by its first non-null value. Pure text
// results (e.g. "list machines") have none, so we render them as a table instead.
func hasNumericColumn(cols []string, rows [][]any) bool {
	for ci := range cols {
		for _, r := range rows {
			if ci >= len(r) || r[ci] == nil {
				continue
			}
			switch r[ci].(type) {
			case int, int16, int32, int64, float32, float64, pgtype.Numeric:
				return true
			}
			break // first non-null value classifies the column; not numeric → next column
		}
	}
	return false
}

// sanitizeEChartOption removes data injection points and validates series references.
// Returns "{}" if the option is invalid or references nonexistent columns.
func sanitizeEChartOption(option json.RawMessage, cols []string) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(option, &m); err != nil || m == nil {
		return json.RawMessage("{}")
	}
	delete(m, "dataset")

	// Normalize series to []map[string]any.
	var series []map[string]any
	if rawSeries, ok := m["series"]; ok {
		switch s := rawSeries.(type) {
		case []any:
			for _, item := range s {
				if sm, ok := item.(map[string]any); ok {
					series = append(series, sm)
				}
			}
		case map[string]any:
			series = append(series, s)
		default:
			return json.RawMessage("{}")
		}
	}

	// Validate and clean series.
	validSeries := make([]map[string]any, 0, len(series))
	seen := map[string]bool{}
	for _, s := range series {
		delete(s, "data")

		// Check type is allowed.
		t, ok := s["type"].(string)
		if !ok || !slices.Contains([]string{"line", "bar", "pie", "scatter", "heatmap"}, t) {
			return json.RawMessage("{}")
		}

		// Validate encode references.
		if encodeRaw, ok := s["encode"]; ok {
			if enc, ok := encodeRaw.(map[string]any); ok {
				for _, v := range enc {
					if err := validateEncodeValue(v, cols); err != nil {
						return json.RawMessage("{}")
					}
				}
			}
		}

		// Series sharing type+encode are duplicates by construction — with no
		// per-series data or filter (both stripped/absent here) they render
		// identical rows. The model sometimes emits one per machine expecting a
		// filter it cannot supply; keep the first — the frontend splits a single
		// series into one line per category value.
		encJSON, _ := json.Marshal(s["encode"])
		key := t + string(encJSON)
		if seen[key] {
			continue
		}
		seen[key] = true
		validSeries = append(validSeries, s)
	}
	// A chart with no series can never render — fall back to the table signal.
	if len(validSeries) == 0 {
		return json.RawMessage("{}")
	}
	m["series"] = validSeries

	result, _ := json.Marshal(m)
	return result
}

// validateEncodeValue checks that a single or array of column names exist in cols.
func validateEncodeValue(v any, cols []string) error {
	switch val := v.(type) {
	case string:
		if !slices.Contains(cols, val) {
			return errors.New("column not found: " + val)
		}
	case []any:
		for _, item := range val {
			if s, ok := item.(string); ok && !slices.Contains(cols, s) {
				return errors.New("column not found: " + s)
			}
		}
	}
	return nil
}

func sampleRows(rows [][]any, n int) [][]any {
	if len(rows) <= n {
		return rows
	}
	return rows[:n]
}

// downsampleRows picks ~n rows evenly across the whole result (unlike sampleRows'
// head-only cut) so a trend question sees the full time span, not just its start.
func downsampleRows(rows [][]any, n int) [][]any {
	if len(rows) <= n {
		return rows
	}
	out := make([][]any, 0, n)
	step := float64(len(rows)) / float64(n)
	for i := 0; i < n; i++ {
		out = append(out, rows[int(float64(i)*step)])
	}
	return out
}

// serializeRows renders a result compactly for a prose prompt: one header line of
// column names, then one comma-separated line per row.
func serializeRows(cols []string, rows [][]any) string {
	var b strings.Builder
	b.WriteString(strings.Join(cols, ", "))
	b.WriteByte('\n')
	for _, r := range rows {
		parts := make([]string, len(r))
		for i, v := range r {
			switch t := v.(type) {
			case time.Time:
				parts[i] = t.Format(time.RFC3339)
			case pgtype.Numeric:
				if f, err := t.Float64Value(); err == nil {
					parts[i] = fmt.Sprintf("%g", f.Float64)
				} else {
					parts[i] = fmt.Sprint(v)
				}
			default:
				parts[i] = fmt.Sprint(v)
			}
		}
		b.WriteString(strings.Join(parts, ", "))
		b.WriteByte('\n')
	}
	return b.String()
}

// toFloat extracts a float from a pgx-scanned value — matches serializeRows' number
// handling (float, int widths, pgtype.Numeric). false for non-numeric values.
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int16:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case pgtype.Numeric:
		if f, err := t.Float64Value(); err == nil {
			return f.Float64, true
		}
	}
	return 0, false
}

// summarizeRows produces a compact per-numeric-column summary — count, min, max (each
// tagged with the timestamp it occurred at, when the result has a time column), and avg —
// computed over EVERY row, so the small sample that accompanies it in an "analyze" prose
// prompt can be thinned without ever hiding an extreme. When the result carries a category
// (text) column (e.g. machine_name), stats are broken down PER category value — analyze
// charts routinely show several machines at once and a blended global min/max would lose
// which machine spiked. Falls back to one global line per numeric column when there is no
// category column. Returns "" when there are no rows or no numeric column (a text listing).
func summarizeRows(cols []string, rows [][]any) string {
	if len(rows) == 0 {
		return ""
	}
	// Classify columns by their first non-null value: one timestamp col (for WHEN the
	// min/max occurred), one category col (the grouping dimension), the numeric cols.
	timeCol, catCol := -1, -1
	var numCols []int
	for ci := range cols {
		for _, r := range rows {
			if ci >= len(r) || r[ci] == nil {
				continue
			}
			switch r[ci].(type) {
			case time.Time:
				if timeCol < 0 {
					timeCol = ci
				}
			case string:
				if catCol < 0 {
					catCol = ci
				}
			default:
				if _, ok := toFloat(r[ci]); ok {
					numCols = append(numCols, ci)
				}
			}
			break // first non-null value classifies the column
		}
	}
	if len(numCols) == 0 {
		return ""
	}

	type stat struct {
		n             int
		sum, min, max float64
		minAt, maxAt  string
	}
	// group value -> one stat per numeric column (parallel to numCols).
	groups := map[string][]*stat{}
	var order []string
	// ponytail: cap categories so a high-cardinality dimension can't blow the payload;
	// raise only if a real result legitimately needs >50 groups summarized.
	const maxGroups = 50
	truncated := false
	for _, r := range rows {
		cat := ""
		if catCol >= 0 && catCol < len(r) && r[catCol] != nil {
			cat = fmt.Sprint(r[catCol])
		}
		st, ok := groups[cat]
		if !ok {
			if len(order) >= maxGroups {
				truncated = true
				continue
			}
			st = make([]*stat, len(numCols))
			for i := range st {
				st[i] = &stat{}
			}
			groups[cat] = st
			order = append(order, cat)
		}
		when := ""
		if timeCol >= 0 && timeCol < len(r) {
			if t, ok := r[timeCol].(time.Time); ok {
				when = t.Format(time.RFC3339)
			}
		}
		for i, ci := range numCols {
			if ci >= len(r) {
				continue
			}
			f, ok := toFloat(r[ci])
			if !ok {
				continue
			}
			s := st[i]
			if s.n == 0 || f < s.min {
				s.min, s.minAt = f, when
			}
			if s.n == 0 || f > s.max {
				s.max, s.maxAt = f, when
			}
			s.sum += f
			s.n++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Summary over all %d rows:\n", len(rows))
	for i, ci := range numCols {
		if catCol >= 0 {
			fmt.Fprintf(&b, "%s by %s:\n", cols[ci], cols[catCol])
		}
		for _, cat := range order {
			s := groups[cat][i]
			if s.n == 0 {
				continue
			}
			label := cols[ci]
			if catCol >= 0 {
				label = "  " + cat
			}
			fmt.Fprintf(&b, "- %s: min=%g (%s), max=%g (%s), avg=%g, n=%d\n",
				label, s.min, s.minAt, s.max, s.maxAt, s.sum/float64(s.n), s.n)
		}
	}
	if truncated {
		fmt.Fprintf(&b, "(only the first %d categories summarized)\n", maxGroups)
	}
	return b.String()
}

// ── HTTP handlers ────────────────────────────────────────────────────────────

// askAIError maps an AI-generation failure to a Fiber response: a provider daily
// quota (quotaError) surfaces as 429 QUOTA_EXCEEDED so the client can tell "come
// back later" apart from a generic failure; anything else stays a 502 with the
// caller's prefix. Shared by AskData's SQL/prose generation sites.
func askAIError(c *fiber.Ctx, prefix string, err error) error {
	var qe *quotaError
	if errors.As(err, &qe) {
		return c.Status(429).JSON(fiber.Map{"success": false,
			"error": fiber.Map{"code": "QUOTA_EXCEEDED", "message": qe.Error()}})
	}
	return c.Status(502).JSON(fiber.Map{"success": false,
		"error": fiber.Map{"message": prefix + err.Error()}})
}

// AskData: question → SQL → rows → ECharts option. POST /ai/ask
func AskData(c *fiber.Ctx) error {
	user := middleware.GetUser(c)
	if user == nil {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "unauthorized"}})
	}
	var body struct {
		Question  string `json:"question"`
		Dataset   string `json:"dataset"`   // "" / "telemetry" | "canonical"
		FactoryID string `json:"factoryId"` // required by "canonical"
		Context   *struct {
			Question      string  `json:"question"`
			SQL           string  `json:"sql"`
			Spec          string  `json:"spec"`
			Clarification string  `json:"clarification"`
			WindowHours   float64 `json:"windowHours"`
		} `json:"context"`
	}
	if err := c.BodyParser(&body); err != nil || strings.TrimSpace(body.Question) == "" {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "question is required"}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), askHandlerTimeout)
	defer cancel()

	// prev's SQL and Clarification are mutually exclusive (see prevTurn) — SQL wins
	// if a caller somehow sent both.
	var prev *prevTurn
	if body.Context != nil {
		switch {
		case strings.TrimSpace(body.Context.Spec) != "":
			prev = &prevTurn{Question: body.Context.Question, Spec: body.Context.Spec,
				SQL: body.Context.SQL, WindowHours: body.Context.WindowHours}
		case strings.TrimSpace(body.Context.SQL) != "":
			prev = &prevTurn{Question: body.Context.Question, SQL: body.Context.SQL, WindowHours: body.Context.WindowHours}
		case strings.TrimSpace(body.Context.Clarification) != "":
			prev = &prevTurn{Question: body.Context.Question, Clarification: body.Context.Clarification}
		}
	}
	question := strings.TrimSpace(body.Question)
	scope := askScope{Dataset: body.Dataset, FactoryID: body.FactoryID}
	schema, err := schemaFor(ctx, scope, user.OrgId)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false,
			"error": fiber.Map{"message": "unknown data scope: " + err.Error()}})
	}
	// The canonical path resolves the catalog once per request: the prompt above and
	// the compiler below read the SAME loaded catalog, so what the model is told and
	// what the server enforces cannot drift apart.
	var cat *catalog
	if scope.canonical() {
		if cat, err = loadCatalog(ctx, scope.FactoryID); err != nil {
			return askAIError(c, "could not load the factory catalog: ", err)
		}
	}

	// Retry loop: up to 3 attempts to generate and validate SQL (or a spec).
	var cols []string
	var rows [][]any
	var sqlText string
	var specText string
	var bucketUsed string
	var bandCols []string // gauge min/max envelope columns, if the compiler added any
	var fixup *sqlFixup
	// The window the emitted SQL's $1/$2 were bound to. Returned to the client so a
	// zoom (RunSQL with an explicit range) starts from what is actually on screen.
	var windowHours float64
	var from, to time.Time
	for attempt := 1; attempt <= 3; attempt++ {
		var emission sqlEmission
		var spec specEmission
		var err error
		if scope.canonical() {
			spec, err = emitSpec(ctx, question, schema, prev, fixup)
		} else {
			emission, err = emitSQL(ctx, question, schema, prev, fixup)
		}
		if errors.Is(err, errNotDataQuestion) {
			// Not a data query — answer in prose. Re-run the previous turn first (same
			// guards) so an "analyze the chart" answer is grounded in the real rows; on
			// any re-run failure just answer without them.
			var pcols []string
			var prows [][]any
			var psummary string
			if cs, rs, rerr := rerunPrev(ctx, user.OrgId, scope, cat, prev); rerr == nil && len(rs) > 0 {
				pcols = cs
				psummary = summarizeRows(cs, rs) // over ALL rows — extremes never sampled away
				prows = downsampleRows(rs, 40)   // ~40 points, trend shape only
				log.Printf("[ask prose] grounded: reran previous turn, rows=%d windowHours=%.0f", len(rs), prev.WindowHours)
			} else if rerr != nil {
				log.Printf("[ask prose] NOT grounded: %v", rerr)
			}
			answer, perr := emitProse(ctx, question, schema, prev, pcols, prows, psummary, "")
			if perr != nil {
				return askAIError(c, "could not answer: ", perr)
			}
			// B1 for prose: judge the answer; on MISMATCH regenerate once with the
			// problem as fixup (no second judge round — bounded cost, same stance as
			// the chart repair). No verdict or a failed regenerate delivers the
			// original answer — never a 502.
			if v, ok := verifyAskProse(ctx, question, answer, pcols, sampleRows(prows, 5)); ok && !v.MatchesIntent {
				if repaired, rerr := emitProse(ctx, question, schema, prev, pcols, prows, psummary, "verifier: "+v.Problem); rerr == nil && repaired != "" {
					answer = repaired
				}
			}
			return c.JSON(fiber.Map{"success": true, "data": fiber.Map{"answer": answer}})
		}
		if err != nil {
			return askAIError(c, "could not generate a query: ", err)
		}
		if clar := firstNonEmpty(emission.Clarification, spec.Clarification); clar != "" {
			// B3: the question is about factory data but under-specified — ask back
			// instead of guessing. No SQL ran, no chart to build.
			return c.JSON(fiber.Map{"success": true, "data": fiber.Map{"clarification": clar}})
		}

		if scope.canonical() {
			windowHours = spec.WindowHours
			from, to = windowFor(windowHours)
			if spec.From != nil && spec.To != nil {
				from, to = *spec.From, *spec.To
				windowHours = to.Sub(from).Hours()
			}
			bucketUsed = autoBucket(to.Sub(from))
			comp, cerr := cat.compile(spec.Spec, from, to, bucketUsed)
			if cerr != nil {
				// The spec named something outside the catalog. Hand the reason back —
				// the model can only recover by picking a metric that really exists.
				if attempt < 3 {
					fixup = &sqlFixup{SQL: specJSON(spec.Spec), Err: cerr.Error()}
					continue
				}
				return c.Status(400).JSON(fiber.Map{"success": false,
					"error": fiber.Map{"message": "could not compile the query: " + cerr.Error()},
					"spec":  specJSON(spec.Spec)})
			}
			sqlText, specText, bandCols = comp.SQL, specJSON(spec.Spec), comp.Band
			if comp.Tier == "catalog" {
				bucketUsed = "" // a listing has no resolution to report
			}
			cols, rows, err = runScopedIn(ctx, user.OrgId, scope.FactoryID, comp.SQL, comp.Args...)
			if err != nil {
				if attempt < 3 {
					fixup = &sqlFixup{SQL: specText, Err: err.Error()}
					continue
				}
				return c.Status(400).JSON(fiber.Map{"success": false,
					"error": fiber.Map{"message": "query failed: " + err.Error()}, "sql": sqlText})
			}
			// A compiled query is always aggregated and always limited, so the
			// truncation check below (needsBucketing) cannot apply here.
			break
		}

		sqlText, err = validateSQL(emission.SQL)
		if err != nil {
			if attempt < 3 {
				fixup = &sqlFixup{SQL: emission.SQL, Err: err.Error()}
				continue
			}
			return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "generated query rejected: " + err.Error()}, "sql": emission.SQL})
		}

		windowHours = emission.WindowHours
		from, to = windowFor(windowHours)
		// An absolute calendar range ("May 2026") overrides the relative window; report
		// its real span as windowHours so a later zoom re-anchors from what's on screen.
		if emission.From != nil && emission.To != nil {
			from, to = *emission.From, *emission.To
			windowHours = to.Sub(from).Hours()
		}
		runText, args := resolveSQL(sqlText, from, to)
		cols, rows, err = runScoped(ctx, user.OrgId, runText, args...)
		if err != nil {
			if attempt < 3 {
				fixup = &sqlFixup{SQL: sqlText, Err: err.Error()}
				continue
			}
			return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "query failed: " + err.Error()}, "sql": sqlText})
		}
		// A raw windowed series that hit the row cap only covers the window's opening
		// slice (ORDER BY ts + LIMIT). Re-emit forcing %BUCKET% so it spans the whole
		// window instead of the first ~maxRows readings. Reuses the fixup channel.
		if needsBucketing(strings.ToLower(sqlText), runText, len(rows)) && attempt < 3 {
			fixup = &sqlFixup{SQL: sqlText, Err: fmt.Sprintf(
				"returned the maximum %d rows and was truncated — a raw per-reading time series over this window only covers its opening slice. Rewrite as an aggregated series: time_bucket('%s', ts) AS bucket with avg()/min()/max() etc., GROUP BY bucket ORDER BY bucket, so it spans the WHOLE window.",
				maxRows, bucketToken)}
			continue
		}
		break
	}

	// Text-only results or empty results have no numeric axis — render as a table.
	// Empty option ({}) is the frontend's "table" signal; also skips a wasted provider call.
	// B2: one retry on a chart-generation error before degrading to the table — a
	// second failure still leaves option "{}" (HTTP 200, never a 502 here).
	option, caption := json.RawMessage("{}"), ""
	var analysis, nextQuestion string
	if len(rows) > 0 && hasNumericColumn(cols, rows) {
		bucket := bucketUsed
		if !scope.canonical() {
			bucket = chartBucket(sqlText, from, to)
		}
		summary := summarizeRows(cols, rows) // grounds the folded analysis in real per-machine stats
		// The min/max envelope stays out of the charting call: the model keeps
		// encoding the mean as before, and the frontend shades the band from the
		// columns it finds by name. Leaving them in would invite a third line.
		ccols, crows := hideColumns(cols, sampleRows(rows, 20), bandCols)
		ce, err := emitEChart(ctx, body.Question, ccols, crows, "", bucket, summary)
		if err != nil {
			ce, err = emitEChart(ctx, body.Question, ccols, crows, err.Error(), bucket, summary)
		}
		if err == nil {
			option, caption = sanitizeEChartOption(ce.Option, cols), ce.Caption
			analysis, nextQuestion = ce.Analysis, ce.NextQuestion
		}
	}

	// B1: judge gate on chart AND table turns (prose turns are free — no call; empty
	// results are skipped — nothing to judge beyond the SQL text). Call budget per
	// turn: SQL 1(-3 on retry) + chart 0-2 + judge 1 (~1s); worst case with the
	// judge's one repair round adds SQL 1 + chart 0-1 more — bounded by
	// askHandlerTimeout, and a repair failure degrades to the table signal, never a 502.
	if len(rows) > 0 {
		sqlText, cols, rows, option, caption, analysis, nextQuestion = verifyAndRepairAnswer(ctx, user.OrgId, question, sqlText, cols, rows, option, caption, analysis, nextQuestion, schema, prev, from, to)
	}

	return c.JSON(fiber.Map{"success": true, "data": fiber.Map{
		"sql":          sqlText,
		"spec":         specText, // canonical path: what a zoom or a saved board replays
		"columns":      cols,
		"rows":         rows,
		"echartOption": option,
		"caption":      caption,
		"analysis":     analysis,
		"nextQuestion": nextQuestion,
		"windowHours":  windowHours,
		"from":         from,
		"to":           to,
	}})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// rerunPrev re-executes the previous turn so a prose follow-up ("what does this
// chart show") is grounded in the same rows the user is looking at. A spec is
// recompiled; SQL text is re-validated and re-bound. Both go through the same
// read-only, scoped path as the original.
func rerunPrev(ctx context.Context, orgID string, scope askScope, cat *catalog, prev *prevTurn) ([]string, [][]any, error) {
	if prev == nil {
		return nil, nil, nil // nothing to ground on, not an error
	}
	from, to := windowFor(prev.WindowHours)

	if scope.canonical() && prev.Spec != "" {
		if cat == nil {
			return nil, nil, errors.New("no catalog loaded for this scope")
		}
		var spec querySpec
		if err := json.Unmarshal([]byte(prev.Spec), &spec); err != nil {
			return nil, nil, fmt.Errorf("previous spec unreadable: %w", err)
		}
		comp, err := cat.compile(spec, from, to, autoBucket(to.Sub(from)))
		if err != nil {
			return nil, nil, fmt.Errorf("previous spec no longer compiles: %w", err)
		}
		return runScopedIn(ctx, orgID, scope.FactoryID, comp.SQL, comp.Args...)
	}

	if prev.SQL == "" {
		return nil, nil, nil
	}
	s, err := validateSQL(prev.SQL)
	if err != nil {
		return nil, nil, fmt.Errorf("previous SQL failed validation: %w", err)
	}
	text, args := resolveSQL(s, from, to)
	return runScoped(ctx, orgID, text, args...)
}

// verifyAndRepairAnswer runs the B1 judge on a delivered answer — chart AND table
// turns — and, on a MISMATCH verdict, ONE repair round: re-emit SQL with the
// verifier's problem as a fixup, re-run it, and re-chart if the repaired result is
// chartable — no second judge call on the repaired result. If the repaired SQL
// fails to emit, validate, or run, or returns no rows, the ORIGINAL rows are kept
// (chart degraded to the table signal "{}") — the judge must never turn an already
// delivered answer into a 502.
func verifyAndRepairAnswer(ctx context.Context, orgID, question, sqlText string, cols []string, rows [][]any, option json.RawMessage, caption, analysis, nextQuestion, schema string, prev *prevTurn, from, to time.Time) (string, []string, [][]any, json.RawMessage, string, string, string) {
	v, ok := verifyAskAnswer(ctx, question, sqlText, cols, sampleRows(rows, 5), option)
	if !ok || v.MatchesIntent {
		return sqlText, cols, rows, option, caption, analysis, nextQuestion
	}

	emission, err := emitSQL(ctx, question, schema, prev, &sqlFixup{SQL: sqlText, Err: "verifier: " + v.Problem})
	if err != nil || emission.Clarification != "" || emission.SQL == "" {
		return sqlText, cols, rows, json.RawMessage("{}"), "", "", ""
	}
	repairedSQL, err := validateSQL(emission.SQL)
	if err != nil {
		return sqlText, cols, rows, json.RawMessage("{}"), "", "", ""
	}
	// The repair keeps the original turn's window — the judge flags what was asked,
	// not when, and a repair that silently moved the range would be its own bug.
	repairedText, repairedArgs := resolveSQL(repairedSQL, from, to)
	repairedCols, repairedRows, err := runScoped(ctx, orgID, repairedText, repairedArgs...)
	if err != nil || len(repairedRows) == 0 {
		return sqlText, cols, rows, json.RawMessage("{}"), "", "", ""
	}

	// Repaired rows accepted. Chart them if chartable; otherwise deliver as a table —
	// ponytail: no second judge round on the repaired result, by design (bounded cost).
	if !hasNumericColumn(repairedCols, repairedRows) {
		return repairedSQL, repairedCols, repairedRows, json.RawMessage("{}"), "", "", ""
	}
	ce, err := emitEChart(ctx, question, repairedCols, sampleRows(repairedRows, 20), "", chartBucket(repairedSQL, from, to), summarizeRows(repairedCols, repairedRows))
	if err != nil {
		return repairedSQL, repairedCols, repairedRows, json.RawMessage("{}"), "", "", ""
	}
	return repairedSQL, repairedCols, repairedRows, sanitizeEChartOption(ce.Option, repairedCols), ce.Caption, ce.Analysis, ce.NextQuestion
}

// RunSQL re-executes a stored query (board reopen → live data) and doubles as the
// zoom endpoint: pass an explicit from/to to re-run the SAME SQL over a narrower
// range, and the bucket token resolves finer automatically — no LLM call.
// Without from/to the window is windowHours back from now, so a saved board chart
// reopens on live data over the span it was created with. POST /ai/run-sql
// Re-validates even though the SQL came from our DB — cheap, and the guard is the point.
func RunSQL(c *fiber.Ctx) error {
	user := middleware.GetUser(c)
	if user == nil {
		return c.Status(401).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "unauthorized"}})
	}
	var body struct {
		SQL         string     `json:"sql"`
		Spec        string     `json:"spec"`      // canonical path: recompiled, not replayed
		FactoryID   string     `json:"factoryId"` // required with spec
		From        *time.Time `json:"from"`
		To          *time.Time `json:"to"`
		WindowHours float64    `json:"windowHours"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "sql is required"}})
	}
	from, to := windowFor(body.WindowHours)
	if body.From != nil && body.To != nil {
		if !body.To.After(*body.From) {
			return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "to must be after from"}})
		}
		from, to = *body.From, *body.To
	}
	ctx := context.Background()

	// Zooming a canonical chart RECOMPILES the stored spec at the new window, so it
	// also drops to a finer rollup tier automatically. Nothing here calls the model:
	// zoom stayed free, and a spec keeps working even when the relations change.
	if s := strings.TrimSpace(body.Spec); s != "" {
		if body.FactoryID == "" {
			return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "factoryId is required with spec"}})
		}
		var spec querySpec
		if err := json.Unmarshal([]byte(s), &spec); err != nil {
			return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "invalid spec: " + err.Error()}})
		}
		cat, err := loadCatalog(ctx, body.FactoryID)
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "unknown factory: " + err.Error()}})
		}
		bucket := autoBucket(to.Sub(from))
		comp, err := cat.compile(spec, from, to, bucket)
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "could not compile spec: " + err.Error()}})
		}
		cols, rows, err := runScopedIn(ctx, user.OrgId, body.FactoryID, comp.SQL, comp.Args...)
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "query failed: " + err.Error()}})
		}
		return c.JSON(fiber.Map{"success": true, "data": fiber.Map{
			"columns": cols, "rows": rows, "from": from, "to": to, "bucket": comp.Bucket}})
	}

	sqlText, err := validateSQL(body.SQL)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": err.Error()}})
	}
	runText, args := resolveSQL(sqlText, from, to)
	cols, rows, err := runScoped(ctx, user.OrgId, runText, args...)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": "query failed: " + err.Error()}})
	}
	return c.JSON(fiber.Map{"success": true, "data": fiber.Map{"columns": cols, "rows": rows, "from": from, "to": to}})
}
