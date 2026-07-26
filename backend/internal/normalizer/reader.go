package normalizer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"iot-dashboard/internal/database"

	"github.com/jackc/pgx/v5"
)

// source is one registered upstream table (a row of source_tables plus its
// source_metrics). Every SQL fragment here comes from that registry — never from
// an end user — which is what lets the reader interpolate them.
type source struct {
	ID       int
	Factory  string
	Table    string
	Shape    string
	TsExpr   string
	MachExpr string
	Labels   []label // sorted by name for a stable column order
	Overlap  time.Duration
	Batch    int
	Metrics  []metric
}

type label struct {
	Name string
	Expr string
}

type metric struct {
	Column   string
	Expr     string // value_expr, or Column when unset
	FieldKey string
	Kind     string
	Sentinel *float64
	ValidMin *float64
	ValidMax *float64
}

// srcRow is one upstream reading, already split into its dimensions and one
// value per registered metric (nil = the column was NULL for this row).
type srcRow struct {
	TS      time.Time
	Machine string
	Labels  map[string]string
	Values  []*float64
}

// reader fetches upstream rows newer than `since`. Swapping in a foreign-table
// or push-ingest source later means another implementation here, not a change to
// the normalizer: the upstream transport is the one thing we were told is still
// undecided.
type reader interface {
	Fetch(ctx context.Context, src source, since time.Time) ([]srcRow, error)
}

// pollReader reads directly from a landing table in our own database.
type pollReader struct{}

func (pollReader) Fetch(ctx context.Context, src source, since time.Time) ([]srcRow, error) {
	cols := []string{
		fmt.Sprintf("(%s) AS __ts", src.TsExpr),
		fmt.Sprintf("(%s)::text AS __machine", src.MachExpr),
	}
	for i, l := range src.Labels {
		cols = append(cols, fmt.Sprintf("(%s)::text AS __l%d", l.Expr, i))
	}
	for i, m := range src.Metrics {
		cols = append(cols, fmt.Sprintf("(%s)::double precision AS __v%d", m.Expr, i))
	}

	// Filter on ts_expr itself: nj5_machines is indexed on to_timestamp(time_stamp)
	// and the freezer tables on created_at_ts, so both stay off a sequential scan.
	q := fmt.Sprintf(
		`SELECT %s FROM %s WHERE (%s) > $1 ORDER BY 1 LIMIT %d`,
		strings.Join(cols, ", "),
		pgx.Identifier{src.Table}.Sanitize(),
		src.TsExpr,
		src.Batch,
	)

	rows, err := database.Pool.Query(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", src.Table, err)
	}
	defer rows.Close()

	out := make([]srcRow, 0, 1024)
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		r := srcRow{Values: make([]*float64, len(src.Metrics))}
		if ts, ok := vals[0].(time.Time); ok {
			r.TS = ts
		} else {
			continue // a NULL time column cannot be placed on the grid
		}
		if s, ok := vals[1].(string); ok {
			r.Machine = strings.TrimSpace(s)
		}
		if r.Machine == "" {
			continue
		}
		off := 2
		if len(src.Labels) > 0 {
			r.Labels = make(map[string]string, len(src.Labels))
			for i, l := range src.Labels {
				if s, ok := vals[off+i].(string); ok && s != "" {
					r.Labels[l.Name] = s
				}
			}
		}
		off += len(src.Labels)
		for i := range src.Metrics {
			if f, ok := vals[off+i].(float64); ok {
				v := f
				r.Values[i] = &v
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadSources reads the registry. Disabled rows and rows with no enabled metric
// are skipped — an unregistered column is invisible on purpose, so a source with
// nothing registered has nothing to do.
func loadSources(ctx context.Context) ([]source, error) {
	rows, err := database.Pool.Query(ctx, `
		SELECT id, factory_id::text, table_name, shape, ts_expr, machine_expr,
		       label_exprs, overlap_seconds, batch_rows
		FROM source_tables
		WHERE enabled
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []source
	for rows.Next() {
		var s source
		var labels map[string]string
		var overlap, batch int
		if err := rows.Scan(&s.ID, &s.Factory, &s.Table, &s.Shape, &s.TsExpr,
			&s.MachExpr, &labels, &overlap, &batch); err != nil {
			return nil, err
		}
		for name, expr := range labels {
			s.Labels = append(s.Labels, label{Name: name, Expr: expr})
		}
		sortLabels(s.Labels)
		s.Overlap = time.Duration(overlap) * time.Second
		s.Batch = batch
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		if out[i].Metrics, err = loadMetrics(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func loadMetrics(ctx context.Context, sourceID int) ([]metric, error) {
	rows, err := database.Pool.Query(ctx, `
		SELECT column_name, COALESCE(value_expr, column_name), field_key, kind,
		       sentinel, valid_min, valid_max
		FROM source_metrics
		WHERE source_table_id = $1 AND enabled
		ORDER BY id`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []metric
	for rows.Next() {
		var m metric
		if err := rows.Scan(&m.Column, &m.Expr, &m.FieldKey, &m.Kind,
			&m.Sentinel, &m.ValidMin, &m.ValidMax); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func sortLabels(ls []label) {
	for i := 1; i < len(ls); i++ {
		for j := i; j > 0 && ls[j].Name < ls[j-1].Name; j-- {
			ls[j], ls[j-1] = ls[j-1], ls[j]
		}
	}
}

// quality classifies a reading against the catalog: 0 ok, 1 sentinel,
// 2 outside the plausible range. Bad readings are stored and flagged rather than
// dropped, so a mis-registered sentinel is fixed by re-registering it.
func (m metric) quality(v float64) int16 {
	if m.Sentinel != nil && v == *m.Sentinel {
		return 1
	}
	if (m.ValidMin != nil && v < *m.ValidMin) || (m.ValidMax != nil && v > *m.ValidMax) {
		return 2
	}
	return 0
}
