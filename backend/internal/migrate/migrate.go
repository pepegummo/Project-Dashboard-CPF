// Package migrate provides idempotent schema creation and seed data insertion.
// Safe to call on every startup — all statements use IF NOT EXISTS / ON CONFLICT DO NOTHING.
package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// RunAll runs EnsureSchema then EnsureSeed. Call this once after DB connect.
func RunAll(ctx context.Context, pool *pgxpool.Pool) error {
	if err := EnsureSchema(ctx, pool); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	if err := EnsureSeed(ctx, pool); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	if err := EnsureDemoOrgs(ctx, pool); err != nil {
		return fmt.Errorf("demo orgs: %w", err)
	}
	return nil
}

// ─── Schema ───────────────────────────────────────────────────────────────────

// EnsureSchema creates all tables, the TimescaleDB hypertable, and indexes.
// Safe to run multiple times — uses IF NOT EXISTS throughout.
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	ddl := []string{
		// Extensions
		`CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE`,
		`CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`,

		// ── organizations ───────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS organizations (
			id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			name       TEXT        NOT NULL,
			slug       TEXT        UNIQUE NOT NULL,
			plan       TEXT        NOT NULL DEFAULT 'starter',
			settings   JSONB       NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		// LED kiosk: permanent read-only token per org (NULL = not yet generated)
		`ALTER TABLE organizations ADD COLUMN IF NOT EXISTS led_token TEXT`,

		// ── users ────────────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS users (
			id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			email           TEXT        UNIQUE NOT NULL,
			name            TEXT        NOT NULL,
			password_hash   TEXT        NOT NULL,
			role            TEXT        NOT NULL DEFAULT 'viewer',
			avatar_url      TEXT,
			preferences     JSONB       NOT NULL DEFAULT '{}'::jsonb,
			last_login_at   TIMESTAMPTZ,
			is_active       BOOLEAN     NOT NULL DEFAULT TRUE,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// ── factories ────────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS factories (
			id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			name            TEXT        NOT NULL,
			location        TEXT,
			timezone        TEXT        NOT NULL DEFAULT 'UTC',
			metadata        JSONB       NOT NULL DEFAULT '{}'::jsonb,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		// slug is the factory's URL name: /ask/<slug> addresses one plant. Nullable —
		// the unique index allows many NULLs, so a factory without one is still legal
		// and is addressed by its UUID instead. The registry script owns the slug for
		// a real plant (scripts/nj5-registry.sql sets 'nj5'); this only derives one
		// for rows that arrived without.
		`ALTER TABLE factories ADD COLUMN IF NOT EXISTS slug TEXT`,
		// ponytail: collisions are skipped, not disambiguated with a suffix — a failed
		// backfill would block startup, and a slugless factory degrades to its UUID.
		`UPDATE factories f SET slug = lower(regexp_replace(f.name, '[^a-zA-Z0-9]+', '-', 'g'))
		 WHERE COALESCE(f.slug, '') = ''
		   AND NOT EXISTS (
		     SELECT 1 FROM factories o WHERE o.id <> f.id
		       AND o.slug = lower(regexp_replace(f.name, '[^a-zA-Z0-9]+', '-', 'g')))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS factories_slug_key ON factories (slug)`,

		// ── production_lines ─────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS production_lines (
			id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			factory_id UUID        NOT NULL REFERENCES factories(id) ON DELETE CASCADE,
			name       TEXT        NOT NULL,
			code       TEXT,
			status     TEXT        NOT NULL DEFAULT 'active',
			metadata   JSONB       NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// ── machines ─────────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS machines (
			id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			production_line_id UUID        NOT NULL REFERENCES production_lines(id) ON DELETE CASCADE,
			name               TEXT        NOT NULL,
			type               TEXT        NOT NULL,
			serial_number      TEXT,
			model              TEXT,
			manufacturer       TEXT,
			status             TEXT        NOT NULL DEFAULT 'online',
			last_seen_at       TIMESTAMPTZ,
			metadata           JSONB       NOT NULL DEFAULT '{}'::jsonb,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		// ── machine_fields ───────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS machine_fields (
			id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			machine_id  UUID        NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
			key         TEXT        NOT NULL,
			label       TEXT        NOT NULL,
			unit        TEXT,
			data_type   TEXT        NOT NULL DEFAULT 'number',
			min         FLOAT,
			max         FLOAT,
			threshold   FLOAT,
			upper_limit FLOAT,
			lower_limit FLOAT,
			precision   INTEGER     NOT NULL DEFAULT 2,
			enum_values JSONB,
			is_key      BOOLEAN     NOT NULL DEFAULT FALSE,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(machine_id, key)
		)`,

		// ── telemetry_raw — composite PK required by TimescaleDB ─────────────
		`CREATE TABLE IF NOT EXISTS telemetry_raw (
			id         BIGINT      GENERATED ALWAYS AS IDENTITY,
			machine_id UUID        NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
			timestamp  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			data       JSONB       NOT NULL,
			quality    TEXT        NOT NULL DEFAULT 'good',
			PRIMARY KEY (id, timestamp)
		)`,

		// ── telemetry_aggregates ─────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS telemetry_aggregates (
			id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			machine_id UUID        NOT NULL REFERENCES machines(id),
			field      TEXT        NOT NULL,
			period     TEXT        NOT NULL,
			timestamp  TIMESTAMPTZ NOT NULL,
			avg        FLOAT,
			min        FLOAT,
			max        FLOAT,
			stddev     FLOAT,
			count      INTEGER
		)`,

		// ── dashboards ───────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS dashboards (
			id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			user_id         UUID        NOT NULL REFERENCES users(id),
			name            TEXT        NOT NULL,
			description     TEXT,
			is_public       BOOLEAN     NOT NULL DEFAULT FALSE,
			is_default      BOOLEAN     NOT NULL DEFAULT FALSE,
			thumbnail       TEXT,
			tags            TEXT[]      NOT NULL DEFAULT '{}',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// ── dashboard_widgets ────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS dashboard_widgets (
			id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			dashboard_id UUID        NOT NULL REFERENCES dashboards(id) ON DELETE CASCADE,
			machine_id   UUID        REFERENCES machines(id) ON DELETE SET NULL,
			widget_type  TEXT        NOT NULL,
			title        TEXT,
			layout       JSONB       NOT NULL DEFAULT '{}'::jsonb,
			config       JSONB       NOT NULL DEFAULT '{}'::jsonb,
			"order"      INTEGER     NOT NULL DEFAULT 0,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// ── alerts ───────────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS alerts (
			id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			machine_id   UUID        NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
			name         TEXT        NOT NULL,
			description  TEXT,
			field        TEXT        NOT NULL,
			condition    TEXT        NOT NULL,
			threshold    FLOAT       NOT NULL,
			threshold_hi FLOAT,
			severity     TEXT        NOT NULL DEFAULT 'warning',
			is_active    BOOLEAN     NOT NULL DEFAULT TRUE,
			cooldown_sec INTEGER     NOT NULL DEFAULT 300,
			notify_email TEXT[]      NOT NULL DEFAULT '{}',
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// ── alert_events ─────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS alert_events (
			id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			alert_id        UUID        NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
			value           FLOAT       NOT NULL,
			message         TEXT,
			status          TEXT        NOT NULL DEFAULT 'open',
			triggered_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			acknowledged_at TIMESTAMPTZ,
			acknowledged_by TEXT,
			resolved_at     TIMESTAMPTZ,
			resolved_by     TEXT
		)`,

		// ── ai_conversations ─────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS ai_conversations (
			id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			title      TEXT        NOT NULL DEFAULT 'New Conversation',
			context    JSONB       NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// ── ai_messages ──────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS ai_messages (
			id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			conversation_id UUID        NOT NULL REFERENCES ai_conversations(id) ON DELETE CASCADE,
			role            TEXT        NOT NULL,
			content         TEXT        NOT NULL,
			tool_name       TEXT,
			tool_input      JSONB,
			tool_result     JSONB,
			tokens          INTEGER,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// ── ai_preview_drafts ────────────────────────────────────────────────
		// The AI page's persisted per-user view state: EITHER an in-progress
		// preview (data set) OR a selected dashboard (dashboard_id set).
		`CREATE TABLE IF NOT EXISTS ai_preview_drafts (
			user_id         UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			conversation_id UUID,
			dashboard_id    UUID,
			data            JSONB,
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		// Bring rows created by the earlier (preview-only) schema up to date.
		`ALTER TABLE ai_preview_drafts ADD COLUMN IF NOT EXISTS dashboard_id UUID`,
		`ALTER TABLE ai_preview_drafts ALTER COLUMN data DROP NOT NULL`,

		// ── user_organizations ───────────────────────────────────────────────
		// Membership join: one user can belong to many orgs. Admins bypass this
		// (they get every org); non-admins are limited to their rows here.
		`CREATE TABLE IF NOT EXISTS user_organizations (
			user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			PRIMARY KEY (user_id, organization_id)
		)`,

		// ── audit_logs ───────────────────────────────────────────────────────
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id     UUID        REFERENCES users(id) ON DELETE SET NULL,
			action      TEXT        NOT NULL,
			resource    TEXT        NOT NULL,
			resource_id UUID,
			before      JSONB,
			after       JSONB,
			ip_address  TEXT,
			user_agent  TEXT,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// ── Ask-Data org-scoped views (NL→SQL safety) ────────────────────────
		// The text-to-SQL feature (modules/ai/nl2sql.go) lets the LLM query ONLY
		// these views. Each bakes in the org filter via the app.current_org GUC set
		// per-request in a read-only tx, so a generated query cannot escape its org
		// even if it omits a WHERE. current_setting(..., true) → NULL when unset,
		// which matches no rows (safe deny).
		`CREATE OR REPLACE VIEW v_machines AS
			SELECT m.id, m.name, m.type, m.status
			FROM machines m
			JOIN production_lines pl ON pl.id = m.production_line_id
			JOIN factories f         ON f.id = pl.factory_id
			WHERE f.organization_id = current_setting('app.current_org', true)::uuid`,
		`CREATE OR REPLACE VIEW v_machine_fields AS
			SELECT mf.machine_id, m.name AS machine_name, mf.key, mf.label, mf.unit
			FROM machine_fields mf
			JOIN v_machines m ON m.id = mf.machine_id`,
		// timestamp <= now() hides future-dated seed rows so "past N" windows have a
		// real upper bound (data runs ahead of the wall clock). Ask-Data-only view.
		`CREATE OR REPLACE VIEW v_telemetry AS
			SELECT t.machine_id, m.name AS machine_name, t.timestamp AS ts, t.data
			FROM telemetry_raw t
			JOIN v_machines m ON m.id = t.machine_id
			WHERE t.timestamp <= now()`,

		// ── ai_boards / ai_board_charts (saved NL→SQL→ECharts charts) ────────
		`CREATE TABLE IF NOT EXISTS ai_boards (
			id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			user_id         UUID        NOT NULL REFERENCES users(id),
			name            TEXT        NOT NULL,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS ai_board_charts (
			id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			board_id      UUID        NOT NULL REFERENCES ai_boards(id) ON DELETE CASCADE,
			question      TEXT        NOT NULL,
			sql           TEXT        NOT NULL,
			echart_option JSONB       NOT NULL DEFAULT '{}'::jsonb,
			"order"       INTEGER     NOT NULL DEFAULT 0,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// ── Source registry (upstream tables we do not control) ───────────────
		// One row per upstream table. The plant systems send whatever shape they
		// send — a shared wide counter table, or one table per freezer — so the
		// SHAPE IS DATA, not code: registering iqf4 is an INSERT here plus its
		// metric rows below, with no deploy. The normalizer builds its SELECT from
		// these expressions; nothing here ever comes from an end user.
		// The watermark is always ts_expr's value, so one TEXT (RFC3339) column
		// works whatever the upstream time column's type is — nj5_machines keeps
		// epoch seconds, the freezers keep timestamptz, and both have an index
		// matching their ts_expr. overlap_seconds re-reads a little of what was
		// already read: upstream clocks drift and rows can land slightly out of
		// order, and ON CONFLICT DO NOTHING makes the re-read free.
		`CREATE TABLE IF NOT EXISTS source_tables (
			id              SERIAL      PRIMARY KEY,
			factory_id      UUID        NOT NULL REFERENCES factories(id) ON DELETE CASCADE,
			table_name      TEXT        UNIQUE NOT NULL,
			shape           TEXT        NOT NULL DEFAULT 'wide_sensor',
			ts_expr         TEXT        NOT NULL,
			machine_expr    TEXT        NOT NULL,
			label_exprs     JSONB       NOT NULL DEFAULT '{}'::jsonb,
			overlap_seconds INT         NOT NULL DEFAULT 120,
			batch_rows      INT         NOT NULL DEFAULT 50000,
			reader          TEXT        NOT NULL DEFAULT 'poll',
			enabled         BOOLEAN     NOT NULL DEFAULT TRUE,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		// One row per numeric column we actually serve. `kind` is what makes a
		// wrong aggregation impossible: the compiler reads it, the model never
		// picks. A column that is absent here is invisible to the LLM — that is how
		// "rail_temp is all NULL" and "count_ng is always 0" are expressed now,
		// instead of a paragraph of prompt prose asking the model to refuse.
		// value_expr overrides column_name when the upstream type is not numeric
		// (a boolean flag needs ::int). column_name stays the drift-check key.
		`CREATE TABLE IF NOT EXISTS source_metrics (
			id              SERIAL  PRIMARY KEY,
			source_table_id INT     NOT NULL REFERENCES source_tables(id) ON DELETE CASCADE,
			column_name     TEXT    NOT NULL,
			value_expr      TEXT,
			field_key       TEXT    NOT NULL,
			kind            TEXT    NOT NULL DEFAULT 'gauge',
			unit            TEXT,
			sentinel        DOUBLE PRECISION,
			valid_min       DOUBLE PRECISION,
			valid_max       DOUBLE PRECISION,
			llm_note        TEXT,
			enabled         BOOLEAN NOT NULL DEFAULT TRUE,
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (source_table_id, column_name)
		)`,
		`CREATE TABLE IF NOT EXISTS source_state (
			source_table_id INT         PRIMARY KEY REFERENCES source_tables(id) ON DELETE CASCADE,
			last_watermark  TEXT,
			last_run_at     TIMESTAMPTZ,
			rows_ingested   BIGINT      NOT NULL DEFAULT 0,
			last_error      TEXT
		)`,

		// ── Canonical model ──────────────────────────────────────────────────
		// series = (machine, metric, labels). A cumulative counter's identity
		// includes its labels — IQF2/OutFeed/Carb is a different counter from
		// IQF2/OutFeed/Tray — so the delta is only ever computed per series.
		`CREATE TABLE IF NOT EXISTS series (
			id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			factory_id UUID        NOT NULL REFERENCES factories(id) ON DELETE CASCADE,
			machine_id UUID        NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
			field_key  TEXT        NOT NULL,
			labels     JSONB       NOT NULL DEFAULT '{}'::jsonb,
			first_seen TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_seen  TIMESTAMPTZ,
			UNIQUE (machine_id, field_key, labels)
		)`,
		// quality: 0=ok, 1=sentinel, 2=out of valid range. Bad readings are KEPT
		// and flagged, never dropped — the rollups filter on quality = 0, so a
		// mis-registered sentinel can be fixed by re-registering, not re-ingesting.
		`CREATE TABLE IF NOT EXISTS readings (
			series_id BIGINT           NOT NULL REFERENCES series(id) ON DELETE CASCADE,
			ts        TIMESTAMPTZ      NOT NULL,
			value     DOUBLE PRECISION,
			quality   SMALLINT         NOT NULL DEFAULT 0,
			UNIQUE (series_id, ts)
		)`,

		// ── Canonical serving views ──────────────────────────────────────────
		// Exactly two, plus the two rollups. This count does NOT grow when a
		// plant, machine, metric or upstream table is added — that is the whole
		// point of the registry. If you are about to add "v_<plant>…", add a
		// source_tables row instead.
		//
		// v_series is the catalog the prompt generator and listing questions read.
		//
		// security_invoker is what makes the factory policy actually apply here: by
		// default a view reads its base tables with the VIEW OWNER's rights, so
		// v_series would happily return another plant's series to a caller that
		// series itself would have blocked. With it, RLS is evaluated as the caller.
		`CREATE OR REPLACE VIEW v_series WITH (security_invoker = true) AS
		 SELECT s.id           AS series_id,
		        f.id           AS factory_id,
		        f.name         AS factory,
		        m.name         AS machine,
		        m.type         AS machine_type,
		        s.field_key,
		        cat.kind,
		        cat.unit,
		        s.labels,
		        s.last_seen
		 FROM series s
		 JOIN machines m         ON m.id  = s.machine_id
		 JOIN production_lines pl ON pl.id = m.production_line_id
		 JOIN factories f        ON f.id  = pl.factory_id
		 LEFT JOIN LATERAL (
		     SELECT sm.kind, sm.unit
		     FROM source_metrics sm
		     JOIN source_tables st ON st.id = sm.source_table_id
		     WHERE sm.field_key = s.field_key AND st.factory_id = s.factory_id
		     LIMIT 1
		 ) cat ON TRUE`,
		// v_readings is raw readings with their dimensions spelled out, so the
		// raw-SQL escape hatch has something safe to be pointed at.
		`CREATE OR REPLACE VIEW v_readings WITH (security_invoker = true) AS
		 SELECT r.series_id, r.ts, r.value, r.quality,
		        vs.factory_id, vs.machine, vs.machine_type, vs.field_key, vs.kind, vs.labels
		 FROM readings r
		 JOIN v_series vs ON vs.series_id = r.series_id`,
	}

	for _, stmt := range ddl {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("DDL failed: %w", err)
		}
	}

	// Apply schema migrations for alert_events columns added after the initial release.
	// CREATE TABLE IF NOT EXISTS never alters existing tables, so we patch here idempotently.
	alertEventsMigrations := []string{
		// Rename created_at → triggered_at (old column name)
		`DO $$ BEGIN
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='alert_events' AND column_name='created_at')
			THEN ALTER TABLE alert_events RENAME COLUMN created_at TO triggered_at; END IF;
		END $$`,
		// Add columns that may be missing from older DB instances
		`ALTER TABLE alert_events ADD COLUMN IF NOT EXISTS acknowledged_at TIMESTAMPTZ`,
		`ALTER TABLE alert_events ADD COLUMN IF NOT EXISTS acknowledged_by TEXT`,
		`ALTER TABLE alert_events ADD COLUMN IF NOT EXISTS resolved_at     TIMESTAMPTZ`,
		`ALTER TABLE alert_events ADD COLUMN IF NOT EXISTS resolved_by     TEXT`,
		// Ask-Data charts saved before the zoomable-window change have their range baked
		// into the SQL (now() - interval); NULL here means "run it as stored".
		`ALTER TABLE ai_board_charts ADD COLUMN IF NOT EXISTS window_hours DOUBLE PRECISION`,
		// Canonical charts store the query spec that produced them. It is recompiled on
		// replay, so it keeps working when the relations underneath change — which is
		// what lets the legacy views be retired without breaking saved boards.
		`ALTER TABLE ai_board_charts ADD COLUMN IF NOT EXISTS spec       JSONB`,
		`ALTER TABLE ai_board_charts ADD COLUMN IF NOT EXISTS factory_id UUID`,
		`ALTER TABLE ai_boards       ADD COLUMN IF NOT EXISTS factory_id UUID`,
	}
	for _, stmt := range alertEventsMigrations {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			fmt.Printf("⚠️  alert_events migration skipped: %v\n", err)
		}
	}

	// TimescaleDB hypertable (non-fatal if TimescaleDB is not available)
	_, err := pool.Exec(ctx, `
		SELECT create_hypertable(
			'telemetry_raw'::regclass,
			by_range('timestamp', INTERVAL '7 days'),
			if_not_exists => TRUE
		)
	`)
	if err != nil {
		fmt.Printf("⚠️  TimescaleDB hypertable skipped: %v\n", err)
	} else {
		fmt.Println("✅ TimescaleDB hypertable ready")
	}

	// Optional: compression policy (non-fatal)
	_, _ = pool.Exec(ctx, `
		ALTER TABLE telemetry_raw SET (
			timescaledb.compress,
			timescaledb.compress_segmentby = 'machine_id',
			timescaledb.compress_orderby   = 'timestamp DESC'
		)
	`)
	_, _ = pool.Exec(ctx, `
		SELECT add_compression_policy('telemetry_raw', INTERVAL '14 days', if_not_exists => TRUE)
	`)

	if err := ensureCanonicalTimescale(ctx, pool); err != nil {
		fmt.Printf("⚠️  canonical rollups skipped: %v\n", err)
	}

	// Performance indexes
	indexes := []string{
		// telemetry_raw
		`CREATE INDEX IF NOT EXISTS idx_tr_machine_ts      ON telemetry_raw (machine_id, timestamp DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_tr_machine_ts_good ON telemetry_raw (machine_id, timestamp DESC) WHERE quality = 'good'`,
		`CREATE INDEX IF NOT EXISTS idx_tr_data_gin        ON telemetry_raw USING GIN (data)`,
		`CREATE INDEX IF NOT EXISTS idx_tr_sku             ON telemetry_raw (machine_id, (data->>'sku'))`,
		// telemetry_aggregates
		`CREATE INDEX IF NOT EXISTS idx_ta_lookup ON telemetry_aggregates (machine_id, field, period, timestamp DESC)`,
		// machines
		`CREATE INDEX IF NOT EXISTS idx_machines_status ON machines (status)`,
		`CREATE INDEX IF NOT EXISTS idx_machines_line   ON machines (production_line_id)`,
		// machine_fields
		`CREATE INDEX IF NOT EXISTS idx_mf_machine ON machine_fields (machine_id)`,
		// dashboards
		`CREATE INDEX IF NOT EXISTS idx_dashboards_org  ON dashboards (organization_id)`,
		`CREATE INDEX IF NOT EXISTS idx_dashboards_user ON dashboards (user_id)`,
		// dashboard_widgets
		`CREATE INDEX IF NOT EXISTS idx_dw_dashboard ON dashboard_widgets (dashboard_id)`,
		`CREATE INDEX IF NOT EXISTS idx_dw_machine   ON dashboard_widgets (machine_id)`,
		// alerts
		`CREATE INDEX IF NOT EXISTS idx_alerts_machine ON alerts (machine_id, is_active)`,
		// alert_events
		`CREATE INDEX IF NOT EXISTS idx_ae_alert_ts ON alert_events (alert_id, triggered_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_ae_open     ON alert_events (triggered_at DESC) WHERE status = 'open'`,
		// ai
		`CREATE INDEX IF NOT EXISTS idx_aiconv_user ON ai_conversations (user_id, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_aimsg_conv  ON ai_messages (conversation_id, created_at ASC)`,
		// canonical model — series lookup drives every compiled query
		`CREATE INDEX IF NOT EXISTS idx_series_factory_field ON series (factory_id, field_key)`,
		`CREATE INDEX IF NOT EXISTS idx_series_machine       ON series (machine_id, field_key)`,
		`CREATE INDEX IF NOT EXISTS idx_series_labels        ON series USING GIN (labels)`,
		// user_organizations
		`CREATE INDEX IF NOT EXISTS idx_uo_user ON user_organizations (user_id)`,
		// audit
		`CREATE INDEX IF NOT EXISTS idx_audit_user     ON audit_logs (user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_resource ON audit_logs (resource, resource_id)`,
	}
	for _, idx := range indexes {
		if _, err := pool.Exec(ctx, idx); err != nil {
			fmt.Printf("⚠️  Index skipped: %v\n", err)
		}
	}

	fmt.Println("✅ Schema ready")
	return nil
}

// ensureCanonicalTimescale turns `readings` into a hypertable and builds the
// rollup ladder + row-level security on the canonical tables. Everything here
// needs TimescaleDB, so the caller treats a failure as non-fatal — the compiler
// falls back to reading `readings` directly when a rollup is missing.
//
// The ladder is 1h and 1d only. There is deliberately NO 1-minute rollup:
// readings arrive about once a minute already, so a 1m aggregate would have as
// many rows as the raw table and buy nothing. Narrow windows read raw.
//
// avg is NOT stored. sum_v + n are, so a hierarchical rollup can compute an
// exact weighted average (sum(sum_v)/sum(n)); an avg of hourly avgs would be
// wrong whenever buckets hold different numbers of readings.
func ensureCanonicalTimescale(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		SELECT create_hypertable(
			'readings'::regclass,
			by_range('ts', INTERVAL '7 days'),
			if_not_exists => TRUE
		)
	`); err != nil {
		return fmt.Errorf("hypertable: %w", err)
	}

	// Hourly rollup straight from raw readings. first_v/last_v are what make a
	// cumulative counter exact: the delta for a bucket is last_v - first_v, which
	// (unlike MAX-MIN) does not lose the piece that straddles a bucket edge and
	// makes a counter reset detectable as last_v < first_v.
	statements := []string{
		`CREATE MATERIALIZED VIEW IF NOT EXISTS readings_1h
		 WITH (timescaledb.continuous) AS
		 SELECT series_id,
		        time_bucket(INTERVAL '1 hour', ts) AS bucket,
		        sum(value)          AS sum_v,
		        count(*)            AS n,
		        min(value)          AS min_v,
		        max(value)          AS max_v,
		        first(value, ts)    AS first_v,
		        last(value, ts)     AS last_v
		 FROM readings
		 WHERE quality = 0
		 GROUP BY series_id, 2
		 WITH NO DATA`,
		// Daily rollup on top of the hourly one (hierarchical continuous
		// aggregate) — a year of questions reads ~10^5 rows instead of 32M.
		`CREATE MATERIALIZED VIEW IF NOT EXISTS readings_1d
		 WITH (timescaledb.continuous) AS
		 SELECT series_id,
		        time_bucket(INTERVAL '1 day', bucket) AS bucket,
		        sum(sum_v)              AS sum_v,
		        sum(n)                  AS n,
		        min(min_v)              AS min_v,
		        max(max_v)              AS max_v,
		        first(first_v, bucket)  AS first_v,
		        last(last_v, bucket)    AS last_v
		 FROM readings_1h
		 GROUP BY series_id, 2
		 WITH NO DATA`,
		// end_offset keeps the refresh off the newest rows so late arrivals are
		// still picked up; real-time aggregation covers the uncovered tail.
		// Real-time aggregation: the rollup is UNION'd with the raw rows newer than
		// the last refresh. Without this a question about the last hour reads an
		// empty bucket and answers zero, which looks like a stopped machine.
		`ALTER MATERIALIZED VIEW readings_1h SET (timescaledb.materialized_only = false)`,
		`ALTER MATERIALIZED VIEW readings_1d SET (timescaledb.materialized_only = false)`,
		`SELECT add_continuous_aggregate_policy('readings_1h',
			start_offset      => INTERVAL '7 days',
			end_offset        => INTERVAL '30 minutes',
			schedule_interval => INTERVAL '30 minutes',
			if_not_exists     => TRUE)`,
		`SELECT add_continuous_aggregate_policy('readings_1d',
			start_offset      => INTERVAL '30 days',
			end_offset        => INTERVAL '2 hours',
			schedule_interval => INTERVAL '1 hour',
			if_not_exists     => TRUE)`,
		`ALTER TABLE readings SET (
			timescaledb.compress,
			timescaledb.compress_segmentby = 'series_id',
			timescaledb.compress_orderby   = 'ts DESC'
		)`,
		`SELECT add_compression_policy('readings', INTERVAL '14 days', if_not_exists => TRUE)`,
	}
	for _, stmt := range statements {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			fmt.Printf("⚠️  canonical rollup statement skipped: %v\n", err)
		}
	}

	// Row-level security on the canonical tables only (the existing tables keep
	// their app-level org filters). FORCE is required: the app connects as the
	// table owner, and an owner bypasses a plain ENABLE.
	//
	// The policy reads `app.factory`, which every caller must set — an unset GUC
	// yields NULL and therefore zero rows, deliberately failing closed. Ask-Data
	// sets it in runScoped; the normalizer sets it per source table.
	//
	// ponytail: RLS is on `series` only, NOT on `readings`. TimescaleDB refuses
	// both a continuous aggregate ("cannot create continuous aggregate on
	// hypertable with row security") and columnstore ("columnstore cannot be used
	// on table with row security") on an RLS table, so `readings` can have the
	// rollup ladder or a row policy, not both — and losing the ladder would mean
	// reading 32M rows per question. What holds the boundary instead: a reading is
	// meaningless without its series, so every path joins `series` (v_readings,
	// every compiled query) and inherits the filter, and `readings` itself is in
	// deniedTables so no generated SQL can name it. Upgrade path if that is not
	// enough: give the app its own non-owner role and REVOKE SELECT on readings,
	// leaving only the views reachable.
	rls := []string{
		`ALTER TABLE series ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE series FORCE  ROW LEVEL SECURITY`,
		`DROP POLICY IF EXISTS series_by_factory ON series`,
		`CREATE POLICY series_by_factory ON series USING
			(factory_id::text = current_setting('app.factory', true))`,
		// A build before the constraint above was known left an inert policy here
		// (created fine, but ENABLE ROW LEVEL SECURITY had failed). Remove it so
		// `\d readings` does not suggest a protection that is not in force.
		`DROP POLICY IF EXISTS readings_by_series ON readings`,
	}
	for _, stmt := range rls {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			fmt.Printf("⚠️  canonical RLS statement skipped: %v\n", err)
		}
	}

	// A superuser (or BYPASSRLS) role ignores every policy, FORCE included — the
	// docker-compose default POSTGRES_USER is exactly that, so the isolation above
	// would be decorative and nobody would notice. Say so at startup.
	var role string
	var privileged bool
	if err := pool.QueryRow(ctx, `
		SELECT rolname, rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user
	`).Scan(&role, &privileged); err == nil && privileged {
		fmt.Printf("⚠️  %s is SUPERUSER/BYPASSRLS — factory row-level security is NOT enforced.\n"+
			"    Queries are still scoped by the compiler; to make the database enforce it too:\n"+
			"    ALTER ROLE %s NOSUPERUSER NOBYPASSRLS;\n", role, role)
	}

	fmt.Println("✅ Canonical rollups + RLS ready")
	return nil
}

// ─── Seed ─────────────────────────────────────────────────────────────────────

// EnsureSeed inserts the default org / factory / machines / users / dashboard
// if the database is empty. Safe to call repeatedly — skipped if data exists.
func EnsureSeed(ctx context.Context, pool *pgxpool.Pool) error {
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM organizations`).Scan(&count); err != nil {
		return fmt.Errorf("check org count: %w", err)
	}
	if count > 0 {
		return nil // already seeded
	}

	fmt.Print("🌱  Seeding database...")

	// Hash admin password (bcrypt cost 12, matching seed.ts)
	hash, err := bcrypt.GenerateFromPassword([]byte("Admin@1234"), 12)
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}

	// Helper pointers for nullable floats
	f := func(v float64) *float64 { return &v }

	stmts := []struct {
		sql  string
		args []interface{}
	}{
		// Organization
		{
			`INSERT INTO organizations (id, name, slug, plan, settings, created_at, updated_at)
			 VALUES ('00000000-0000-0000-0000-000000000001','ACME Foods Co.','acme-foods','pro','{}',NOW(),NOW())
			 ON CONFLICT (id) DO NOTHING`,
			nil,
		},
		// Factory
		{
			`INSERT INTO factories (id, organization_id, name, location, timezone, metadata, created_at, updated_at)
			 VALUES ('00000000-0000-0000-0000-000000000002','00000000-0000-0000-0000-000000000001',
			         'Bangkok Plant 1','Bangkok, Thailand','Asia/Bangkok','{}',NOW(),NOW())
			 ON CONFLICT (id) DO NOTHING`,
			nil,
		},
		// Production Lines
		{
			`INSERT INTO production_lines (id, factory_id, name, code, status, metadata, created_at, updated_at)
			 VALUES ('00000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000002',
			         'Line A — Packaging','LINE-A','active','{}',NOW(),NOW())
			 ON CONFLICT (id) DO NOTHING`,
			nil,
		},
		{
			`INSERT INTO production_lines (id, factory_id, name, code, status, metadata, created_at, updated_at)
			 VALUES ('00000000-0000-0000-0000-000000000004','00000000-0000-0000-0000-000000000002',
			         'Line B — Filling','LINE-B','active','{}',NOW(),NOW())
			 ON CONFLICT (id) DO NOTHING`,
			nil,
		},
		// Machines
		{
			`INSERT INTO machines (id, production_line_id, name, type, serial_number, model, manufacturer, status, metadata, created_at, updated_at)
			 VALUES ('00000000-0000-0000-0000-000000000005','00000000-0000-0000-0000-000000000003',
			         'Checkweigher CW-01','checkweigher','CW-2024-001','ProCheck X200','MettlerToledo','online',
			         '{"targetWeight":500,"tolerance":5}',NOW(),NOW())
			 ON CONFLICT (id) DO NOTHING`,
			nil,
		},
		{
			`INSERT INTO machines (id, production_line_id, name, type, serial_number, model, manufacturer, status, metadata, created_at, updated_at)
			 VALUES ('00000000-0000-0000-0000-000000000006','00000000-0000-0000-0000-000000000003',
			         'Temp Sensor TS-01','temperature_sensor','TS-2024-001','ThermoGuard Pro','Omega','online',
			         '{"location":"cold_storage_a"}',NOW(),NOW())
			 ON CONFLICT (id) DO NOTHING`,
			nil,
		},
		{
			`INSERT INTO machines (id, production_line_id, name, type, serial_number, model, manufacturer, status, metadata, created_at, updated_at)
			 VALUES ('00000000-0000-0000-0000-000000000007','00000000-0000-0000-0000-000000000003',
			         'Conveyor Belt CB-01','conveyor','CB-2024-001','FlexLine 500','Interroll','online',
			         '{"maxSpeed":2000}',NOW(),NOW())
			 ON CONFLICT (id) DO NOTHING`,
			nil,
		},
		{
			`INSERT INTO machines (id, production_line_id, name, type, serial_number, model, manufacturer, status, metadata, created_at, updated_at)
			 VALUES ('00000000-0000-0000-0000-000000000008','00000000-0000-0000-0000-000000000004',
			         'Vision AI Camera VC-01','vision_camera','VC-2024-001','SmartEye AI-Pro','Cognex','online',
			         '{"resolution":"4K","model":"defect-detection-v2"}',NOW(),NOW())
			 ON CONFLICT (id) DO NOTHING`,
			nil,
		},
		// Admin User
		{
			`INSERT INTO users (id, organization_id, email, name, password_hash, role, is_active, created_at, updated_at)
			 VALUES ('00000000-0000-0000-0000-000000000009','00000000-0000-0000-0000-000000000001',
			         'admin@acme-foods.com','Admin User',$1,'admin',TRUE,NOW(),NOW())
			 ON CONFLICT (id) DO NOTHING`,
			[]interface{}{string(hash)},
		},
		// Dashboard
		{
			`INSERT INTO dashboards (id, organization_id, user_id, name, description, is_public, is_default, tags, created_at, updated_at)
			 VALUES ('00000000-0000-0000-0000-000000000010','00000000-0000-0000-0000-000000000001',
			         '00000000-0000-0000-0000-000000000009','Production Overview',
			         'Main production monitoring dashboard',FALSE,TRUE,
			         ARRAY['production','overview'],NOW(),NOW())
			 ON CONFLICT (id) DO NOTHING`,
			nil,
		},
	}

	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("seed stmt: %w", err)
		}
	}

	// ── Machine Fields ────────────────────────────────────────────────────────
	type mf struct {
		machineID string
		key, label, unit string
		min, max, thr, upper, lower *float64
		prec  int
		isKey bool
	}

	fields := []mf{
		// CW-01 Checkweigher
		{"00000000-0000-0000-0000-000000000005", "weight",      "Weight",       "g",       f(0),   f(2000), f(500),  f(550),  f(450),  2, true},
		{"00000000-0000-0000-0000-000000000005", "speed",       "Belt Speed",   "ppm",     f(0),   f(120),  f(60),   f(66),   f(54),   2, false},
		{"00000000-0000-0000-0000-000000000005", "rejects",     "Reject Count", "pcs",     f(0),   f(9999), f(0),    f(3),    f(0),    2, false},
		{"00000000-0000-0000-0000-000000000005", "throughput",  "Throughput",   "pcs/min", f(0),   f(120),  f(60),   f(66),   f(54),   2, false},
		{"00000000-0000-0000-0000-000000000005", "status_code", "Status Code",  "",        nil,    nil,     nil,     nil,     nil,     2, false},
		// TS-01 Temp Sensor
		{"00000000-0000-0000-0000-000000000006", "temp",      "Temperature", "°C",  f(-20), f(80),  f(22), f(24.2), f(19.8), 2, true},
		{"00000000-0000-0000-0000-000000000006", "humidity",  "Humidity",    "%RH", f(0),   f(100), f(55), f(60.5), f(49.5), 2, false},
		{"00000000-0000-0000-0000-000000000006", "dew_point", "Dew Point",   "°C",  f(-30), f(60),  f(11), f(12.1), f(9.9),  2, false},
		// CB-01 Conveyor
		{"00000000-0000-0000-0000-000000000007", "speed",     "Belt Speed", "mm/s",  f(0), f(2000), f(1000), f(1100), f(900), 2, true},
		{"00000000-0000-0000-0000-000000000007", "load",      "Motor Load", "%",     f(0), f(100),  f(45),   f(49.5), f(40.5),2, false},
		{"00000000-0000-0000-0000-000000000007", "rpm",       "Motor RPM",  "rpm",   f(0), f(1500), f(750),  f(825),  f(675), 2, false},
		{"00000000-0000-0000-0000-000000000007", "vibration", "Vibration",  "mm/s²", f(0), f(50),   f(5),    f(5.5),  f(4.5), 2, false},
		// VC-01 Vision Camera
		{"00000000-0000-0000-0000-000000000008", "defect_rate", "Defect Rate",     "%",   f(0), f(100),    f(1),  f(1.1),  f(0.9),  2, true},
		{"00000000-0000-0000-0000-000000000008", "inspected",   "Items Inspected", "pcs", f(0), f(999999), nil,   nil,     nil,     2, false},
		{"00000000-0000-0000-0000-000000000008", "passed",      "Items Passed",    "pcs", f(0), f(999999), nil,   nil,     nil,     2, false},
		{"00000000-0000-0000-0000-000000000008", "failed",      "Items Failed",    "pcs", f(0), f(999999), nil,   nil,     nil,     2, false},
		{"00000000-0000-0000-0000-000000000008", "confidence",  "AI Confidence",   "%",   f(0), f(100),    f(97), f(99.9), f(87.3), 2, false},
	}

	// SKU dimension lives in the JSONB data (no column); per-minute good/reject piece counts are
	// declared here so other widgets/AI can see them. (sku itself is a string dimension fetched via /skus.)
	for _, mid := range []string{
		"00000000-0000-0000-0000-000000000005",
		"00000000-0000-0000-0000-000000000006",
		"00000000-0000-0000-0000-000000000007",
		"00000000-0000-0000-0000-000000000008",
	} {
		fields = append(fields,
			mf{mid, "good", "Good (pcs)", "pcs", f(0), f(9999), nil, nil, nil, 0, false},
			mf{mid, "reject", "Reject (pcs)", "pcs", f(0), f(9999), nil, nil, nil, 0, false},
		)
	}

	for _, field := range fields {
		unit := (*string)(nil)
		if field.unit != "" {
			u := field.unit
			unit = &u
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO machine_fields
				(id, machine_id, key, label, unit, data_type, min, max, threshold, upper_limit, lower_limit, precision, is_key, created_at)
			VALUES
				(gen_random_uuid(), $1, $2, $3, $4, 'number', $5, $6, $7, $8, $9, $10, $11, NOW())
			ON CONFLICT (machine_id, key) DO NOTHING`,
			field.machineID, field.key, field.label, unit,
			field.min, field.max, field.thr, field.upper, field.lower,
			field.prec, field.isKey,
		)
		if err != nil {
			return fmt.Errorf("field %s.%s: %w", field.machineID[len(field.machineID)-4:], field.key, err)
		}
	}

	// ── Dashboard Widgets ─────────────────────────────────────────────────────
	type widget struct {
		id, machineID, wType, title, layout, config string
	}
	cw := "00000000-0000-0000-0000-000000000005"
	ts := "00000000-0000-0000-0000-000000000006"
	cb := "00000000-0000-0000-0000-000000000007"

	widgets := []widget{
		{"00000000-0000-0000-0000-000000000014", cw, "line-chart", "Weight Over Time",
			`{"x":0,"y":0,"w":6,"h":4}`, `{"field":"weight","timeRange":"1h","color":"#3b82f6"}`},
		{"00000000-0000-0000-0000-000000000015", cw, "gauge", "Current Weight",
			`{"x":6,"y":0,"w":3,"h":4}`, `{"field":"weight","min":400,"max":600,"unit":"g"}`},
		{"00000000-0000-0000-0000-000000000016", ts, "kpi-card", "Temperature",
			`{"x":9,"y":0,"w":3,"h":2}`, `{"field":"temp","unit":"°C","precision":1}`},
		{"00000000-0000-0000-0000-000000000017", cw, "kpi-card", "Throughput",
			`{"x":9,"y":2,"w":3,"h":2}`, `{"field":"throughput","unit":"pcs/min","precision":0}`},
		{"00000000-0000-0000-0000-000000000018", cb, "line-chart", "Conveyor Speed",
			`{"x":0,"y":4,"w":6,"h":4}`, `{"field":"speed","timeRange":"30m","color":"#10b981"}`},
		{"00000000-0000-0000-0000-000000000019", "", "alarm-panel", "Active Alerts",
			`{"x":6,"y":4,"w":6,"h":4}`, `{"maxItems":10,"severities":["warning","critical"]}`},
	}

	for _, w := range widgets {
		var machineID interface{} = w.machineID
		if w.machineID == "" {
			machineID = nil
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO dashboard_widgets (id, dashboard_id, machine_id, widget_type, title, layout, config, created_at, updated_at)
			VALUES ($1,'00000000-0000-0000-0000-000000000010',$2,$3,$4,$5::jsonb,$6::jsonb,NOW(),NOW())
			ON CONFLICT (id) DO NOTHING`,
			w.id, machineID, w.wType, w.title, w.layout, w.config,
		)
		if err != nil {
			return fmt.Errorf("widget %s: %w", w.id[len(w.id)-4:], err)
		}
	}

	// ── Alert Rules ───────────────────────────────────────────────────────────
	type alertRule struct {
		id, machineID, name, field, condition, severity string
		threshold                                        float64
	}
	alertRules := []alertRule{
		{"00000000-0000-0000-0000-000000000011", cw, "Weight Over Tolerance",  "weight", "gt", "warning",  510},
		{"00000000-0000-0000-0000-000000000012", cw, "Weight Under Tolerance", "weight", "lt", "critical", 490},
		{"00000000-0000-0000-0000-000000000013", ts, "High Temperature",       "temp",   "gt", "critical",  35},
	}
	for _, a := range alertRules {
		_, err := pool.Exec(ctx, `
			INSERT INTO alerts (id, machine_id, name, field, condition, threshold, severity, is_active, cooldown_sec, notify_email, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,300,'{}',NOW(),NOW())
			ON CONFLICT (id) DO NOTHING`,
			a.id, a.machineID, a.name, a.field, a.condition, a.threshold, a.severity,
		)
		if err != nil {
			return fmt.Errorf("alert %s: %w", a.name, err)
		}
	}

	fmt.Println(" done ✅")
	return nil
}

// ─── Demo Orgs ──────────────────────────────────────────────────────────────────

// EnsureDemoOrgs seeds 3 mock organizations (each with its own factory, machines,
// fields, admin user, and a default dashboard) so the login page can offer a
// multi-tenant demo. Idempotent: fixed UUIDs + ON CONFLICT DO NOTHING, so it runs
// safely on every startup regardless of whether the main seed has run.
// All admins share the password "Demo@1234". No telemetry is seeded.
func EnsureDemoOrgs(ctx context.Context, pool *pgxpool.Pool) error {
	hash, err := bcrypt.GenerateFromPassword([]byte("Demo@1234"), 12)
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}
	f := func(v float64) *float64 { return &v }

	type field struct {
		key, label, unit           string
		min, max, thr, upper, lower *float64
		isKey                      bool
	}
	type machine struct {
		id, name, mtype string
		fields          []field
	}
	type widget struct {
		id, machineID, wType, title, layout, config string
	}
	type demoOrg struct {
		orgID, slug, name           string
		factoryID, lineID, lineName string
		userID, adminEmail          string
		dashboardID                 string
		machines                    []machine
		widgets                     []widget
	}

	orgs := []demoOrg{
		{
			orgID: "00000000-0000-0000-0000-0000000a0000", slug: "nova-bottling", name: "Nova Bottling",
			factoryID: "00000000-0000-0000-0000-0000000a0001", lineID: "00000000-0000-0000-0000-0000000a0002", lineName: "Line 1 — Bottling",
			userID: "00000000-0000-0000-0000-0000000a0003", adminEmail: "admin@nova-bottling.com",
			dashboardID: "00000000-0000-0000-0000-0000000a0004",
			machines: []machine{
				{"00000000-0000-0000-0000-0000000a0010", "Filler FL-01", "filler", []field{
					{"fill_volume", "Fill Volume", "ml", f(0), f(1000), f(500), f(515), f(485), true},
					{"fill_rate", "Fill Rate", "bpm", f(0), f(600), f(300), f(330), f(270), false},
				}},
				{"00000000-0000-0000-0000-0000000a0011", "Capper CP-01", "capper", []field{
					{"torque", "Cap Torque", "Nm", f(0), f(5), f(2), f(2.2), f(1.8), true},
					{"speed", "Speed", "bpm", f(0), f(600), f(300), f(330), f(270), false},
				}},
				{"00000000-0000-0000-0000-0000000a0012", "Labeler LB-01", "labeler", []field{
					{"throughput", "Throughput", "bpm", f(0), f(600), f(300), f(330), f(270), true},
					{"misalign", "Misaligned", "pcs", f(0), f(9999), f(0), f(5), f(0), false},
				}},
			},
			widgets: []widget{
				{"00000000-0000-0000-0000-0000000a0020", "00000000-0000-0000-0000-0000000a0010", "line-chart", "Fill Volume", `{"x":0,"y":0,"w":6,"h":4}`, `{"field":"fill_volume","timeRange":"1h","color":"#3b82f6"}`},
				{"00000000-0000-0000-0000-0000000a0021", "00000000-0000-0000-0000-0000000a0011", "gauge", "Cap Torque", `{"x":6,"y":0,"w":3,"h":4}`, `{"field":"torque","min":0,"max":5,"unit":"Nm"}`},
				{"00000000-0000-0000-0000-0000000a0022", "00000000-0000-0000-0000-0000000a0012", "kpi-card", "Throughput", `{"x":9,"y":0,"w":3,"h":4}`, `{"field":"throughput","unit":"bpm","precision":0}`},
				{"00000000-0000-0000-0000-0000000a0023", "", "alarm-panel", "Active Alerts", `{"x":0,"y":4,"w":12,"h":4}`, `{"maxItems":10,"severities":["warning","critical"]}`},
			},
		},
		{
			orgID: "00000000-0000-0000-0000-0000000b0000", slug: "sakura-textiles", name: "Sakura Textiles",
			factoryID: "00000000-0000-0000-0000-0000000b0001", lineID: "00000000-0000-0000-0000-0000000b0002", lineName: "Line 1 — Weaving",
			userID: "00000000-0000-0000-0000-0000000b0003", adminEmail: "admin@sakura-textiles.com",
			dashboardID: "00000000-0000-0000-0000-0000000b0004",
			machines: []machine{
				{"00000000-0000-0000-0000-0000000b0010", "Loom LM-01", "loom", []field{
					{"rpm", "Loom RPM", "rpm", f(0), f(800), f(400), f(440), f(360), true},
					{"tension", "Warp Tension", "N", f(0), f(200), f(100), f(110), f(90), false},
				}},
				{"00000000-0000-0000-0000-0000000b0011", "Dyeing Vat DV-01", "dyeing", []field{
					{"temp", "Bath Temp", "°C", f(0), f(120), f(60), f(66), f(54), true},
					{"ph", "pH", "", f(0), f(14), f(7), f(7.5), f(6.5), false},
				}},
				{"00000000-0000-0000-0000-0000000b0012", "Dryer DR-01", "dryer", []field{
					{"temp", "Air Temp", "°C", f(0), f(200), f(120), f(132), f(108), true},
					{"humidity", "Humidity", "%RH", f(0), f(100), f(30), f(33), f(27), false},
				}},
			},
			widgets: []widget{
				{"00000000-0000-0000-0000-0000000b0020", "00000000-0000-0000-0000-0000000b0010", "line-chart", "Loom RPM", `{"x":0,"y":0,"w":6,"h":4}`, `{"field":"rpm","timeRange":"1h","color":"#10b981"}`},
				{"00000000-0000-0000-0000-0000000b0021", "00000000-0000-0000-0000-0000000b0011", "gauge", "Bath Temp", `{"x":6,"y":0,"w":3,"h":4}`, `{"field":"temp","min":0,"max":120,"unit":"°C"}`},
				{"00000000-0000-0000-0000-0000000b0022", "00000000-0000-0000-0000-0000000b0012", "kpi-card", "Dryer Humidity", `{"x":9,"y":0,"w":3,"h":4}`, `{"field":"humidity","unit":"%RH","precision":0}`},
				{"00000000-0000-0000-0000-0000000b0023", "", "alarm-panel", "Active Alerts", `{"x":0,"y":4,"w":12,"h":4}`, `{"maxItems":10,"severities":["warning","critical"]}`},
			},
		},
		{
			orgID: "00000000-0000-0000-0000-0000000c0000", slug: "andes-brewing", name: "Andes Brewing",
			factoryID: "00000000-0000-0000-0000-0000000c0001", lineID: "00000000-0000-0000-0000-0000000c0002", lineName: "Line 1 — Brewhouse",
			userID: "00000000-0000-0000-0000-0000000c0003", adminEmail: "admin@andes-brewing.com",
			dashboardID: "00000000-0000-0000-0000-0000000c0004",
			machines: []machine{
				{"00000000-0000-0000-0000-0000000c0010", "Mash Tun MT-01", "mash", []field{
					{"temp", "Mash Temp", "°C", f(0), f(100), f(65), f(68), f(62), true},
					{"gravity", "Specific Gravity", "SG", f(1), f(1.1), f(1.05), f(1.06), f(1.04), false},
				}},
				{"00000000-0000-0000-0000-0000000c0011", "Fermenter FV-01", "fermenter", []field{
					{"temp", "Ferment Temp", "°C", f(0), f(40), f(18), f(20), f(16), true},
					{"pressure", "Pressure", "bar", f(0), f(3), f(1), f(1.2), f(0.8), false},
				}},
				{"00000000-0000-0000-0000-0000000c0012", "Bottling Line BL-01", "bottling", []field{
					{"throughput", "Throughput", "bpm", f(0), f(500), f(250), f(275), f(225), true},
					{"fill_level", "Fill Level", "%", f(0), f(100), f(98), f(100), f(96), false},
				}},
			},
			widgets: []widget{
				{"00000000-0000-0000-0000-0000000c0020", "00000000-0000-0000-0000-0000000c0010", "line-chart", "Mash Temp", `{"x":0,"y":0,"w":6,"h":4}`, `{"field":"temp","timeRange":"1h","color":"#f59e0b"}`},
				{"00000000-0000-0000-0000-0000000c0021", "00000000-0000-0000-0000-0000000c0011", "gauge", "Fermenter Pressure", `{"x":6,"y":0,"w":3,"h":4}`, `{"field":"pressure","min":0,"max":3,"unit":"bar"}`},
				{"00000000-0000-0000-0000-0000000c0022", "00000000-0000-0000-0000-0000000c0012", "kpi-card", "Bottling Throughput", `{"x":9,"y":0,"w":3,"h":4}`, `{"field":"throughput","unit":"bpm","precision":0}`},
				{"00000000-0000-0000-0000-0000000c0023", "", "alarm-panel", "Active Alerts", `{"x":0,"y":4,"w":12,"h":4}`, `{"maxItems":10,"severities":["warning","critical"]}`},
			},
		},
	}

	for _, o := range orgs {
		if _, err := pool.Exec(ctx, `INSERT INTO organizations (id, name, slug, plan) VALUES ($1,$2,$3,'starter') ON CONFLICT (id) DO NOTHING`,
			o.orgID, o.name, o.slug); err != nil {
			return fmt.Errorf("org %s: %w", o.slug, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO factories (id, organization_id, name, timezone) VALUES ($1,$2,$3,'UTC') ON CONFLICT (id) DO NOTHING`,
			o.factoryID, o.orgID, o.name+" Plant"); err != nil {
			return fmt.Errorf("factory %s: %w", o.slug, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO production_lines (id, factory_id, name, status) VALUES ($1,$2,$3,'active') ON CONFLICT (id) DO NOTHING`,
			o.lineID, o.factoryID, o.lineName); err != nil {
			return fmt.Errorf("line %s: %w", o.slug, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, organization_id, email, name, password_hash, role, is_active)
			VALUES ($1,$2,$3,'Demo Admin',$4,'admin',TRUE) ON CONFLICT (id) DO NOTHING`,
			o.userID, o.orgID, o.adminEmail, string(hash)); err != nil {
			return fmt.Errorf("user %s: %w", o.slug, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO dashboards (id, organization_id, user_id, name, description, is_default, tags)
			VALUES ($1,$2,$3,'Production Overview','Demo dashboard',TRUE,ARRAY['demo']) ON CONFLICT (id) DO NOTHING`,
			o.dashboardID, o.orgID, o.userID); err != nil {
			return fmt.Errorf("dashboard %s: %w", o.slug, err)
		}

		for _, m := range o.machines {
			if _, err := pool.Exec(ctx, `INSERT INTO machines (id, production_line_id, name, type, status)
				VALUES ($1,$2,$3,$4,'online') ON CONFLICT (id) DO NOTHING`,
				m.id, o.lineID, m.name, m.mtype); err != nil {
				return fmt.Errorf("machine %s: %w", m.name, err)
			}
			for _, fld := range m.fields {
				unit := (*string)(nil)
				if fld.unit != "" {
					u := fld.unit
					unit = &u
				}
				if _, err := pool.Exec(ctx, `INSERT INTO machine_fields
					(id, machine_id, key, label, unit, data_type, min, max, threshold, upper_limit, lower_limit, precision, is_key)
					VALUES (gen_random_uuid(),$1,$2,$3,$4,'number',$5,$6,$7,$8,$9,2,$10)
					ON CONFLICT (machine_id, key) DO NOTHING`,
					m.id, fld.key, fld.label, unit, fld.min, fld.max, fld.thr, fld.upper, fld.lower, fld.isKey); err != nil {
					return fmt.Errorf("field %s.%s: %w", m.name, fld.key, err)
				}
			}
		}

		for _, w := range o.widgets {
			var machineID interface{} = w.machineID
			if w.machineID == "" {
				machineID = nil
			}
			if _, err := pool.Exec(ctx, `INSERT INTO dashboard_widgets (id, dashboard_id, machine_id, widget_type, title, layout, config)
				VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb) ON CONFLICT (id) DO NOTHING`,
				w.id, o.dashboardID, machineID, w.wType, w.title, w.layout, w.config); err != nil {
				return fmt.Errorf("widget %s: %w", w.id[len(w.id)-4:], err)
			}
		}
	}

	// Limited non-admin user to demonstrate the org picker's locked state.
	// Member of ACME + Nova only → Sakura + Andes show darkened for them.
	// (Admins bypass membership and see every org.)
	const (
		viewerID = "00000000-0000-0000-0000-0000000d0001"
		acmeOrg  = "00000000-0000-0000-0000-000000000001"
		novaOrg  = "00000000-0000-0000-0000-0000000a0000"
	)
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, organization_id, email, name, password_hash, role, is_active)
		VALUES ($1,$2,'viewer@acme-foods.com','Demo Viewer',$3,'viewer',TRUE) ON CONFLICT (id) DO NOTHING`,
		viewerID, acmeOrg, string(hash)); err != nil {
		return fmt.Errorf("viewer user: %w", err)
	}
	for _, orgID := range []string{acmeOrg, novaOrg} {
		if _, err := pool.Exec(ctx, `INSERT INTO user_organizations (user_id, organization_id)
			VALUES ($1,$2) ON CONFLICT DO NOTHING`, viewerID, orgID); err != nil {
			return fmt.Errorf("membership %s: %w", orgID, err)
		}
	}

	fmt.Println("✅ Demo orgs ready")
	return nil
}
