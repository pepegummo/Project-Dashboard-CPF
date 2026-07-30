package ai

// Full-loop /ai/ask integration test: every askCase from nl2sql_live_test.go driven
// through the REAL AskData handler — HTTP parse → emitSQL → validateSQL → runScoped
// (live TimescaleDB) → hasNumericColumn → emitEChart → sanitize → judge → repair →
// JSON response — exactly the path a real user's request takes, minus only JWT
// verification (auth is stubbed by injecting the same Locals the middleware would).
//
// Needs BOTH a live AI key (liveKeyOrSkip) and a reachable DATABASE_URL with the
// seeded/backfilled org data (machine "CW-01" must exist); skips fast otherwise. Run:
//   cd backend && go test ./internal/modules/ai/ -run AskDataFullLoopLive -v -timeout 90m

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"iot-dashboard/internal/database"
	"iot-dashboard/internal/middleware"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// askResponse mirrors AskData's fiber.Map response shape.
type askResponse struct {
	Success bool `json:"success"`
	Data    struct {
		SQL           string          `json:"sql"`
		Columns       []string        `json:"columns"`
		Rows          [][]any         `json:"rows"`
		EchartOption  json.RawMessage `json:"echartOption"`
		Answer        string          `json:"answer"`
		Clarification string          `json:"clarification"`
		WindowHours   float64         `json:"windowHours"`
	} `json:"data"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func TestAskDataFullLoopLive(t *testing.T) {
	liveKeyOrSkip(t)

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set — skipping full-loop test")
	}
	ctx := context.Background()
	// Direct pool, not database.Connect — that helper retries 15×3s on a down DB.
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("DB config invalid: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("DB unreachable: %v", err)
	}
	database.Pool = pool
	t.Cleanup(func() { database.Pool = nil; pool.Close() })

	// machines has no org column — org comes via production_lines → factories,
	// the same join v_machines bakes in (migrate.go).
	var orgID string
	if err := pool.QueryRow(ctx,
		`SELECT f.organization_id::text
		 FROM machines m
		 JOIN production_lines pl ON pl.id = m.production_line_id
		 JOIN factories f         ON f.id = pl.factory_id
		 WHERE m.name ILIKE '%CW-01%' LIMIT 1`,
	).Scan(&orgID); err != nil {
		t.Skipf("seeded machine CW-01 not found (run backfill first): %v", err)
	}

	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("user", &middleware.JwtClaims{OrgId: orgID, Role: "admin"})
		return c.Next()
	})
	app.Post("/ai/ask", AskData)

	askStart := time.Now()
	var askRows [][]string // case | expect | result | tokens | time
	var askTok int64

	for _, c := range askCases {
		t.Run(c.name, func(t *testing.T) {
			pace()

			body := map[string]any{"question": c.question}
			if c.prev != nil {
				prevCtx := map[string]string{"question": c.prev.Question}
				if c.prev.SQL != "" {
					prevCtx["sql"] = c.prev.SQL
				}
				if c.prev.Clarification != "" {
					prevCtx["clarification"] = c.prev.Clarification
				}
				body["context"] = prevCtx
			}
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", "/ai/ask", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")

			// 60s > the handler's own 45s ctx, so a timeout surfaces as the handler's
			// error response, not a cut connection.
			resetTokenMeter()
			caseStart := time.Now()
			resp, err := app.Test(req, 60000)
			caseTok := loadTokenMeter()
			// Record per-case cost + result before any assertion FailNow's the subtest.
			// The defer fills the result cell — Goexit still runs defers, so a t.Fatalf
			// below is still recorded as FAIL (mirrors chat_fullloop's outcome pattern).
			rowIdx := len(askRows)
			askRows = append(askRows, []string{c.name, c.expect, "PASS",
				fmt.Sprintf("%d", caseTok), fmt.Sprintf("%.1fs", time.Since(caseStart).Seconds())})
			askTok += caseTok
			defer func() {
				if t.Failed() {
					askRows[rowIdx][2] = "FAIL"
				}
			}()
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()
			var out askResponse
			if derr := json.NewDecoder(resp.Body).Decode(&out); derr != nil {
				t.Fatalf("bad response JSON (status %d): %v", resp.StatusCode, derr)
			}

			switch c.expect {
			case "notdata":
				if resp.StatusCode != 200 || out.Data.Answer == "" {
					t.Fatalf("want a prose answer, got status=%d answer=%q sql=%q clarification=%q err=%q",
						resp.StatusCode, out.Data.Answer, out.Data.SQL, out.Data.Clarification, out.Error.Message)
				}

			case "clarify":
				if resp.StatusCode != 200 || out.Data.Clarification == "" {
					t.Fatalf("want a clarification, got status=%d sql=%q answer=%q err=%q",
						resp.StatusCode, out.Data.SQL, out.Data.Answer, out.Error.Message)
				}
				if out.Data.SQL != "" {
					t.Fatalf("a clarification turn must not also carry SQL, got sql=%q", out.Data.SQL)
				}

			case "sql":
				if resp.StatusCode != 200 {
					t.Fatalf("status %d, error: %s", resp.StatusCode, out.Error.Message)
				}
				if out.Data.Clarification != "" {
					t.Fatalf("expected sql, got clarification: %q", out.Data.Clarification)
				}
				if out.Data.Answer != "" {
					t.Fatalf("expected sql, got prose answer: %q", out.Data.Answer)
				}
				if out.Data.SQL == "" || len(out.Data.Columns) == 0 {
					t.Fatalf("expected executed SQL + columns, got sql=%q columns=%v", out.Data.SQL, out.Data.Columns)
				}
				low := strings.ToLower(out.Data.SQL)
				for _, want := range c.sqlHas {
					if !strings.Contains(low, want) {
						t.Errorf("sql missing %q\nfull sql: %s", want, out.Data.SQL)
					}
				}
				if !c.sqlHasAnyOK(low) {
					t.Errorf("sql has none of %v\nfull sql: %s", c.sqlHasAny, out.Data.SQL)
				}
				for _, bad := range c.sqlNot {
					if strings.Contains(low, bad) {
						t.Errorf("sql contains forbidden %q\nfull sql: %s", bad, out.Data.SQL)
					}
				}
				if c.windowHours > 0 && out.Data.WindowHours != c.windowHours {
					t.Errorf("windowHours = %v, want %v\nfull sql: %s", out.Data.WindowHours, c.windowHours, out.Data.SQL)
				}
				if len(out.Data.Rows) == 0 {
					t.Logf("note: query ran but returned 0 rows — chart/judge stages skipped")
				}
				if c.chart != "" && len(out.Data.Rows) > 0 {
					var opt struct {
						Series []struct {
							Type string `json:"type"`
						} `json:"series"`
					}
					if perr := json.Unmarshal(out.Data.EchartOption, &opt); perr != nil {
						t.Fatalf("bad echartOption: %v\noption: %s", perr, out.Data.EchartOption)
					}
					if len(opt.Series) == 0 {
						t.Errorf("expected a %q chart, got table signal (option=%s) — chart degraded",
							c.chart, out.Data.EchartOption)
					} else if opt.Series[0].Type != c.chart {
						t.Errorf("series[0].type = %q, want %q", opt.Series[0].Type, c.chart)
					}
				}

			case "either":
				// Acceptable: prose answer, clarification, a 4xx rejection, or executed
				// SQL that avoids the forbidden terms.
				if out.Data.Answer != "" || out.Data.Clarification != "" {
					return
				}
				if resp.StatusCode != 200 {
					return // validator/runtime correctly rejected it
				}
				low := strings.ToLower(out.Data.SQL)
				for _, bad := range c.sqlNot {
					if strings.Contains(low, bad) {
						t.Fatalf("sql contains forbidden %q\nfull sql: %s", bad, out.Data.SQL)
					}
				}

			default:
				t.Fatalf("unknown expect class %q", c.expect)
			}
		})
	}

	writeSuiteTokenReport(t, "../../../../docs/ask-demo-test-results.md", "/ask/demo full-loop live results",
		[]string{"case", "expect", "result", "tokens", "time"}, askRows, askTok, time.Since(askStart))
}
