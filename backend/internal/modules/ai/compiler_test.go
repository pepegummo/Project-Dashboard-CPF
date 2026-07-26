package ai

// Eval gate for the query compiler. These run offline (no DB, no model) because
// the property being protected is structural: for a given metric kind, the SQL
// must carry exactly one aggregation, and it must be the right one. A prompt
// regression cannot be caught by a unit test; this can.

import (
	"strings"
	"testing"
	"time"
)

// testCatalog mirrors the NJ5 registry: one cumulative counter with two labels,
// two gauges, one event flag.
func testCatalog() *catalog {
	return &catalog{
		FactoryID:   "00000000-0000-0000-0001-000000004046",
		FactoryName: "Nongjok5",
		Timezone:    "Asia/Bangkok",
		Machines: map[string][]string{
			"IQF":     {"IQF1", "IQF2", "IQF3", "IQF4"},
			"Packing": {"Packing1", "Packing2"},
		},
		Metrics: []catalogMetric{
			{FieldKey: "produced_count", Kind: "counter", Unit: "pieces", Types: []string{"IQF", "Packing"}},
			{FieldKey: "evap_temp", Kind: "gauge", Unit: "°C", Types: []string{"IQF"}},
			{FieldKey: "freezing_time", Kind: "gauge", Unit: "s", Types: []string{"IQF"}},
			{FieldKey: "network_drop", Kind: "event", Types: []string{"IQF"}},
		},
		Labels: map[string][]string{
			"area": {"InFeed_IQF2", "OutFeed_IQF2"},
			"sku":  {"Carb", "Soup Box", "Tray"},
		},
		Span: [2]time.Time{time.Now().AddDate(-1, 0, 0), time.Now()},
	}
}

func mustCompile(t *testing.T, spec querySpec, hours float64) *compiled {
	t.Helper()
	from, to := windowFor(hours)
	got, err := testCatalog().compile(spec, from, to, autoBucket(to.Sub(from)))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return got
}

func TestCompilerCounterUsesDeltaNeverSum(t *testing.T) {
	got := mustCompile(t, querySpec{
		Shape:   "timeseries",
		Metrics: []specMetric{{Field: "produced_count", Machine: "IQF2", Labels: map[string]string{"area": "OutFeed*"}}},
	}, 168)

	if !strings.Contains(got.SQL, "SUM(GREATEST(t.last_v - t.first_v, 0))") {
		t.Errorf("counter must aggregate as a clamped delta, got:\n%s", got.SQL)
	}
	// The failure this guards against: summing or averaging a cumulative counter,
	// which yields a number in the billions that looks plausible on a chart.
	for _, wrong := range []string{"SUM(t.sum_v)", "AVG(t.value)", "SUM(t.value)"} {
		if strings.Contains(got.SQL, wrong) {
			t.Errorf("counter must never use %s, got:\n%s", wrong, got.SQL)
		}
	}
}

func TestCompilerGaugeIsWeightedNotAvgOfAvg(t *testing.T) {
	got := mustCompile(t, querySpec{
		Shape:   "timeseries",
		Metrics: []specMetric{{Field: "evap_temp", Machine: "IQF2"}},
	}, 168)

	if !strings.Contains(got.SQL, "SUM(t.sum_v)") || !strings.Contains(got.SQL, "NULLIF(SUM(t.n)") {
		t.Errorf("gauge must be sum/count (weighted), got:\n%s", got.SQL)
	}
	if strings.Contains(got.SQL, "GREATEST") {
		t.Errorf("gauge must never use a counter delta, got:\n%s", got.SQL)
	}
}

func TestCompilerGaugeMinMaxHonoured(t *testing.T) {
	for agg, want := range map[string]string{"min": "MIN(t.min_v)", "max": "MAX(t.max_v)"} {
		got := mustCompile(t, querySpec{
			Shape:   "timeseries",
			Metrics: []specMetric{{Field: "evap_temp", Machine: "IQF2", Agg: agg}},
		}, 168)
		if !strings.Contains(got.SQL, want) {
			t.Errorf("agg %q should compile to %s, got:\n%s", agg, want, got.SQL)
		}
	}
}

func TestCompilerEventSums(t *testing.T) {
	got := mustCompile(t, querySpec{
		Shape:   "timeseries",
		Metrics: []specMetric{{Field: "network_drop", Machine: "IQF3"}},
	}, 168)
	if !strings.Contains(got.SQL, "SUM(t.sum_v)") {
		t.Errorf("event metric must sum occurrences, got:\n%s", got.SQL)
	}
}

// An agg hint on a counter must be ignored: a counter has exactly one correct
// aggregation, so a model that asks for "max produced_count" gets the delta.
func TestCompilerIgnoresAggHintOnCounter(t *testing.T) {
	got := mustCompile(t, querySpec{
		Shape:   "timeseries",
		Metrics: []specMetric{{Field: "produced_count", Agg: "max"}},
	}, 168)
	if !strings.Contains(got.SQL, "SUM(GREATEST(t.last_v - t.first_v, 0))") {
		t.Errorf("counter must ignore the agg hint, got:\n%s", got.SQL)
	}
}

func TestCompilerTierFollowsBucket(t *testing.T) {
	cases := []struct {
		hours    float64
		wantRel  string
		wantTier string
	}{
		{2, "v_readings", "raw"},       // 2h  -> 1 minute buckets
		{168, "readings_1h", "hourly"}, // 7d  -> 1 hour
		{8760, "readings_1d", "daily"}, // 1y  -> 1 day or coarser
	}
	for _, tc := range cases {
		got := mustCompile(t, querySpec{
			Shape:   "timeseries",
			Metrics: []specMetric{{Field: "evap_temp"}},
		}, tc.hours)
		if got.Tier != tc.wantTier || !strings.Contains(got.SQL, " "+tc.wantRel+" t ") {
			t.Errorf("%.0fh: want tier %s on %s, got tier %s:\n%s",
				tc.hours, tc.wantTier, tc.wantRel, got.Tier, got.SQL)
		}
	}
}

// Raw readings include flagged values (sentinels, out-of-range); the rollups
// already exclude them in their definition, so only the raw tier may filter.
func TestCompilerRawTierFiltersQuality(t *testing.T) {
	raw := mustCompile(t, querySpec{Shape: "timeseries", Metrics: []specMetric{{Field: "freezing_time"}}}, 2)
	if !strings.Contains(raw.SQL, "t.quality = 0") {
		t.Errorf("raw tier must exclude flagged readings, got:\n%s", raw.SQL)
	}
	rolled := mustCompile(t, querySpec{Shape: "timeseries", Metrics: []specMetric{{Field: "freezing_time"}}}, 168)
	if strings.Contains(rolled.SQL, "quality") {
		t.Errorf("rollup tier must not re-filter quality, got:\n%s", rolled.SQL)
	}
}

// Two metrics in one spec is how a correlation question is answered: one bucket
// column plus two numeric columns, which the frontend draws on a dual axis.
func TestCompilerTwoMetricsOneQuery(t *testing.T) {
	got := mustCompile(t, querySpec{
		Shape: "timeseries",
		Metrics: []specMetric{
			{Field: "produced_count", Machine: "IQF2", Labels: map[string]string{"area": "OutFeed*"}},
			{Field: "evap_temp", Machine: "IQF2"},
		},
	}, 168)
	if !strings.Contains(got.SQL, `AS "produced_count"`) || !strings.Contains(got.SQL, `AS "evap_temp"`) {
		t.Errorf("both metrics must be columns, got:\n%s", got.SQL)
	}
	if strings.Count(got.SQL, "FILTER (WHERE") < 2 {
		t.Errorf("each metric needs its own row filter, got:\n%s", got.SQL)
	}
	if !strings.Contains(got.SQL, "GROUP BY 1") || strings.Contains(got.SQL, "GROUP BY 1, 2") {
		t.Errorf("only the bucket is a dimension here, got:\n%s", got.SQL)
	}
}

func TestCompilerBreakdownGroupsDimensionsOnly(t *testing.T) {
	got := mustCompile(t, querySpec{
		Shape:     "timeseries",
		Metrics:   []specMetric{{Field: "produced_count"}},
		Breakdown: []string{"sku"},
	}, 168)
	if !strings.Contains(got.SQL, "GROUP BY 1, 2") {
		t.Errorf("bucket + sku are the dimensions, got:\n%s", got.SQL)
	}
	if strings.Contains(got.SQL, "GROUP BY 1, 2, 3") {
		t.Errorf("the metric column must not be grouped, got:\n%s", got.SQL)
	}
}

func TestCompilerBarRanksByMetricAndAppliesTopN(t *testing.T) {
	got := mustCompile(t, querySpec{
		Shape:     "bar",
		Metrics:   []specMetric{{Field: "produced_count"}},
		Breakdown: []string{"sku"},
		TopN:      10,
	}, 720)
	if strings.Contains(got.SQL, "time_bucket") {
		t.Errorf("a bar has no time dimension, got:\n%s", got.SQL)
	}
	if !strings.Contains(got.SQL, "ORDER BY 2 DESC NULLS LAST") {
		t.Errorf("a bar ranks by its metric, got:\n%s", got.SQL)
	}
	if !strings.HasSuffix(got.SQL, "LIMIT 10") {
		t.Errorf("top_n must cap the rows, got:\n%s", got.SQL)
	}
}

func TestCompilerUnknownMetricIsRejected(t *testing.T) {
	from, to := windowFor(24)
	if _, err := testCatalog().compile(querySpec{
		Shape:   "timeseries",
		Metrics: []specMetric{{Field: "count_ng"}},
	}, from, to, "1 hour"); err == nil {
		t.Fatal("a metric outside the catalog must not compile — that is how 'this factory has no reject data' is enforced")
	}
}

func TestCompilerUnknownLabelIsDroppedNotInterpolated(t *testing.T) {
	got := mustCompile(t, querySpec{
		Shape:   "timeseries",
		Metrics: []specMetric{{Field: "evap_temp", Labels: map[string]string{"batch_id": "'; DROP TABLE readings; --"}}},
	}, 168)
	if strings.Contains(got.SQL, "DROP") || strings.Contains(got.SQL, "batch_id") {
		t.Errorf("an unknown label must be ignored, never interpolated, got:\n%s", got.SQL)
	}
}

// Nothing the model wrote may reach the SQL text: machine names and label values
// are bind parameters, so a hostile value ends up as data, not syntax.
func TestCompilerBindsModelSuppliedValues(t *testing.T) {
	got := mustCompile(t, querySpec{
		Shape: "timeseries",
		Metrics: []specMetric{{
			Field:   "produced_count",
			Machine: "IQF2'; DROP TABLE readings; --",
			Labels:  map[string]string{"sku": "Carb*"},
		}},
	}, 168)
	if strings.Contains(got.SQL, "DROP") || strings.Contains(got.SQL, "IQF2") || strings.Contains(got.SQL, "Carb") {
		t.Errorf("values must be bound, not interpolated, got:\n%s", got.SQL)
	}
	var sawMachine, sawSKU bool
	for _, a := range got.Args {
		s, _ := a.(string)
		if strings.Contains(s, "IQF2") {
			sawMachine = true
		}
		if s == "Carb%" {
			sawSKU = true // trailing * became a prefix match
		}
	}
	if !sawMachine || !sawSKU {
		t.Errorf("machine and label patterns must appear as args, got %#v", got.Args)
	}
}

func TestCompilerListNeedsNoWindow(t *testing.T) {
	for _, what := range []string{"machine", "metric", "sku"} {
		got, err := testCatalog().compile(querySpec{Shape: "list", Breakdown: []string{what}},
			time.Time{}, time.Time{}, "")
		if err != nil {
			t.Fatalf("list %s: %v", what, err)
		}
		if strings.Contains(got.SQL, "time_bucket") || strings.Contains(got.SQL, ">=") {
			t.Errorf("a listing has no time window, got:\n%s", got.SQL)
		}
		if !strings.Contains(got.SQL, "v_series") {
			t.Errorf("a listing reads the catalog view, got:\n%s", got.SQL)
		}
	}
}

// Every compiled query must be scoped and capped without the model's help.
func TestCompilerAlwaysScopesAndLimits(t *testing.T) {
	got := mustCompile(t, querySpec{Shape: "timeseries", Metrics: []specMetric{{Field: "evap_temp"}}}, 168)
	if !strings.Contains(got.SQL, "vs.factory_id = $1::uuid") {
		t.Errorf("query must be factory-scoped, got:\n%s", got.SQL)
	}
	// The window is inlined on purpose (see tsLiteral): a bound $1/$2 costs chunk
	// exclusion and turns a sub-second query into a minute-long one.
	if !strings.Contains(got.SQL, "TIMESTAMPTZ '") {
		t.Errorf("window must be a literal so chunks can be excluded, got:\n%s", got.SQL)
	}
	if !strings.Contains(got.SQL, "LIMIT") {
		t.Errorf("query must be capped, got:\n%s", got.SQL)
	}
}

func TestSpecEmissionParsing(t *testing.T) {
	em, err := parseSpecEmission(`{"answerable":true,"shape":"timeseries",
		"metrics":[{"field":"produced_count","machine":"IQF2"}],"window":{"hours":168}}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if em.WindowHours != 168 || len(em.Spec.Metrics) != 1 || em.Spec.Metrics[0].Field != "produced_count" {
		t.Fatalf("unexpected emission: %+v", em)
	}

	// An absolute range wins over hours and reports its real span, so a later zoom
	// re-anchors on what is on screen.
	em, err = parseSpecEmission(`{"answerable":true,"shape":"bar","metrics":[{"field":"produced_count"}],
		"window":{"from":"2026-05-01","to":"2026-06-01"}}`)
	if err != nil {
		t.Fatalf("parse absolute: %v", err)
	}
	if em.From == nil || em.To == nil || em.WindowHours != 744 {
		t.Fatalf("absolute range not honoured: %+v", em)
	}

	if _, err := parseSpecEmission(`{"answerable":false,"metrics":[]}`); err != errNotDataQuestion {
		t.Fatalf("unanswerable spec should route to prose, got %v", err)
	}

	em, err = parseSpecEmission(`{"answerable":true,"metrics":[],"clarification":"เครื่องไหน?"}`)
	if err != nil || em.Clarification == "" {
		t.Fatalf("clarification should survive: %+v (%v)", em, err)
	}
}

// A gauge mean ships its min/max envelope so the chart can shade the spread: an hour
// that ran at −35 and defrosted to +25 averages to +1.6, which never happened.
func TestGaugeMeanCarriesMinMaxEnvelope(t *testing.T) {
	got := mustCompile(t, querySpec{Shape: "timeseries",
		Metrics: []specMetric{{Field: "evap_temp", Machine: "IQF2"}}}, 168)
	for _, want := range []string{`MIN(t.min_v) FILTER`, `MAX(t.max_v) FILTER`, `"evap_temp_min"`, `"evap_temp_max"`} {
		if !strings.Contains(got.SQL, want) {
			t.Errorf("expected %s in:\n%s", want, got.SQL)
		}
	}
	if len(got.Band) != 2 {
		t.Errorf("Band must name the envelope columns so they can be hidden from the chart model, got %v", got.Band)
	}

	// Not for a counter (a delta has no meaningful envelope), not for an explicit
	// min/max (that IS the extreme), and not for a bar (no spread to shade).
	for _, spec := range []querySpec{
		{Shape: "timeseries", Metrics: []specMetric{{Field: "produced_count"}}},
		{Shape: "timeseries", Metrics: []specMetric{{Field: "evap_temp", Agg: "max"}}},
		{Shape: "bar", Metrics: []specMetric{{Field: "evap_temp"}}, Breakdown: []string{"machine"}},
	} {
		if c := mustCompile(t, spec, 168); len(c.Band) != 0 {
			t.Errorf("%s/%s should carry no envelope, got %v", spec.Shape, spec.Metrics[0].Field, c.Band)
		}
	}

	// Raw readings have no min_v/max_v to aggregate — every point is already the value.
	if c := mustCompile(t, querySpec{Shape: "timeseries",
		Metrics: []specMetric{{Field: "evap_temp"}}}, 2); len(c.Band) != 0 || c.Tier != "raw" {
		t.Errorf("raw tier needs no envelope, got tier=%s band=%v", c.Tier, c.Band)
	}
}

// hideColumns keeps the column list and its rows in step — a mismatch would hand the
// charting model values under the wrong headings.
func TestHideColumnsDropsColumnsAndCells(t *testing.T) {
	cols := []string{"bucket", "evap_temp", "evap_temp_min", "evap_temp_max"}
	rows := [][]any{{"t0", 1.5, -35.8, 25.2}, {"t1", 2.5, -36.0, 24.0}}
	gotCols, gotRows := hideColumns(cols, rows, []string{"evap_temp_min", "evap_temp_max"})
	if len(gotCols) != 2 || gotCols[1] != "evap_temp" {
		t.Fatalf("columns: %v", gotCols)
	}
	for i, r := range gotRows {
		if len(r) != 2 || r[1] != rows[i][1] {
			t.Fatalf("row %d: %v", i, r)
		}
	}
	if c, r := hideColumns(cols, rows, nil); len(c) != 4 || len(r[0]) != 4 {
		t.Fatalf("nothing to hide must pass through untouched: %v %v", c, r)
	}
}

// A model-written query must not be able to read a rollup directly: continuous
// aggregates are materialized separately, so RLS on `readings` does not filter them.
// The compiler's own SQL is server-authored and bypasses this validator.
func TestRollupsAreNotReachableByGeneratedSQL(t *testing.T) {
	for _, rel := range []string{"readings_1h", "readings_1d", "readings", "series"} {
		if _, err := validateSQL("SELECT bucket FROM " + rel + " LIMIT 10"); err == nil {
			t.Errorf("%s must not be queryable by a generated statement", rel)
		}
	}
	// The two views that DO enforce factory scoping stay reachable for the fallback.
	for _, rel := range []string{"v_readings", "v_series"} {
		if _, err := validateSQL("SELECT * FROM " + rel + " LIMIT 10"); err != nil {
			t.Errorf("%s should stay queryable: %v", rel, err)
		}
	}
}
