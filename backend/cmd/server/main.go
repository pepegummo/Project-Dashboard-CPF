package main

import (
	"context"
	"fmt"
	"iot-dashboard/internal/broadcaster"
	"iot-dashboard/internal/config"
	"iot-dashboard/internal/database"
	"iot-dashboard/internal/middleware"
	"iot-dashboard/internal/migrate"
	"iot-dashboard/internal/modules/admin"
	"iot-dashboard/internal/modules/ai"
	"iot-dashboard/internal/modules/alerts"
	"iot-dashboard/internal/modules/auth"
	"iot-dashboard/internal/modules/dashboards"
	"iot-dashboard/internal/modules/led"
	"iot-dashboard/internal/modules/machines"
	"iot-dashboard/internal/modules/telemetry"
	"iot-dashboard/internal/normalizer"
	ws "iot-dashboard/internal/websocket"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/helmet"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/gofiber/fiber/v2/middleware/logger"
	fiberws "github.com/gofiber/websocket/v2"
)

// alertEvalAdapter evaluates alert rules and broadcasts triggered events over WS.
type alertEvalAdapter struct {
	svc     *alerts.Service
	gateway *ws.Gateway
}

func (a *alertEvalAdapter) EvaluateAndBroadcast(ctx context.Context, machineID, machineName string, data map[string]interface{}) {
	triggered, err := a.svc.EvaluateTelemetry(ctx, machineID, data)
	if err != nil {
		return
	}
	for _, t := range triggered {
		a.gateway.BroadcastAlert(ws.AlertPayload{
			AlertID:     t.AlertID,
			AlertName:   t.AlertName,
			MachineID:   machineID,
			MachineName: machineName,
			Field:       t.Field,
			Value:       t.Value,
			Threshold:   t.Threshold,
			Condition:   t.Condition,
			Severity:    t.Severity,
			Message:     t.Message,
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
		})
	}
}

func main() {
	// ── Config ────────────────────────────────────────────────────────────────
	config.Load()
	fmt.Println("\n🏭 Industrial IoT AI Dashboard — Backend (Go/Fiber)")
	fmt.Printf("   Environment: %s\n", config.Env.NodeEnv)

	// ── Database ──────────────────────────────────────────────────────────────
	ctx := context.Background()
	if err := database.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Database connection failed: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()
	if err := migrate.RunAll(ctx, database.Pool); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Migration warning: %v\n", err)
		// non-fatal — server continues; DB may already be set up
	}

	// ── WebSocket gateway ─────────────────────────────────────────────────────
	gateway := ws.NewGateway()

	// ── Alert evaluator — shared by broadcaster (periodic) and ingest (on-demand) ──
	alertEval := &alertEvalAdapter{svc: alerts.NewService(), gateway: gateway}

	// ── DB Broadcaster — pushes latest DB telemetry to WS clients every 30s, evaluates alerts ──
	dbBroadcaster := broadcaster.New(gateway, 30*time.Second, alertEval)
	dbBroadcaster.Start()

	// ── Normalizer — landing tables → canonical series/readings, driven by the
	// source registry. Idle when no source_tables rows are registered.
	norm := normalizer.New(30 * time.Second)
	norm.Start()

	// ── Fiber App ─────────────────────────────────────────────────────────────
	app := fiber.New(fiber.Config{
		ErrorHandler: middleware.ErrorHandler,
		BodyLimit:    1 * 1024 * 1024, // 1MB
	})

	// Security & middleware
	app.Use(helmet.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins:     config.Env.CorsOrigin,
		AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
		AllowHeaders:     "Content-Type,Authorization",
		AllowCredentials: true,
	}))
	app.Use(compress.New())
	app.Use(logger.New(logger.Config{
		Format: "${time} ${method} ${path} ${status} ${latency}\n",
	}))

	// Rate limiting
	app.Use("/api/auth", limiter.New(limiter.Config{
		Max:        20,
		Expiration: 15 * time.Minute,
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(429).JSON(fiber.Map{
				"success": false,
				"error":   fiber.Map{"code": "TOO_MANY_REQUESTS", "message": "Too many requests"},
			})
		},
	}))
	app.Use(limiter.New(limiter.Config{
		Max:        100_000,
		Expiration: 1 * time.Minute,
	}))

	// Health check
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status":    "ok",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"version":   "2.0.0-go",
			"env":       config.Env.NodeEnv,
		})
	})

	// ── Routes ────────────────────────────────────────────────────────────────
	api := app.Group("/api")
	admin.RegisterRoutes(api.Group("/admin"))
	auth.RegisterRoutes(api.Group("/auth"))
	machines.RegisterRoutes(api.Group("/machines"))
	telemetry.RegisterRoutes(api.Group("/telemetry"), dbBroadcaster, alertEval)
	dashboards.RegisterRoutes(api.Group("/dashboards"))
	alerts.RegisterRoutes(api.Group("/alerts"))
	ai.RegisterRoutes(api.Group("/ai"))
	led.RegisterRoutes(api.Group("/led"))

	// WebSocket on same port as REST — auth via ?token query param
	app.Get("/ws",
		func(c *fiber.Ctx) error {
			if fiberws.IsWebSocketUpgrade(c) {
				return c.Next()
			}
			return fiber.ErrUpgradeRequired
		},
		middleware.AuthenticateWS,
		fiberws.New(gateway.HandleFiber),
	)

	// 404 fallback
	app.Use(func(c *fiber.Ctx) error {
		return c.Status(404).JSON(fiber.Map{
			"success": false,
			"error":   fiber.Map{"code": "NOT_FOUND", "message": fmt.Sprintf("Route %s %s not found", c.Method(), c.Path())},
		})
	})

	// ── Start ─────────────────────────────────────────────────────────────────
	go func() {
		addr := fmt.Sprintf(":%d", config.Env.Port)
		fmt.Printf("✅ REST API listening on http://localhost%s\n", addr)
		printRoutes()
		if err := app.Listen(addr); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Server error: %v\n", err)
		}
	}()

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	fmt.Println("\nShutting down gracefully…")
	dbBroadcaster.Stop()
	norm.Stop()
	_ = app.ShutdownWithTimeout(10 * time.Second)
	fmt.Println("👋 Shutdown complete")
}

func printRoutes() {
	fmt.Println("\n📋 API Endpoints:")
	fmt.Println("   POST /api/auth/login")
	fmt.Println("   GET  /api/machines")
	fmt.Println("   GET  /api/telemetry/:id/latest")
	fmt.Println("   GET  /api/dashboards")
	fmt.Println("   GET  /api/alerts")
	fmt.Println()
}
