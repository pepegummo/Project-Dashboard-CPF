// Package normalizer turns rows from the landing tables (whatever shape the
// plant systems send) into the canonical series + readings model, driven entirely
// by the source registry. Registering a new upstream table is an INSERT into
// source_tables/source_metrics — no code change, no deploy.
//
// It follows the same shape as internal/broadcaster: New / Start / Stop around a
// ticker, wired once in cmd/server/main.go.
package normalizer

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"iot-dashboard/internal/database"

	"github.com/jackc/pgx/v5"
)

type Normalizer struct {
	interval time.Duration
	reader   reader
	stop     chan struct{}
	// machines/series resolved so far, so a steady state does no lookup queries.
	machineIDs map[string]string // factory|name -> machines.id
	seriesIDs  map[string]int64  // machine|field|labels -> series.id
}

func New(interval time.Duration) *Normalizer {
	return &Normalizer{
		interval:   interval,
		reader:     pollReader{},
		stop:       make(chan struct{}),
		machineIDs: map[string]string{},
		seriesIDs:  map[string]int64{},
	}
}

func (n *Normalizer) Start() { go n.run() }
func (n *Normalizer) Stop()  { close(n.stop) }

func (n *Normalizer) run() {
	fmt.Printf("🔄  Normalizer started — polling registry every %v\n", n.interval)
	ticker := time.NewTicker(n.interval)
	defer ticker.Stop()

	n.tick()
	for {
		select {
		case <-n.stop:
			return
		case <-ticker.C:
			n.tick()
		}
	}
}

// tick drains every enabled source once. A source that errors records the error
// and is retried next tick — one bad upstream table never stops the others.
func (n *Normalizer) tick() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sources, err := loadSources(ctx)
	if err != nil {
		log.Printf("[normalizer] load registry: %v", err)
		return
	}
	for _, src := range sources {
		if len(src.Metrics) == 0 {
			continue
		}
		if _, err := n.syncSource(ctx, src); err != nil {
			log.Printf("[normalizer] %s: %v", src.Table, err)
			recordError(ctx, src.ID, err)
		}
	}
}

// RunUntilCaughtUp drains every source until each returns a short batch. Used by
// cmd/normalize-backfill to load a historical dump in one pass.
func RunUntilCaughtUp(ctx context.Context) error {
	n := New(time.Minute)
	sources, err := loadSources(ctx)
	if err != nil {
		return err
	}
	for _, src := range sources {
		if len(src.Metrics) == 0 {
			continue
		}
		total := 0
		for {
			got, err := n.syncSource(ctx, src)
			if err != nil {
				return fmt.Errorf("%s: %w", src.Table, err)
			}
			total += got
			if got < src.Batch {
				break
			}
			if total%500_000 < src.Batch {
				fmt.Printf("   %s: %d rows…\n", src.Table, total)
			}
		}
		fmt.Printf("✅ %s: %d readings\n", src.Table, total)
	}
	return nil
}

// syncSource reads one batch and writes it. Returns the number of upstream rows
// read so the caller can tell "caught up" from "more to come".
func (n *Normalizer) syncSource(ctx context.Context, src source) (int, error) {
	since, err := watermark(ctx, src)
	if err != nil {
		return 0, err
	}

	rows, err := n.reader.Fetch(ctx, src, since)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, markRun(ctx, src.ID)
	}

	// Resolve dimensions before the write transaction: an unknown machine or
	// series is created here (auto-discovery), so a machine the plant adds
	// overnight is answerable the next morning with nobody editing anything.
	type point struct {
		series  int64
		ts      time.Time
		value   float64
		quality int16
	}
	points := make([]point, 0, len(rows)*len(src.Metrics))
	maxTS := since
	for _, r := range rows {
		if r.TS.After(maxTS) {
			maxTS = r.TS
		}
		machineID, err := n.resolveMachine(ctx, src, r.Machine)
		if err != nil {
			return 0, err
		}
		for i, m := range src.Metrics {
			if r.Values[i] == nil {
				continue // upstream sent NULL — nothing to record
			}
			seriesID, err := n.resolveSeries(ctx, src, machineID, m, r.Labels)
			if err != nil {
				return 0, err
			}
			points = append(points, point{seriesID, r.TS, *r.Values[i], m.quality(*r.Values[i])})
		}
	}

	seriesIDs := make([]int64, len(points))
	ts := make([]time.Time, len(points))
	values := make([]float64, len(points))
	quality := make([]int16, len(points))
	for i, p := range points {
		seriesIDs[i], ts[i], values[i], quality[i] = p.series, p.ts, p.value, p.quality
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	// readings/series are FORCE RLS on app.factory — the writer must scope itself
	// too, which also means a registry row can never write into another plant.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.factory', $1, true)`, src.Factory); err != nil {
		return 0, err
	}
	// ON CONFLICT DO NOTHING makes the overlap re-read (and any replay of the
	// same dump) free instead of a duplicate-key failure.
	if _, err := tx.Exec(ctx, `
		INSERT INTO readings (series_id, ts, value, quality)
		SELECT * FROM unnest($1::bigint[], $2::timestamptz[], $3::double precision[], $4::smallint[])
		ON CONFLICT (series_id, ts) DO NOTHING`,
		seriesIDs, ts, values, quality); err != nil {
		return 0, err
	}
	// ponytail: the watermark only ever moves forward, which is what makes a second
	// writer safe — running `normalize-backfill` while the server's normalizer is
	// polling the same source means duplicate reads (dropped by ON CONFLICT), never
	// a rewind into an endless re-read. Compared as timestamptz, not text: the
	// RFC3339Nano string drops trailing zeros, so ".6Z" sorts after ".608Z".
	if _, err := tx.Exec(ctx, `
		UPDATE source_state
		SET last_watermark = CASE
		        WHEN last_watermark IS NULL
		          OR last_watermark::timestamptz < $2::text::timestamptz
		        THEN $2 ELSE last_watermark END,
		    last_run_at = NOW(),
		    rows_ingested = rows_ingested + $3, last_error = NULL
		WHERE source_table_id = $1`,
		src.ID, maxTS.Format(time.RFC3339Nano), len(points)); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// watermark is the last timestamp written, pulled back by overlap_seconds so
// rows that arrived slightly out of order are picked up on the next pass.
func watermark(ctx context.Context, src source) (time.Time, error) {
	var raw *string
	err := database.Pool.QueryRow(ctx,
		`SELECT last_watermark FROM source_state WHERE source_table_id = $1`, src.ID).Scan(&raw)
	if err == pgx.ErrNoRows {
		if _, err := database.Pool.Exec(ctx,
			`INSERT INTO source_state (source_table_id) VALUES ($1)
			 ON CONFLICT (source_table_id) DO NOTHING`, src.ID); err != nil {
			return time.Time{}, err
		}
		return time.Unix(0, 0).UTC(), nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if raw == nil || *raw == "" {
		return time.Unix(0, 0).UTC(), nil
	}
	t, perr := time.Parse(time.RFC3339Nano, *raw)
	if perr != nil {
		return time.Unix(0, 0).UTC(), nil
	}
	return t.Add(-src.Overlap), nil
}

func markRun(ctx context.Context, sourceID int) error {
	_, err := database.Pool.Exec(ctx, `
		UPDATE source_state SET last_run_at = NOW(), last_error = NULL
		WHERE source_table_id = $1`, sourceID)
	return err
}

func recordError(ctx context.Context, sourceID int, cause error) {
	_, _ = database.Pool.Exec(ctx, `
		UPDATE source_state SET last_run_at = NOW(), last_error = $2
		WHERE source_table_id = $1`, sourceID, cause.Error())
}

// resolveMachine maps an upstream machine string to a machines row, creating it
// when unseen. metadata.discovery marks it for an admin to confirm — the name is
// whatever the plant sent, which may need a friendlier label.
func (n *Normalizer) resolveMachine(ctx context.Context, src source, name string) (string, error) {
	key := src.Factory + "|" + name
	if id, ok := n.machineIDs[key]; ok {
		return id, nil
	}

	var id string
	err := database.Pool.QueryRow(ctx, `
		SELECT m.id::text
		FROM machines m
		JOIN production_lines pl ON pl.id = m.production_line_id
		WHERE pl.factory_id = $1::uuid AND m.name = $2
		LIMIT 1`, src.Factory, name).Scan(&id)
	if err == nil {
		n.machineIDs[key] = id
		return id, nil
	}
	if err != pgx.ErrNoRows {
		return "", err
	}

	lineID, err := n.resolveLine(ctx, src.Factory)
	if err != nil {
		return "", err
	}
	meta, _ := json.Marshal(map[string]any{
		"discovery": "auto",
		"source":    src.Table,
		"confirmed": false,
	})
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO machines (production_line_id, name, type, status, metadata)
		VALUES ($1::uuid, $2, $3, 'online', $4::jsonb)
		RETURNING id::text`, lineID, name, machineType(name), meta).Scan(&id); err != nil {
		return "", fmt.Errorf("auto-discover machine %q: %w", name, err)
	}
	log.Printf("[normalizer] auto-discovered machine %q (type %s) from %s", name, machineType(name), src.Table)
	n.machineIDs[key] = id
	return id, nil
}

func (n *Normalizer) resolveLine(ctx context.Context, factoryID string) (string, error) {
	var id string
	err := database.Pool.QueryRow(ctx,
		`SELECT id::text FROM production_lines WHERE factory_id = $1::uuid ORDER BY created_at LIMIT 1`,
		factoryID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != pgx.ErrNoRows {
		return "", err
	}
	err = database.Pool.QueryRow(ctx, `
		INSERT INTO production_lines (factory_id, name, code, status)
		VALUES ($1::uuid, 'Auto', 'AUTO', 'active') RETURNING id::text`, factoryID).Scan(&id)
	return id, err
}

// resolveSeries finds or creates the (machine, metric, labels) series. A counter's
// identity includes its labels: IQF2/OutFeed/Carb counts separately from
// IQF2/OutFeed/Tray, so their deltas must never be mixed.
func (n *Normalizer) resolveSeries(ctx context.Context, src source, machineID string, m metric, labels map[string]string) (int64, error) {
	lbl, err := json.Marshal(orEmpty(labels))
	if err != nil {
		return 0, err
	}
	key := machineID + "|" + m.FieldKey + "|" + string(lbl)
	if id, ok := n.seriesIDs[key]; ok {
		return id, nil
	}

	var id int64
	// The labels JSONB is compared by value, so the ON CONFLICT target matches
	// only when the same dimension set comes back around.
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO series (factory_id, machine_id, field_key, labels)
		VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)
		ON CONFLICT (machine_id, field_key, labels) DO UPDATE SET last_seen = NOW()
		RETURNING id`, src.Factory, machineID, m.FieldKey, lbl).Scan(&id); err != nil {
		return 0, fmt.Errorf("series %s/%s: %w", machineID, m.FieldKey, err)
	}
	n.seriesIDs[key] = id
	return id, nil
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// machineType strips the trailing unit number so the catalog prompt can describe
// a family once ("IQF") instead of every instance — that is what keeps prompt
// size tied to the number of metric kinds rather than the size of the fleet.
func machineType(name string) string {
	end := len(name)
	for end > 0 && name[end-1] >= '0' && name[end-1] <= '9' {
		end--
	}
	for end > 0 && (name[end-1] == '_' || name[end-1] == '-' || name[end-1] == ' ') {
		end--
	}
	if end == 0 {
		return "unknown"
	}
	return name[:end]
}
