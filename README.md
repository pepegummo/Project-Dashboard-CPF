# IotVision — Industrial IoT AI Dashboard Platform

A production-style MVP for real-time industrial monitoring, built with Vue 3, Go (Fiber v2), PostgreSQL/TimescaleDB, and WebSocket telemetry streaming.

---

## 📐 Architecture

```
Project-Dashboard/
├── backend/                    # Go (module iot-dashboard) + Fiber v2 + pgx/v5
│   ├── cmd/
│   │   ├── server/main.go      # Entry point — auto-runs migrations on start
│   │   ├── backfill/main.go    # Demo telemetry: seeds ~2.3M historical rows
│   │   └── normalize-backfill/ # Real plants: registry → canonical series/readings + rollups
│   └── internal/
│       ├── config/             # env.go — loads .env via godotenv
│       ├── database/           # pgxpool singleton
│       ├── migrate/            # embedded SQL migrations, run on startup (no CLI)
│       ├── broadcaster/        # polls DB every 30s → broadcasts telemetry over WS
│       ├── websocket/          # ws_gateway.go — gofiber/websocket hub at GET /ws
│       ├── middleware/         # JWT auth, error handler
│       └── modules/
│           ├── auth/           # login, JWT sign/verify, bcrypt
│           ├── machines/       # CRUD + dynamic field schema
│           ├── telemetry/      # ingest, latest, series, aggregate, daily-count
│           ├── alerts/         # rule CRUD + EvaluateTelemetry
│           ├── dashboards/     # dashboard + widget CRUD, layout PATCH
│           ├── ai/             # /ai/chat and /ai/ask pipelines (see docs/ai-pages.md)
│           └── led/            # LED kiosk token: generate/get/revoke
│
└── frontend/                   # Vue 3 + Vite + TypeScript
    └── src/
        ├── stores/             # Pinia: auth, machines, telemetry, dashboards, alerts
        ├── services/           # api.service.ts, ws.service.ts
        ├── composables/        # useTelemetry, useWebSocket
        ├── pages/              # Login, DashboardList, DashboardEditor, Machines, Alerts,
        │                       #   AI (/ai chat), Ask (/ask + /ask/:scope), LedView (/led)
        ├── components/
        │   ├── layout/         # Sidebar, TopBar
        │   ├── dashboard/      # GridStackCanvas, WidgetToolbox, WidgetConfigModal
        │   └── widgets/        # LineChart, Gauge, KpiCard, StatusCard, Table, AlarmPanel
        └── layouts/            # AppLayout (sidebar + topbar shell)
```

---

## 🚀 Quick Start (Docker — recommended)

```bash
# Copy env file
cp .env.example .env
# fill in JWT_SECRET (≥32 chars) and AI_API_KEY

# Start all services (db, redis, backend, frontend)
docker compose up -d

# Migrations + seed data run automatically on backend startup.
# Optionally load ~2.3M rows of historical DEMO telemetry:
docker compose exec backend ./backfill

# Real plant data instead? Register the source, then drain it into the
# canonical model and refresh rollups — see docs/onboarding-new-source.md:
docker compose exec backend ./normalize-backfill
```

**Access:**
- Frontend: http://localhost:5173
- Backend API: http://localhost:4000
- WebSocket: ws://localhost:4000/ws

**Default login:** `admin@acme-foods.com` / `Admin@1234`

---

## 💻 Local Development (without Docker)

### Prerequisites
- Go 1.x
- Node.js 20+
- PostgreSQL 16 + TimescaleDB extension
- Redis (optional — not strictly required for dev)

### Backend

```bash
cd backend

# Copy and edit .env
cp ../.env.example .env

# Start dev server — migrations + seed data run automatically on startup
go run ./cmd/server/
```

### Frontend

```bash
cd frontend
npm install

# Start dev server
npm run dev
```

Frontend dev server: http://localhost:5173 (proxies /api and /ws to :4000)

---

## 🗄️ Database Schema

| Table | Description |
|---|---|
| `organizations` | Multi-tenant root entity |
| `factories` | Physical factory locations |
| `production_lines` | Lines within a factory |
| `machines` | Individual machines with type and metadata |
| `machine_fields` | Dynamic telemetry field schema per machine |
| `telemetry_raw` | TimescaleDB hypertable — 1s granularity JSONB |
| `telemetry_aggregates` | Pre-computed 1m/5m/1h/1d rollups |
| `users` | Users with role-based access |
| `dashboards` | User-owned dashboard configurations |
| `dashboard_widgets` | Widget layout + JSON config |
| `alerts` | Alert rules with threshold conditions |
| `alert_events` | Alert firing history with lifecycle |
| `ai_conversations` | AI conversation sessions |
| `ai_messages` | Messages + tool call records |
| `audit_logs` | Full audit trail |

### TimescaleDB setup

Hypertable creation runs automatically on startup:

```sql
SELECT create_hypertable('telemetry_raw', 'timestamp', if_not_exists => TRUE);
SELECT add_compression_policy('telemetry_raw', INTERVAL '7 days', if_not_exists => TRUE);
```

---

## 🔌 WebSocket Protocol

Connect: `ws://localhost:4000/ws?token=<jwt>`

```jsonc
// Client → Server: subscribe to machine telemetry
{ "type": "subscribe", "payload": { "machineIds": ["uuid1", "uuid2"] }, "timestamp": 1719000000000 }

// Server → Client: telemetry broadcast (on ingest, plus a 30s fallback poll)
{ "type": "telemetry", "payload": { "machineId": "...", "machineName": "CW-01", "timestamp": "...", "data": { "weight": 501.3, "speed": 62 } }, "timestamp": ... }

// Server → Client: alert event
{ "type": "alert", "payload": { "alertId": "...", "severity": "warning", "field": "weight", "value": 511.2, ... }, "timestamp": ... }
```

---

## 🤖 AI Assistant

Two independent AI surfaces, both backed by an OpenAI-compatible chat completions API (`AI_BASE_URL`, `AI_API_KEY`; Groq/gpt-oss models are only the fallback defaults in code):

| Surface | Route | Purpose |
|---|---|---|
| Chat Assistant | `POST /api/ai/chat` | Conversational agent — intent router → tool loop → verify-then-repair; reads live telemetry and stages dashboard edits via structured tool calls. |
| Ask-Data | `POST /api/ai/ask` | Natural language → hardened read-only SQL → LLM-authored ECharts chart; results can be saved to boards. One URL shape per dataset: `/ask/:scope` (scopes listed by `GET /api/ai/scopes`). |

Production runs on KKU GenAI: generation model `claude-sonnet-5` (`AI_MODEL`), router/verifier model `gpt-5.4-mini` (`AI_ROUTER_MODEL`).

See [`docs/ai-pages.md`](docs/ai-pages.md) for the full end-to-end pipeline breakdown,
[`docs/onboarding-new-source.md`](docs/onboarding-new-source.md) for registering a new upstream
table/plant, and [`docs/ask-demo-test-results.md`](docs/ask-demo-test-results.md) for the latest
`/ask/demo` full-loop results.

---

## 🎛️ REST API Reference

```
POST   /api/auth/login
GET    /api/auth/me

GET    /api/machines
POST   /api/machines
GET    /api/machines/:id
PATCH  /api/machines/:id
DELETE /api/machines/:id
GET    /api/machines/:id/fields
PUT    /api/machines/:id/fields

GET    /api/telemetry/:machineId/latest
GET    /api/telemetry/:machineId/series?field=weight&timeRange=1h
POST   /api/telemetry/:machineId/ingest
GET    /api/telemetry/latest?ids=id1,id2

GET    /api/dashboards
POST   /api/dashboards
GET    /api/dashboards/:id
PATCH  /api/dashboards/:id
DELETE /api/dashboards/:id
POST   /api/dashboards/:id/widgets
PATCH  /api/dashboards/:id/layout
PATCH  /api/dashboards/:id/widgets/:widgetId
DELETE /api/dashboards/:id/widgets/:widgetId

GET    /api/alerts
POST   /api/alerts
PATCH  /api/alerts/:id
DELETE /api/alerts/:id
GET    /api/alerts/events/active
PATCH  /api/alerts/events/:eventId/acknowledge
PATCH  /api/alerts/events/:eventId/resolve

GET    /api/ai/tools
POST   /api/ai/tools/execute
GET    /api/ai/conversations
POST   /api/ai/conversations
GET    /api/ai/conversations/:id/messages
POST   /api/ai/conversations/:id/messages
GET    /api/ai/preview-draft
PUT    /api/ai/preview-draft
DELETE /api/ai/preview-draft
PUT    /api/ai/selected-dashboard
POST   /api/ai/chat
GET    /api/ai/scopes
POST   /api/ai/ask
POST   /api/ai/run-sql
GET    /api/ai/boards
POST   /api/ai/boards
GET    /api/ai/boards/:id
PATCH  /api/ai/boards/:id
DELETE /api/ai/boards/:id
POST   /api/ai/boards/:id/charts
DELETE /api/ai/boards/:id/charts/:chartId

POST   /api/led/token          # admin/editor only — generate per-org led-viewer JWT
GET    /api/led/token
DELETE /api/led/token
```

---

## 📡 Demo Machines

| Machine | Type | Key Fields |
|---|---|---|
| Checkweigher CW-01 | `checkweigher` | weight, speed, rejects, throughput |
| Temp Sensor TS-01 | `temperature_sensor` | temp, humidity, dew_point |
| Conveyor Belt CB-01 | `conveyor` | speed, load, rpm, vibration |
| Vision AI Camera VC-01 | `vision_camera` | defect_rate, inspected, passed, failed, confidence |

There is **no simulator**. Devices POST to `/api/telemetry/:id/ingest`; each row is alert-evaluated
and broadcast over WS immediately, and the broadcaster re-polls the DB every 30s as a fallback
heartbeat. The `backfill` tool seeds these four machines with historical 1s-granularity rows.

---

## 🧱 Widget Types

| Widget | Config Keys |
|---|---|
| `line-chart` | `field`, `timeRange`, `color` |
| `gauge` | `field`, `min`, `max`, `unit`, `thresholds` |
| `kpi-card` | `field`, `unit`, `precision` |
| `status-card` | _(machine-level)_ |
| `table` | _(all fields)_ |
| `alarm-panel` | `maxItems`, `severities` |
| `daily-count` | `field`, `bucket` (minute / hour / day) |
| `chart` | `fields[]`, `timeRange`, `chartType` (line / bar / area) — multi-field overlay |

---

## 🔐 Authentication & Roles

| Role | Permissions |
|---|---|
| `admin` | Full access including delete |
| `editor` | Create/update machines, dashboards, alerts |
| `viewer` | Read-only |

JWT tokens expire after 24h (configurable via `JWT_EXPIRES_IN`).

---

## 🏗️ Production Checklist

- [ ] Change `JWT_SECRET` to a 256-bit random value
- [ ] Set strong `POSTGRES_PASSWORD`
- [ ] Configure `CORS_ORIGIN` to your actual frontend domain
- [ ] Enable SSL/TLS termination at the load balancer
- [ ] Set up TimescaleDB retention policy for `telemetry_raw`
- [ ] Configure Redis for session storage (optional)
- [ ] Set up log aggregation (Fiber logger middleware)
- [ ] Configure monitoring / alerting for the backend service

---

## 📦 Tech Stack

| Layer | Technology |
|---|---|
| Frontend framework | Vue 3 + TypeScript |
| Build tool | Vite 5 |
| State management | Pinia |
| Router | Vue Router 4 |
| Dashboard grid | GridStack.js |
| Charts | Apache ECharts + vue-echarts |
| Styling | TailwindCSS 3 |
| Icons | lucide-vue-next |
| HTTP client | Axios |
| Backend runtime | Go 1.x |
| Web framework | Fiber v2 |
| Database driver | pgx v5 (no ORM — raw SQL) |
| Database | PostgreSQL 16 + TimescaleDB |
| Real-time | gofiber/websocket (WS on the same port as REST, `/ws`) |
| Auth | JWT (golang-jwt) + bcrypt |
| Cache / future use | Redis (provisioned, no Go client wired up yet) |
| Container | Docker + Docker Compose |
