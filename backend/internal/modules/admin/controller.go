// Package admin exposes the source registry to an operator: what is registered,
// how ingest is going, whether the upstream schema has drifted, and which
// auto-discovered machines still need a human to look at them.
//
// The drift endpoint exists because the one thing we do NOT control is the shape
// of the plant's tables. When they rename or retype a column, the normalizer must
// not quietly skip it — this is how someone finds out on purpose.
package admin

import (
	"context"

	"iot-dashboard/internal/database"
	"iot-dashboard/internal/middleware"

	"github.com/gofiber/fiber/v2"
)

// GET /admin/sources — registry plus ingest state.
func ListSources(c *fiber.Ctx) error {
	rows, err := database.Pool.Query(context.Background(), `
		SELECT st.id, st.table_name, st.shape, f.name, st.enabled,
		       COALESCE(ss.last_watermark, ''), ss.last_run_at,
		       COALESCE(ss.rows_ingested, 0), COALESCE(ss.last_error, ''),
		       (SELECT count(*) FROM source_metrics sm WHERE sm.source_table_id = st.id AND sm.enabled)
		FROM source_tables st
		JOIN factories f       ON f.id = st.factory_id
		LEFT JOIN source_state ss ON ss.source_table_id = st.id
		ORDER BY f.name, st.table_name`)
	if err != nil {
		return fail(c, 500, err.Error())
	}
	defer rows.Close()

	out := []fiber.Map{}
	for rows.Next() {
		var id, metrics int
		var table, shape, factory, watermark, lastErr string
		var enabled bool
		var lastRun any
		var ingested int64
		if err := rows.Scan(&id, &table, &shape, &factory, &enabled,
			&watermark, &lastRun, &ingested, &lastErr, &metrics); err != nil {
			return fail(c, 500, err.Error())
		}
		out = append(out, fiber.Map{
			"id": id, "table": table, "shape": shape, "factory": factory,
			"enabled": enabled, "metrics": metrics, "lastWatermark": watermark,
			"lastRunAt": lastRun, "rowsIngested": ingested, "lastError": lastErr,
		})
	}
	return c.JSON(fiber.Map{"success": true, "data": out})
}

// GET /admin/sources/drift — compare what upstream actually has against what the
// registry says. Three answers matter:
//
//	missing    — a registered column is gone: that metric stops arriving NOW
//	unregistered — a new numeric column nobody has decided about yet
//	retyped    — the column still exists but is no longer numeric-compatible
func CheckDrift(c *fiber.Ctx) error {
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		SELECT st.id, st.table_name FROM source_tables st ORDER BY st.table_name`)
	if err != nil {
		return fail(c, 500, err.Error())
	}
	type src struct {
		id    int
		table string
	}
	var sources []src
	for rows.Next() {
		var s src
		if err := rows.Scan(&s.id, &s.table); err != nil {
			rows.Close()
			return fail(c, 500, err.Error())
		}
		sources = append(sources, s)
	}
	rows.Close()

	out := []fiber.Map{}
	for _, s := range sources {
		live := map[string]string{}
		lrows, err := database.Pool.Query(ctx, `
			SELECT column_name, data_type
			FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1`, s.table)
		if err != nil {
			return fail(c, 500, err.Error())
		}
		for lrows.Next() {
			var name, dtype string
			if err := lrows.Scan(&name, &dtype); err != nil {
				lrows.Close()
				return fail(c, 500, err.Error())
			}
			live[name] = dtype
		}
		lrows.Close()

		if len(live) == 0 {
			out = append(out, fiber.Map{"table": s.table, "tableMissing": true})
			continue
		}

		registered := map[string]bool{}
		mrows, err := database.Pool.Query(ctx, `
			SELECT column_name FROM source_metrics WHERE source_table_id = $1`, s.id)
		if err != nil {
			return fail(c, 500, err.Error())
		}
		missing, retyped := []string{}, []string{}
		for mrows.Next() {
			var col string
			if err := mrows.Scan(&col); err != nil {
				mrows.Close()
				return fail(c, 500, err.Error())
			}
			registered[col] = true
			dtype, ok := live[col]
			switch {
			case !ok:
				missing = append(missing, col)
			case !numericish(dtype):
				retyped = append(retyped, col+" is now "+dtype)
			}
		}
		mrows.Close()

		unregistered := []string{}
		for col, dtype := range live {
			if !registered[col] && numericish(dtype) && !isDimensionColumn(col) {
				unregistered = append(unregistered, col)
			}
		}

		entry := fiber.Map{"table": s.table, "columns": len(live)}
		if len(missing) > 0 {
			entry["missing"] = missing
		}
		if len(retyped) > 0 {
			entry["retyped"] = retyped
		}
		if len(unregistered) > 0 {
			entry["unregistered"] = unregistered
		}
		entry["ok"] = len(missing) == 0 && len(retyped) == 0
		out = append(out, entry)
	}
	return c.JSON(fiber.Map{"success": true, "data": out})
}

// numericish reports whether a column can be read as a number, directly or via
// the value_expr cast the registry allows (a boolean flag becomes 0/1).
func numericish(dataType string) bool {
	switch dataType {
	case "smallint", "integer", "bigint", "real", "double precision",
		"numeric", "boolean":
		return true
	}
	return false
}

// isDimensionColumn keeps obvious identity/time columns out of the "you forgot to
// register this metric" list — they are dimensions or the time axis, not readings.
func isDimensionColumn(col string) bool {
	switch col {
	case "id", "time_stamp", "created_at", "updated_at", "plant_id", "series_id":
		return true
	}
	return false
}

// GET /admin/machines/pending — machines the normalizer created on its own.
func PendingMachines(c *fiber.Ctx) error {
	rows, err := database.Pool.Query(context.Background(), `
		SELECT m.id::text, m.name, m.type, f.name,
		       COALESCE(m.metadata->>'source',''), m.created_at
		FROM machines m
		JOIN production_lines pl ON pl.id = m.production_line_id
		JOIN factories f         ON f.id = pl.factory_id
		WHERE m.metadata->>'discovery' = 'auto'
		  AND COALESCE(m.metadata->>'confirmed','false') <> 'true'
		ORDER BY m.created_at DESC`)
	if err != nil {
		return fail(c, 500, err.Error())
	}
	defer rows.Close()
	out := []fiber.Map{}
	for rows.Next() {
		var id, name, mtype, factory, source string
		var created any
		if err := rows.Scan(&id, &name, &mtype, &factory, &source, &created); err != nil {
			return fail(c, 500, err.Error())
		}
		out = append(out, fiber.Map{"id": id, "name": name, "type": mtype,
			"factory": factory, "source": source, "discoveredAt": created})
	}
	return c.JSON(fiber.Map{"success": true, "data": out})
}

// POST /admin/machines/:id/confirm {name?, type?} — accept a discovered machine,
// optionally giving it a friendlier name than the code upstream sends.
func ConfirmMachine(c *fiber.Ctx) error {
	var body struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	_ = c.BodyParser(&body)

	ct, err := database.Pool.Exec(context.Background(), `
		UPDATE machines
		SET name       = COALESCE(NULLIF($2,''), name),
		    type       = COALESCE(NULLIF($3,''), type),
		    metadata   = metadata || '{"confirmed":true}'::jsonb,
		    updated_at = NOW()
		WHERE id = $1::uuid`, c.Params("id"), body.Name, body.Type)
	if err != nil {
		return fail(c, 500, err.Error())
	}
	if ct.RowsAffected() == 0 {
		return fail(c, 404, "machine not found")
	}
	return c.JSON(fiber.Map{"success": true})
}

func fail(c *fiber.Ctx, code int, msg string) error {
	return c.Status(code).JSON(fiber.Map{"success": false, "error": fiber.Map{"message": msg}})
}

func RegisterRoutes(r fiber.Router) {
	r.Use(middleware.Authenticate)
	// Registry contents describe every plant, so this is admin-only.
	r.Use(middleware.RequireRole("admin"))

	r.Get("/sources", ListSources)
	r.Get("/sources/drift", CheckDrift)
	r.Get("/machines/pending", PendingMachines)
	r.Post("/machines/:id/confirm", ConfirmMachine)
}
