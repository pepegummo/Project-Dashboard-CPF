package ai

// Catalog-driven schema context for the canonical dataset.
//
// The telemetry prompt is assembled from a hand-written string plus two lookup
// queries (buildSchemaContext); the NJ5 prompt was a Go constant. Neither scales:
// a machine the plant adds overnight silently falls out of a hardcoded roster,
// and every new upstream table wants another paragraph of prose.
//
// Here the prompt is generated from the source registry instead, and the SAME
// loaded catalog is what the query compiler reads to choose an aggregation — so
// the rule the model is told and the rule the server enforces cannot drift apart.
//
// Prompt size is deliberately tied to the number of METRIC KINDS, not the size of
// the fleet: metrics are described once per machine type, and machine/label values
// are listed only up to a cap, with the model told to query v_series when it needs
// the full roster. One factory per question keeps it bounded as plants are added.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"iot-dashboard/internal/database"
	"iot-dashboard/internal/middleware"

	"github.com/gofiber/fiber/v2"
)

// Caps on what goes into the prompt. Past these the model is told to list values
// from v_series rather than being handed a wall of text.
const (
	maxPromptMachines = 24
	maxPromptLabels   = 30
)

type catalogMetric struct {
	FieldKey string
	Kind     string // gauge | counter | event | state
	Unit     string
	Note     string
	Types    []string // machine types that actually carry this metric
}

type catalog struct {
	FactoryID   string
	FactoryName string
	Timezone    string
	Machines    map[string][]string // machine type -> machine names
	Metrics     []catalogMetric
	Labels      map[string][]string // label key -> observed values (capped)
	Span        [2]time.Time        // first/last reading seen, for the model's sense of range
	version     string
}

// aggDescription is the prompt-facing twin of aggSQL (compiler.go): the model is
// told what a kind MEANS, the compiler decides the SQL. Keep the two in step.
func aggDescription(kind string) string {
	switch kind {
	case "counter":
		return "cumulative counter — a period's total is the delta, never a sum of readings"
	case "event":
		return "occurrence flag — count/sum the occurrences"
	case "state":
		return "discrete state — take the value in force at the end of the bucket"
	default:
		return "instantaneous gauge — average/min/max over the period"
	}
}

var (
	catalogMu    sync.Mutex
	catalogCache = map[string]*catalog{}
)

// loadCatalog reads the registry for one factory. Cached on a version stamp
// derived from the catalog's own timestamps, so a registered metric or a
// newly discovered machine invalidates it, while a steady state costs one cheap
// query and keeps the generated prompt byte-identical (prompt cache stays warm).
func loadCatalog(ctx context.Context, factoryID string) (*catalog, error) {
	version, err := catalogVersion(ctx, factoryID)
	if err != nil {
		return nil, err
	}

	catalogMu.Lock()
	if c, ok := catalogCache[factoryID]; ok && c.version == version {
		catalogMu.Unlock()
		return c, nil
	}
	catalogMu.Unlock()

	c := &catalog{
		FactoryID: factoryID,
		Machines:  map[string][]string{},
		Labels:    map[string][]string{},
		version:   version,
	}

	if err := database.Pool.QueryRow(ctx,
		`SELECT name, timezone FROM factories WHERE id = $1::uuid`, factoryID).
		Scan(&c.FactoryName, &c.Timezone); err != nil {
		return nil, fmt.Errorf("factory %s: %w", factoryID, err)
	}

	// Metrics come from the registry (kind/unit/note), and the machine types that
	// carry them from the series that actually exist — so a registered column with
	// no data yet is not advertised as answerable.
	rows, err := database.Pool.Query(ctx, `
		SELECT sm.field_key, sm.kind, COALESCE(sm.unit,''), COALESCE(sm.llm_note,''),
		       array_agg(DISTINCT m.type) FILTER (WHERE m.type IS NOT NULL) AS types
		FROM source_metrics sm
		JOIN source_tables st ON st.id = sm.source_table_id
		LEFT JOIN series s    ON s.field_key = sm.field_key AND s.factory_id = st.factory_id
		LEFT JOIN machines m  ON m.id = s.machine_id
		WHERE st.factory_id = $1::uuid AND sm.enabled AND st.enabled
		GROUP BY 1, 2, 3, 4
		ORDER BY 1`, factoryID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m catalogMetric
		var types []string
		if err := rows.Scan(&m.FieldKey, &m.Kind, &m.Unit, &m.Note, &types); err != nil {
			rows.Close()
			return nil, err
		}
		m.Types = types
		c.Metrics = append(c.Metrics, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	mrows, err := database.Pool.Query(ctx, `
		SELECT machine_type, machine FROM v_series
		WHERE factory_id = $1::uuid
		GROUP BY 1, 2 ORDER BY 1, 2`, factoryID)
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var mtype, name string
		if err := mrows.Scan(&mtype, &name); err != nil {
			mrows.Close()
			return nil, err
		}
		c.Machines[mtype] = append(c.Machines[mtype], name)
	}
	mrows.Close()

	// Label keys and their values, capped. These are the dimensions a question can
	// break down by (area, sku); high-cardinality keys must never be registered as
	// labels, so a cap here is a safety net, not the design.
	lrows, err := database.Pool.Query(ctx, `
		SELECT kv.key, kv.value
		FROM series s, jsonb_each_text(s.labels) AS kv
		WHERE s.factory_id = $1::uuid
		GROUP BY 1, 2 ORDER BY 1, 2`, factoryID)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var k, v string
		if err := lrows.Scan(&k, &v); err != nil {
			lrows.Close()
			return nil, err
		}
		c.Labels[k] = append(c.Labels[k], v)
	}
	lrows.Close()

	_ = database.Pool.QueryRow(ctx, `
		SELECT COALESCE(min(bucket), now()), COALESCE(max(bucket), now())
		FROM readings_1d h JOIN series s ON s.id = h.series_id
		WHERE s.factory_id = $1::uuid`, factoryID).Scan(&c.Span[0], &c.Span[1])

	catalogMu.Lock()
	catalogCache[factoryID] = c
	catalogMu.Unlock()
	return c, nil
}

func catalogVersion(ctx context.Context, factoryID string) (string, error) {
	var metricStamp, seriesStamp *time.Time
	var metrics, series int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT max(sm.updated_at) FROM source_metrics sm
		          JOIN source_tables st ON st.id = sm.source_table_id
		         WHERE st.factory_id = $1::uuid),
		       (SELECT count(*)         FROM source_metrics sm
		          JOIN source_tables st ON st.id = sm.source_table_id
		         WHERE st.factory_id = $1::uuid),
		       (SELECT max(first_seen)  FROM series WHERE factory_id = $1::uuid),
		       (SELECT count(*)         FROM series WHERE factory_id = $1::uuid)`,
		factoryID).Scan(&metricStamp, &metrics, &seriesStamp, &series); err != nil {
		return "", err
	}
	return fmt.Sprintf("%v/%d/%v/%d", metricStamp, metrics, seriesStamp, series), nil
}

// ListScopes answers GET /ai/scopes: which datasets this user can ask against.
// The demo/telemetry dataset is always offered; a canonical scope appears for every
// factory that has registered sources, so standing up plant #2 puts it in the picker
// without a frontend change.
func ListScopes(c *fiber.Ctx) error {
	user := middleware.GetUser(c)
	if user == nil {
		return c.Status(401).JSON(fiber.Map{"success": false})
	}
	scopes := []fiber.Map{
		{"dataset": "telemetry", "label": "Demo telemetry", "factoryId": ""},
	}
	rows, err := database.Pool.Query(context.Background(), `
		SELECT f.id::text, f.name, count(DISTINCT st.id)
		FROM factories f
		JOIN source_tables st ON st.factory_id = f.id AND st.enabled
		GROUP BY 1, 2 ORDER BY 2`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, name string
			var sources int
			if err := rows.Scan(&id, &name, &sources); err != nil {
				break
			}
			scopes = append(scopes, fiber.Map{
				"dataset": "canonical", "label": name, "factoryId": id, "sources": sources,
			})
		}
	}
	return c.JSON(fiber.Map{"success": true, "data": scopes})
}

// promptContext renders the catalog as the schema context handed to the model.
// It describes what exists and what each metric MEANS; it does not teach SQL,
// because on this path the model emits a query spec and the server writes the SQL.
func (c *catalog) promptContext() string {
	var b strings.Builder

	fmt.Fprintf(&b, `You answer questions about ONE factory: %s (timezone %s).
Data available from %s to %s. There is no other plant in scope — never ask the user which one.

Return a query SPEC, not SQL: name the metric(s), the machine, the labels to filter or break down by,
and the time window. The server compiles the SQL, chooses the aggregation from each metric's kind, the
resolution the window deserves, and the row limit. You never choose how a metric is aggregated.

`, c.FactoryName, c.Timezone,
		c.Span[0].Format("2006-01-02"), c.Span[1].Format("2006-01-02"))

	b.WriteString("MACHINES (by type):\n")
	types := make([]string, 0, len(c.Machines))
	for t := range c.Machines {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		names := c.Machines[t]
		shown := names
		suffix := ""
		if len(names) > maxPromptMachines {
			shown = names[:maxPromptMachines]
			suffix = fmt.Sprintf(", … (%d total — ask for shape=list to enumerate)", len(names))
		}
		fmt.Fprintf(&b, "  %-10s %s%s\n", t, strings.Join(shown, ", "), suffix)
	}
	b.WriteString("  Machines are added upstream without warning. If a question names a machine that is not\n" +
		"  listed here, do NOT guess it away — set shape=list to look it up.\n\n")

	b.WriteString("METRICS (kind decides the math — you never override it):\n")
	for _, m := range c.Metrics {
		unit := m.Unit
		if unit != "" {
			unit = " [" + unit + "]"
		}
		fmt.Fprintf(&b, "  %-16s %-8s%-8s on %s\n", m.FieldKey, m.Kind, unit, strings.Join(m.Types, ", "))
		fmt.Fprintf(&b, "        %s\n", aggDescription(m.Kind))
		if m.Note != "" {
			fmt.Fprintf(&b, "        %s\n", m.Note)
		}
	}
	b.WriteString("  A metric that is not listed does not exist in this factory's data — say so plainly\n" +
		"  (answerable=false) instead of substituting a different one.\n\n")

	if len(c.Labels) > 0 {
		b.WriteString("LABELS (dimensions you may filter or break down by):\n")
		keys := make([]string, 0, len(c.Labels))
		for k := range c.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			vals := c.Labels[k]
			shown := vals
			suffix := ""
			if len(vals) > maxPromptLabels {
				shown = vals[:maxPromptLabels]
				suffix = fmt.Sprintf(", … (%d total)", len(vals))
			}
			fmt.Fprintf(&b, "  %-6s %s%s\n", k, strings.Join(shown, ", "), suffix)
		}
		b.WriteString("  Match label values loosely — a trailing * is a prefix match, and values can be dirty\n" +
			"  ('Unknown', ingest typos). When a question is ambiguous, ask which one.\n\n")
	}

	b.WriteString(`SHAPES:
  timeseries — a trend over time ("over time", "per hour", "ย้อนหลัง", "trend"). Two metrics in one
               timeseries spec draws a dual axis, which is how a correlation question is answered.
  bar        — one number per label value or per machine (a ranking, "which SKU most")
  table      — a plain listing of values
  list       — the catalog itself: which machines / metrics / label values exist
`)
	return b.String()
}
