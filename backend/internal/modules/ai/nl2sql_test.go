package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

// TestNeedsBucketing guards the raw-time-series truncation backstop: a windowed
// query with no time_bucket that hit the row cap must be flagged for re-emission,
// while bucketed series, listings, and under-cap results must not be.
func TestNeedsBucketing(t *testing.T) {
	const rawSeries = "select ts, (data->>'speed')::float as speed from v_telemetry where ts >= $1 and ts < $2 order by ts limit 5000"
	cases := []struct {
		name     string
		sqlLower string
		runText  string
		rowCount int
		want     bool
	}{
		{"raw windowed series truncated", rawSeries, rawSeries, maxRows, true},
		{"raw series under cap", rawSeries, rawSeries, maxRows - 1, false},
		{"bucketed series at cap", "select time_bucket('%bucket%', ts) as bucket, avg(x) from v_telemetry where ts >= $1 group by bucket", "select time_bucket('1 day', ts) as bucket, avg(x) from v_telemetry where ts >= $1 group by bucket", maxRows, false},
		{"distinct listing at cap", "select distinct data->>'sku' as sku from v_telemetry where ts >= $1", "select distinct data->>'sku' as sku from v_telemetry where ts >= $1", maxRows, false},
		{"unwindowed at cap", "select ts, x from v_telemetry order by ts", "select ts, x from v_telemetry order by ts", maxRows, false},
	}
	for _, tc := range cases {
		if got := needsBucketing(tc.sqlLower, tc.runText, tc.rowCount); got != tc.want {
			t.Errorf("%s: needsBucketing = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAskAIErrorMapsQuota verifies the shared /ask error mapper turns a provider
// quotaError into 429 QUOTA_EXCEEDED and leaves any other error a 502 — the same
// distinction Chat makes via errors.As on the 429 path.
func TestAskAIErrorMapsQuota(t *testing.T) {
	app := fiber.New()
	app.Get("/quota", func(c *fiber.Ctx) error { return askAIError(c, "x: ", &quotaError{}) })
	app.Get("/other", func(c *fiber.Ctx) error { return askAIError(c, "boom: ", fmt.Errorf("nope")) })

	q, err := app.Test(httptest.NewRequest("GET", "/quota", nil))
	if err != nil {
		t.Fatalf("test quota route: %v", err)
	}
	if q.StatusCode != 429 {
		t.Errorf("quota status = %d, want 429", q.StatusCode)
	}
	body, _ := io.ReadAll(q.Body)
	if !containsJSONCode(body, "QUOTA_EXCEEDED") {
		t.Errorf("quota body = %s, want code QUOTA_EXCEEDED", body)
	}

	o, err := app.Test(httptest.NewRequest("GET", "/other", nil))
	if err != nil {
		t.Fatalf("test other route: %v", err)
	}
	if o.StatusCode != 502 {
		t.Errorf("generic status = %d, want 502", o.StatusCode)
	}
}

func containsJSONCode(body []byte, code string) bool {
	var parsed struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	return parsed.Error.Code == code
}

func TestParseSQLEmission(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantSQL     string
		wantClarify string
		wantErr     error // non-nil: exact error to match with errors.Is; else just "any error"
		wantAnyErr  bool
	}{
		{
			name:        "clarification set",
			raw:         `{"answerable":true,"sql":"","clarification":"Which machine and metric?"}`,
			wantClarify: "Which machine and metric?",
		},
		{
			name:        "clarification wins over sql when both set",
			raw:         `{"answerable":true,"sql":"SELECT 1","clarification":"Which machine?"}`,
			wantClarify: "Which machine?",
		},
		{
			name:    "not answerable and no clarification -> errNotDataQuestion",
			raw:     `{"answerable":false,"sql":""}`,
			wantErr: errNotDataQuestion,
		},
		{
			name:    "answerable but empty sql and no clarification -> errNotDataQuestion",
			raw:     `{"answerable":true,"sql":""}`,
			wantErr: errNotDataQuestion,
		},
		{
			name:    "valid sql passes through",
			raw:     `{"answerable":true,"sql":"SELECT 1 FROM v_machines LIMIT 1"}`,
			wantSQL: "SELECT 1 FROM v_machines LIMIT 1",
		},
		{
			name:       "malformed JSON errors",
			raw:        `{not json`,
			wantAnyErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSQLEmission(c.raw)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("got err=%v, want errors.Is(err, %v)", err, c.wantErr)
				}
				return
			}
			if c.wantAnyErr {
				if err == nil {
					t.Fatalf("expected an error, got none (result=%+v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.SQL != c.wantSQL {
				t.Errorf("SQL = %q, want %q", got.SQL, c.wantSQL)
			}
			if got.Clarification != c.wantClarify {
				t.Errorf("Clarification = %q, want %q", got.Clarification, c.wantClarify)
			}
		})
	}
}

func TestHasNumericColumn(t *testing.T) {
	cases := []struct {
		name string
		cols []string
		rows [][]any
		want bool
	}{
		{"text only", []string{"name"}, [][]any{{"CW-01"}, {"CW-02"}}, false},
		{"int column", []string{"name", "count"}, [][]any{{"CW-01", int64(5)}}, true},
		{"float column", []string{"bucket", "avg"}, [][]any{{"09:00", 42.5}}, true},
		{"leading nils then int", []string{"n"}, [][]any{{nil}, {int64(3)}}, true},
		{"empty", []string{"name"}, nil, false},
	}
	for _, c := range cases {
		if got := hasNumericColumn(c.cols, c.rows); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestValidateSQL(t *testing.T) {
	ok := []string{
		`SELECT time_bucket('1 hour', ts) AS bucket, avg((data->>'speed')::float) AS avg_speed FROM v_telemetry WHERE machine_name = 'CW-01' GROUP BY bucket ORDER BY bucket LIMIT 500`,
		`select name, status from v_machines order by name limit 100;`, // trailing semicolon allowed
		`SELECT key, label, unit FROM v_machine_fields LIMIT 50`,
	}
	for _, s := range ok {
		if _, err := validateSQL(s); err != nil {
			t.Errorf("expected valid, got error: %v\n  sql: %s", err, s)
		}
	}

	bad := []string{
		`DELETE FROM v_telemetry`,                                             // write
		`SELECT 1; DROP TABLE users`,                                          // multi-statement
		`INSERT INTO v_machines VALUES (1)`,                                   // write
		`SELECT * FROM users`,                                                 // base table (cross-org leak)
		`SELECT * FROM telemetry_raw`,                                         // base table, bypasses org view
		`SELECT * FROM machines m, organizations o`,                           // comma join to base table
		`SELECT password_hash FROM v_machines JOIN users ON true`,             // base table in join
		`SELECT pg_sleep(10)`,                                                 // system function... actually via pg_ table? ensure denied
		`UPDATE v_machines SET name = 'x'`,                                    // write
		`WITH x AS (SELECT * FROM telemetry_raw) SELECT * FROM x`,             // base table inside CTE
	}
	for _, s := range bad {
		if _, err := validateSQL(s); err == nil {
			t.Errorf("expected rejection, got none for: %s", s)
		}
	}
}

func TestSanitizeEChartOption(t *testing.T) {
	cases := []struct {
		name      string
		option    json.RawMessage
		cols      []string
		checkFn   func(t *testing.T, result json.RawMessage)
		wantValid bool
	}{
		{
			name: "valid line series with matching encode",
			option: json.RawMessage(`{
				"title": {"text": "Test"},
				"series": [{"type": "line", "encode": {"x": "bucket", "y": "avg_speed"}}],
				"xAxis": {"type": "time"}
			}`),
			cols:       []string{"bucket", "avg_speed"},
			wantValid:  true,
			checkFn: func(t *testing.T, result json.RawMessage) {
				var m map[string]any
				if err := json.Unmarshal(result, &m); err != nil {
					t.Errorf("result should be valid JSON: %v", err)
					return
				}
				if m == nil || m["series"] == nil {
					t.Error("expected series in result")
					return
				}
				series, ok := m["series"].([]any)
				if !ok || len(series) == 0 {
					t.Error("expected non-empty series array")
					return
				}
				s, ok := series[0].(map[string]any)
				if !ok {
					t.Error("expected first series to be object")
					return
				}
				if s["type"] != "line" {
					t.Errorf("expected series type 'line', got %v", s["type"])
				}
				if s["data"] != nil {
					t.Error("expected series.data to be removed")
				}
			},
		},
		{
			name: "duplicate series (same type+encode, per-machine names) collapse to one",
			option: json.RawMessage(`{
				"series": [
					{"type": "line", "name": "CW-01", "encode": {"x": "bucket", "y": "avg_speed"}},
					{"type": "line", "name": "CB-01", "encode": {"x": "bucket", "y": "avg_speed"}}
				]
			}`),
			cols:      []string{"bucket", "machine_name", "avg_speed"},
			wantValid: true,
			checkFn: func(t *testing.T, result json.RawMessage) {
				var m map[string]any
				if err := json.Unmarshal(result, &m); err != nil {
					t.Errorf("result should be valid JSON: %v", err)
					return
				}
				series, _ := m["series"].([]any)
				if len(series) != 1 {
					t.Errorf("expected duplicate series collapsed to 1, got %d", len(series))
				}
			},
		},
		{
			name: "distinct series (different encode y) both kept",
			option: json.RawMessage(`{
				"series": [
					{"type": "line", "encode": {"x": "bucket", "y": "avg_speed"}},
					{"type": "line", "encode": {"x": "bucket", "y": "max_speed"}}
				]
			}`),
			cols:      []string{"bucket", "avg_speed", "max_speed"},
			wantValid: true,
			checkFn: func(t *testing.T, result json.RawMessage) {
				var m map[string]any
				if err := json.Unmarshal(result, &m); err != nil {
					t.Errorf("result should be valid JSON: %v", err)
					return
				}
				series, _ := m["series"].([]any)
				if len(series) != 2 {
					t.Errorf("expected both distinct series kept, got %d", len(series))
				}
			},
		},
		{
			name: "dataset and series.data stripped",
			option: json.RawMessage(`{
				"dataset": {"source": [[1, 2], [3, 4]]},
				"series": [{"type": "bar", "data": [1, 2, 3], "encode": {"x": "col1", "y": "col2"}}],
				"xAxis": {"type": "category"}
			}`),
			cols:       []string{"col1", "col2"},
			wantValid:  true,
			checkFn: func(t *testing.T, result json.RawMessage) {
				var m map[string]any
				json.Unmarshal(result, &m)
				if m["dataset"] != nil {
					t.Error("expected dataset to be removed")
				}
				series := m["series"].([]any)
				if series[0].(map[string]any)["data"] != nil {
					t.Error("expected series.data to be removed")
				}
			},
		},
		{
			name: "valid heatmap series with x/y/value encode survives",
			option: json.RawMessage(`{
				"series": [{"type": "heatmap", "encode": {"x": "hour", "y": "machine_name", "value": "avg_speed"}}],
				"visualMap": {"inRange": {"color": ["#22c55e", "#ef4444"]}},
				"xAxis": {"type": "category"}, "yAxis": {"type": "category"}
			}`),
			cols:      []string{"hour", "machine_name", "avg_speed"},
			wantValid: true,
			checkFn: func(t *testing.T, result json.RawMessage) {
				var m map[string]any
				if err := json.Unmarshal(result, &m); err != nil {
					t.Errorf("result should be valid JSON: %v", err)
					return
				}
				if m["visualMap"] == nil {
					t.Error("expected visualMap to survive sanitize")
				}
				series, _ := m["series"].([]any)
				if len(series) != 1 || series[0].(map[string]any)["type"] != "heatmap" {
					t.Errorf("expected one heatmap series, got %v", m["series"])
				}
			},
		},
		{
			name:      "heatmap with encode value on missing column rejected",
			option:    json.RawMessage(`{"series": [{"type": "heatmap", "encode": {"x": "hour", "y": "machine_name", "value": "missing"}}]}`),
			cols:      []string{"hour", "machine_name"},
			wantValid: false,
		},
		{
			name:      "unknown series type rejected",
			option:    json.RawMessage(`{"series": [{"type": "radar"}]}`),
			cols:      []string{},
			wantValid: false,
		},
		{
			name: "encode referencing missing column rejected",
			option: json.RawMessage(`{
				"series": [{"type": "line", "encode": {"x": "bucket", "y": "missing_column"}}]
			}`),
			cols:      []string{"bucket"},
			wantValid: false,
		},
		{
			name:      "invalid JSON returns empty object",
			option:    json.RawMessage(`{invalid json}`),
			cols:      []string{},
			wantValid: false,
		},
		{
			name: "encode with array values",
			option: json.RawMessage(`{
				"series": [{"type": "bar", "encode": {"x": "category", "y": ["value1", "value2"]}}]
			}`),
			cols:       []string{"category", "value1", "value2"},
			wantValid:  true,
			checkFn: func(t *testing.T, result json.RawMessage) {
				var m map[string]any
				if err := json.Unmarshal(result, &m); err != nil {
					t.Errorf("result should be valid: %v", err)
				}
				// Just verify it parses; the main test is that it doesn't return "{}"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := sanitizeEChartOption(tc.option, tc.cols)
			if tc.wantValid {
				if string(result) == "{}" {
					t.Error("expected valid option, got empty object")
				}
				if tc.checkFn != nil {
					tc.checkFn(t, result)
				}
			} else {
				if string(result) != "{}" {
					t.Errorf("expected empty object for invalid input, got: %s", string(result))
				}
			}
		})
	}
}

// Zoom contract: the bucket must get finer as the window narrows, stay under the
// point budget, and the SQL must only pick up bind args when it actually has $1.
func TestAutoBucketAndResolveSQL(t *testing.T) {
	windows := []struct {
		window time.Duration
		want   string
	}{
		{365 * 24 * time.Hour, "1 day"},
		{30 * 24 * time.Hour, "6 hours"},
		{7 * 24 * time.Hour, "1 hour"},
		{24 * time.Hour, "5 minutes"},
		{2 * time.Hour, "1 minute"},
	}
	for _, w := range windows {
		if got := autoBucket(w.window); got != w.want {
			t.Errorf("autoBucket(%v) = %q, want %q", w.window, got, w.want)
		}
	}

	// Whatever the window, the bucket must keep the result chartable.
	for _, h := range []float64{1, 24, 168, 720, 8760, 5 * 365 * 24} {
		from, to := windowFor(h)
		label := autoBucket(to.Sub(from))
		var step time.Duration
		for _, b := range bucketLadder {
			if b.label == label {
				step = b.step
			}
		}
		if points := to.Sub(from) / step; points > targetPoints {
			t.Errorf("window %vh → %s yields %d points, over the %d budget", h, label, points, targetPoints)
		}
	}

	from, to := windowFor(24)
	tmpl := "SELECT time_bucket('%BUCKET%', ts) FROM v_telemetry WHERE ts >= $1 AND ts < $2"
	gotSQL, args := resolveSQL(tmpl, from, to)
	if strings.Contains(gotSQL, bucketToken) {
		t.Errorf("bucket token survived resolution: %s", gotSQL)
	}
	if len(args) != 2 {
		t.Errorf("windowed SQL got %d args, want 2", len(args))
	}

	// A model that writes a literal interval (user insisting on "real 1-minute data
	// over a year") is normalized back to the token — the server owns the bucket.
	got, err := validateSQL("SELECT time_bucket( '1 minute',  ts) FROM v_telemetry WHERE ts >= $1")
	if err != nil {
		t.Fatalf("validateSQL: %v", err)
	}
	if !strings.Contains(got, bucketToken) {
		t.Errorf("literal interval not normalized: %s", got)
	}
	if b := chartBucket(got, to.Add(-365*24*time.Hour), to); b != "1 day" {
		t.Errorf("chartBucket over a year = %q, want %q", b, "1 day")
	}

	// A listing query has no window — it must run with no args, or pgx errors out.
	if _, args := resolveSQL("SELECT DISTINCT data->>'sku' FROM v_telemetry", from, to); args != nil {
		t.Errorf("param-free SQL got args %v, want none", args)
	}

	// A hallucinated window falls back to the default rather than scanning everything.
	if f, tt := windowFor(-5); tt.Sub(f) != defaultWindowHours*time.Hour {
		t.Errorf("windowFor(-5) spans %v, want %vh", tt.Sub(f), defaultWindowHours)
	}
}

// TestSummarizeRowsKeepsExtremes: the summary is computed over EVERY row, so a spike on
// a row the 40-point sample would skip must still surface as the max — the whole reason
// the analyze path sends a summary instead of only a thinned sample.
func TestSummarizeRowsKeepsExtremes(t *testing.T) {
	cols := []string{"bucket", "avg_speed"}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var rows [][]any
	for i := 0; i < 365; i++ {
		rows = append(rows, []any{base.AddDate(0, 0, i), 100.0})
	}
	rows[201][1] = 999.0 // a spike downsampleRows(_, 40) does not land on

	if s := downsampleRows(rows, 40); containsSpeed(s, 999.0) {
		t.Fatalf("test premise broken: the sample happened to include the spike")
	}
	got := summarizeRows(cols, rows)
	if !strings.Contains(got, "max=999") {
		t.Fatalf("summary lost the spike:\n%s", got)
	}
	if !strings.Contains(got, "min=100") {
		t.Fatalf("summary lost the baseline min:\n%s", got)
	}
}

// TestSummarizeRowsPerCategory: with a category column the stats are broken down per
// value, so machine B's spike shows under B — not blended into a single global line.
func TestSummarizeRowsPerCategory(t *testing.T) {
	cols := []string{"bucket", "machine_name", "avg_speed"}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var rows [][]any
	for i := 0; i < 100; i++ {
		rows = append(rows, []any{base.AddDate(0, 0, i), "Checkweigher CW-01", 100.0})
		rows = append(rows, []any{base.AddDate(0, 0, i), "Filler FL-03", 200.0})
	}
	rows[41][2] = 999.0 // a spike on Filler FL-03

	got := summarizeRows(cols, rows)
	if !strings.Contains(got, "by machine_name") {
		t.Fatalf("expected a per-category breakdown:\n%s", got)
	}
	if !strings.Contains(got, "Filler FL-03: min=200 (") || !strings.Contains(got, "max=999") {
		t.Fatalf("Filler FL-03 spike not reported under its own line:\n%s", got)
	}
	// CW-01 must keep its own range, unblended by the other machine's spike.
	if !strings.Contains(got, "Checkweigher CW-01: min=100 (") || !strings.Contains(got, "Checkweigher CW-01: min=100 (2026-01-01T00:00:00Z), max=100") {
		t.Fatalf("CW-01 stats blended with the other machine:\n%s", got)
	}
}

// TestCleanChartTextsRecoversLeak: when a model bleeds tool-format XML tags into the
// analysis value (and drops nextQuestion into it), the analysis is cut at the tag and
// the nextQuestion is recovered — the user never sees raw markup.
func TestCleanChartTextsRecoversLeak(t *testing.T) {
	leaked := chartEmission{
		Caption:  "ความเร็วเฉลี่ยรายเดือน",
		Analysis: `Checkweigher CW-01 เฉลี่ย 62.6 ต่ำสุด มิ.ย. 2026 ส่วน CB-01 เฉลี่ย 1027.8</analysis>` + "\n" + `<parameter name="nextQuestion">ขอดูความเร็วรายนาที CB-01 พ.ย. 2025 ได้ไหม`,
	}
	got := cleanChartTexts(leaked)
	if strings.ContainsAny(got.Analysis, "<>") {
		t.Fatalf("analysis still has markup: %q", got.Analysis)
	}
	if !strings.Contains(got.Analysis, "1027.8") || strings.Contains(got.Analysis, "parameter") {
		t.Fatalf("analysis not cleanly cut: %q", got.Analysis)
	}
	if got.NextQuestion != "ขอดูความเร็วรายนาที CB-01 พ.ย. 2025 ได้ไหม" {
		t.Fatalf("nextQuestion not recovered: %q", got.NextQuestion)
	}

	// A clean emission must pass through untouched.
	clean := chartEmission{Caption: "c", Analysis: "ทุกอย่างปกติ", NextQuestion: "ดูเครื่องอื่นไหม"}
	if out := cleanChartTexts(clean); out.Analysis != "ทุกอย่างปกติ" || out.NextQuestion != "ดูเครื่องอื่นไหม" {
		t.Fatalf("clean emission altered: %+v", out)
	}
}

// TestUnwrapJSONString guards the shape emit_echart_option's `option` arrives in.
// It is declared as a JSON string because Gemini leaves a schema-less object empty,
// but Claude and OpenAI may still answer with a real object — both must survive, and
// anything unparseable must be handed on for sanitizeEChartOption to reject.
func TestUnwrapJSONString(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"json string (gemini)", `"{\"series\":[{\"type\":\"line\"}]}"`, `{"series":[{"type":"line"}]}`},
		{"bare object (claude/openai)", `{"series":[{"type":"line"}]}`, `{"series":[{"type":"line"}]}`},
		{"string that isn't json", `"not an option"`, `not an option`},
		{"empty", ``, ``},
		{"json null", `null`, `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(unwrapJSONString(json.RawMessage(c.in))); got != c.want {
				t.Fatalf("unwrapJSONString(%s) = %s, want %s", c.in, got, c.want)
			}
		})
	}

	// End to end with the real validator: the unwrapped string must survive sanitizing
	// with its series intact — that is the whole point of the change.
	cols := []string{"bucket", "room_temp"}
	wrapped := json.RawMessage(`"{\"series\":[{\"type\":\"line\",\"encode\":{\"x\":\"bucket\",\"y\":\"room_temp\"}}]}"`)
	out := sanitizeEChartOption(unwrapJSONString(wrapped), cols)
	if string(out) == "{}" {
		t.Fatalf("sanitize dropped an option that came in as a JSON string: %s", out)
	}
	// And a broken one still fails closed.
	if got := sanitizeEChartOption(unwrapJSONString(json.RawMessage(`"{oops"`)), cols); string(got) != "{}" {
		t.Fatalf("malformed option should degrade to {}, got %s", got)
	}
}

// containsSpeed reports whether any row's last column equals v — a tiny test helper.
func containsSpeed(rows [][]any, v float64) bool {
	for _, r := range rows {
		if f, ok := r[len(r)-1].(float64); ok && f == v {
			return true
		}
	}
	return false
}
