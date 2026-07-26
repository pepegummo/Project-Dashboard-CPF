package ai

// Query-spec compiler: a spec from the model plus the catalog becomes SQL here.
//
// The point is that the model cannot choose an aggregation. On the raw-SQL path a
// prompt has to ASK for MAX-MIN on a counter and AVG on a gauge, and nothing
// checks that it complied — apply the wrong one and the answer is confidently
// wrong. Here the spec names a field, the catalog says what kind of thing that
// field is, and aggSQL decides the math. Getting it wrong is not expressible.
//
// Everything that varies with the question (machine, label patterns, field keys,
// window, bucket) is a bind parameter, so nothing the model produced is ever
// interpolated into SQL text. The only interpolated parts are chosen from fixed
// server-side sets: which relation to read and which aggregate to apply.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// specMetric is one series selection: which metric, on which machine, narrowed by
// labels. Two metrics in one timeseries spec is how a correlation is asked for.
type specMetric struct {
	Field       string            `json:"field"`
	Machine     string            `json:"machine"`
	MachineType string            `json:"machine_type"`
	Labels      map[string]string `json:"labels"`
	Agg         string            `json:"agg"` // gauge only: avg (default) | min | max
}

type specWindow struct {
	Hours float64 `json:"hours"`
	From  string  `json:"from"`
	To    string  `json:"to"`
}

type querySpec struct {
	Answerable    bool         `json:"answerable"`
	Clarification string       `json:"clarification"`
	Shape         string       `json:"shape"` // timeseries | bar | table | list
	Metrics       []specMetric `json:"metrics"`
	Breakdown     []string     `json:"breakdown"` // label keys, or "machine"/"metric" for list
	Window        specWindow   `json:"window"`
	TopN          int          `json:"top_n"`
}

// compiled is what runs: SQL plus its bind args, and the resolution actually used
// so the caption can say so and a zoom can recompile at a finer one.
type compiled struct {
	SQL    string
	Args   []any
	Bucket string
	From   time.Time
	To     time.Time
	Tier   string
	// Band holds the min/max envelope columns added for gauge means — extra
	// context for the reader, hidden from the charting model.
	Band []string
}

// tierFor picks the relation from the bucket size alone, which keeps one rule for
// both "how coarse is the answer" and "how much data must be read":
// finer than an hour needs raw readings, a day or coarser reads the daily rollup.
func tierFor(bucket string) string {
	switch bucket {
	case "1 minute", "5 minutes", "15 minutes":
		return "raw"
	case "1 hour", "6 hours":
		return "hourly"
	default:
		return "daily"
	}
}

func relationFor(tier string) string {
	switch tier {
	case "raw":
		return "v_readings"
	case "hourly":
		return "readings_1h"
	default:
		return "readings_1d"
	}
}

// tsLiteral inlines a timestamp instead of binding it. Measured on the NJ5 data:
// the same counter query took 545ms with literal bounds and 65s with $1/$2 —
// TimescaleDB excludes chunks (and the raw half of a real-time aggregate) while
// planning, which a bind parameter defeats. Safe because from/to are time.Time
// values the SERVER computed (windowFor, or a parsed absolute range); nothing the
// model or the user typed reaches this. Every model-supplied value still binds.
func tsLiteral(t time.Time) string {
	return "TIMESTAMPTZ '" + t.UTC().Format(time.RFC3339Nano) + "'"
}

// timeColumn is the bucketable time column of each tier.
func timeColumn(tier string) string {
	if tier == "raw" {
		return "t.ts"
	}
	return "t.bucket"
}

// aggSQL turns a metric kind into an aggregate over the chosen tier, restricted
// to one metric's rows by `filter`. Counter deltas are clamped at zero so a
// counter that was restarted contributes nothing instead of a negative spike.
func aggSQL(kind, tier, agg, filter string) string {
	f := ""
	if filter != "" {
		f = " FILTER (WHERE " + filter + ")"
	}
	raw := tier == "raw"
	switch kind {
	case "counter":
		if raw {
			return fmt.Sprintf("GREATEST(MAX(t.value)%s - MIN(t.value)%s, 0)", f, f)
		}
		// One rollup row per (series, bucket): the delta per row, summed across the
		// series that fall in the output bucket.
		return fmt.Sprintf("SUM(GREATEST(t.last_v - t.first_v, 0))%s", f)
	case "event":
		if raw {
			return fmt.Sprintf("SUM(t.value)%s", f)
		}
		return fmt.Sprintf("SUM(t.sum_v)%s", f)
	case "state":
		if raw {
			return fmt.Sprintf("last(t.value, t.ts)%s", f)
		}
		return fmt.Sprintf("last(t.last_v, t.bucket)%s", f)
	default: // gauge
		switch agg {
		case "min":
			if raw {
				return fmt.Sprintf("MIN(t.value)%s", f)
			}
			return fmt.Sprintf("MIN(t.min_v)%s", f)
		case "max":
			if raw {
				return fmt.Sprintf("MAX(t.value)%s", f)
			}
			return fmt.Sprintf("MAX(t.max_v)%s", f)
		default:
			if raw {
				return fmt.Sprintf("AVG(t.value)%s", f)
			}
			// Weighted: an average of hourly averages would be wrong whenever
			// buckets hold different numbers of readings.
			return fmt.Sprintf("(SUM(t.sum_v)%s) / NULLIF(SUM(t.n)%s, 0)", f, f)
		}
	}
}

// Suffixes of the min/max envelope columns that accompany a gauge mean. The chart
// model never sees them (compiled.Band lists them, hideColumns drops them) — the
// frontend recognizes them by name and shades the band itself.
const (
	bandMinSuffix = "_min"
	bandMaxSuffix = "_max"
)

// isMeanAgg reports whether a gauge is being averaged, which is the only case the
// envelope helps: an explicit min/max already IS the extreme.
func isMeanAgg(agg string) bool {
	a := strings.ToLower(strings.TrimSpace(agg))
	return a == "" || a == "auto" || a == "avg" || a == "mean"
}

// hideColumns drops the named columns from a column list and its rows, keeping the
// two in step. Used to show the charting model the mean without the envelope, so it
// encodes exactly what it did before the band existed.
func hideColumns(cols []string, rows [][]any, hide []string) ([]string, [][]any) {
	if len(hide) == 0 {
		return cols, rows
	}
	keep := make([]int, 0, len(cols))
	outCols := make([]string, 0, len(cols))
	for i, c := range cols {
		if slices.Contains(hide, c) {
			continue
		}
		keep = append(keep, i)
		outCols = append(outCols, c)
	}
	outRows := make([][]any, 0, len(rows))
	for _, r := range rows {
		row := make([]any, 0, len(keep))
		for _, i := range keep {
			if i < len(r) {
				row = append(row, r[i])
			}
		}
		outRows = append(outRows, row)
	}
	return outCols, outRows
}

var errEmptySpec = errors.New("spec names no metric")

// argBuilder collects bind parameters so callers never format a value into SQL.
type argBuilder struct{ args []any }

func (a *argBuilder) add(v any) string {
	a.args = append(a.args, v)
	return fmt.Sprintf("$%d", len(a.args))
}

// compile builds the SQL for a spec. from/to are already resolved by the caller
// (windowFor / an absolute range), and bucket by autoBucket, so a zoom is just
// another compile with a narrower window — no model call.
func (c *catalog) compile(spec querySpec, from, to time.Time, bucket string) (*compiled, error) {
	if spec.Shape == "list" {
		return c.compileList(spec)
	}
	if len(spec.Metrics) == 0 {
		return nil, errEmptySpec
	}

	tier := tierFor(bucket)
	rel := relationFor(tier)
	var a argBuilder
	fromArg, toArg := tsLiteral(from), tsLiteral(to)
	factoryArg := a.add(c.FactoryID)

	// One OR-group per metric: each metric may sit on a different machine, so the
	// row filter is per metric rather than global.
	// Dimension columns come first, then one aggregate per metric, so GROUP BY is
	// just the ordinals 1..dims.
	selects := make([]string, 0, len(spec.Metrics)+2)
	var rowFilters []string
	var band []string

	timeCol := timeColumn(tier)
	if spec.Shape == "timeseries" {
		selects = append(selects, fmt.Sprintf("time_bucket(%s::interval, %s) AS bucket", a.add(bucket), timeCol))
	}

	// A breakdown label becomes a text column; the frontend splits one series per
	// distinct value (withDataset), so the model never has to author series.
	for _, key := range spec.Breakdown {
		k := strings.ToLower(strings.TrimSpace(key))
		if k == "machine" {
			selects = append(selects, "vs.machine AS machine")
			continue
		}
		if _, ok := c.Labels[k]; !ok {
			continue // unknown dimension — ignore rather than fail the answer
		}
		selects = append(selects, fmt.Sprintf("vs.labels->>%s AS %s", a.add(k), quoteIdent(k)))
	}
	dims := len(selects)
	groups := make([]string, 0, dims)
	for i := 0; i < dims; i++ {
		groups = append(groups, fmt.Sprint(i+1))
	}

	for _, m := range spec.Metrics {
		cm, ok := c.metric(m.Field)
		if !ok {
			return nil, fmt.Errorf("metric %q is not in this factory's catalog", m.Field)
		}
		conds := []string{fmt.Sprintf("vs.field_key = %s", a.add(cm.FieldKey))}
		if s := strings.TrimSpace(m.Machine); s != "" {
			conds = append(conds, fmt.Sprintf("vs.machine ILIKE %s", a.add("%"+s+"%")))
		}
		if s := strings.TrimSpace(m.MachineType); s != "" {
			conds = append(conds, fmt.Sprintf("vs.machine_type ILIKE %s", a.add(s)))
		}
		for k, v := range m.Labels {
			key := strings.ToLower(strings.TrimSpace(k))
			if _, known := c.Labels[key]; !known {
				continue
			}
			pat := strings.TrimSpace(v)
			if pat == "" {
				continue
			}
			// A trailing * is a prefix match; otherwise match the value loosely,
			// because upstream label values are dirty ('Unknown', ingest typos).
			if strings.HasSuffix(pat, "*") {
				pat = strings.TrimSuffix(pat, "*") + "%"
			} else {
				pat = "%" + pat + "%"
			}
			conds = append(conds, fmt.Sprintf("vs.labels->>%s ILIKE %s", a.add(key), a.add(pat)))
		}
		filter := strings.Join(conds, " AND ")
		rowFilters = append(rowFilters, "("+filter+")")
		selects = append(selects,
			fmt.Sprintf("%s AS %s", aggSQL(cm.Kind, tier, m.Agg, filter), quoteIdent(cm.FieldKey)))

		// The mean of a bimodal hour is a value the machine was never at: IQF2 evap
		// running at −35 with a 25-minute defrost to +25 averages to +1.6, and a
		// single line at +1.6 hides both states. The rollup already stores min_v and
		// max_v, so ship the envelope with the mean — the frontend shades it behind
		// the line and the spike is visible without zooming in.
		if spec.Shape == "timeseries" && tier != "raw" && cm.Kind == "gauge" && isMeanAgg(m.Agg) {
			lo, hi := cm.FieldKey+bandMinSuffix, cm.FieldKey+bandMaxSuffix
			selects = append(selects,
				fmt.Sprintf("MIN(t.min_v) FILTER (WHERE %s) AS %s", filter, quoteIdent(lo)),
				fmt.Sprintf("MAX(t.max_v) FILTER (WHERE %s) AS %s", filter, quoteIdent(hi)))
			band = append(band, lo, hi)
		}
	}

	where := []string{
		fmt.Sprintf("vs.factory_id = %s::uuid", factoryArg),
		fmt.Sprintf("%s >= %s AND %s < %s", timeCol, fromArg, timeCol, toArg),
		"(" + strings.Join(rowFilters, " OR ") + ")",
	}
	if tier == "raw" {
		// The rollups already exclude flagged readings; raw has to say so.
		where = append(where, "t.quality = 0")
	}

	orderBy := "1"
	limit := maxRows
	if spec.Shape == "bar" || spec.Shape == "table" {
		if dims > 0 {
			// Rank by the first metric column — "which SKU produced most".
			orderBy = fmt.Sprint(dims+1) + " DESC NULLS LAST"
		}
		if spec.TopN > 0 && spec.TopN < limit {
			limit = spec.TopN
		}
	}

	groupBy := ""
	if len(groups) > 0 {
		groupBy = " GROUP BY " + strings.Join(groups, ", ")
	}

	sql := fmt.Sprintf(
		"SELECT %s FROM %s t JOIN v_series vs ON vs.series_id = t.series_id WHERE %s%s ORDER BY %s LIMIT %d",
		strings.Join(selects, ", "), rel, strings.Join(where, " AND "), groupBy, orderBy, limit)

	return &compiled{SQL: sql, Args: a.args, Bucket: bucket, From: from, To: to, Tier: tier, Band: band}, nil
}

// compileList answers "what exists" from the catalog view — no time window, no
// aggregation, so the frontend renders it as a table.
func (c *catalog) compileList(spec querySpec) (*compiled, error) {
	var a argBuilder
	factoryArg := a.add(c.FactoryID)
	what := "machine"
	if len(spec.Breakdown) > 0 {
		what = strings.ToLower(strings.TrimSpace(spec.Breakdown[0]))
	}

	var sql string
	switch {
	case what == "metric" || what == "field":
		sql = fmt.Sprintf(`SELECT DISTINCT vs.field_key AS metric, vs.kind, COALESCE(vs.unit,'') AS unit
			FROM v_series vs WHERE vs.factory_id = %s::uuid ORDER BY 1 LIMIT 200`, factoryArg)
	case what == "machine":
		sql = fmt.Sprintf(`SELECT DISTINCT vs.machine, vs.machine_type
			FROM v_series vs WHERE vs.factory_id = %s::uuid ORDER BY 1 LIMIT 500`, factoryArg)
	default:
		if _, ok := c.Labels[what]; !ok {
			return nil, fmt.Errorf("%q is not a dimension of this factory", what)
		}
		sql = fmt.Sprintf(`SELECT DISTINCT vs.labels->>%s AS %s
			FROM v_series vs WHERE vs.factory_id = %s::uuid AND vs.labels ? %s
			ORDER BY 1 LIMIT 500`, a.add(what), quoteIdent(what), factoryArg, a.add(what))
	}
	return &compiled{SQL: sql, Args: a.args, Tier: "catalog"}, nil
}

func (c *catalog) metric(field string) (catalogMetric, bool) {
	want := strings.ToLower(strings.TrimSpace(field))
	for _, m := range c.Metrics {
		if strings.ToLower(m.FieldKey) == want {
			return m, true
		}
	}
	return catalogMetric{}, false
}

// quoteIdent makes a result-column alias out of a catalog key. Keys come from the
// registry (admin-entered), but an alias is the one place a name lands in SQL
// text, so keep it to characters that cannot end an identifier early.
func quoteIdent(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteByte('"')
	return b.String()
}
