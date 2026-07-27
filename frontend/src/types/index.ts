// ─── Auth ─────────────────────────────────────────────────────────────────────
export interface User {
  id: string;
  email: string;
  name: string;
  role: 'admin' | 'editor' | 'viewer';
  organizationId: string;
  avatarUrl?: string;
  preferences?: Record<string, unknown>;
}

export interface OrgOption {
  id: string;
  name: string;
  isMember: boolean;
}

export interface LoginCredentials {
  email: string;
  password: string;
}

// ─── Organization ────────────────────────────────────────────────────────────
export interface Organization {
  id: string;
  name: string;
  slug: string;
  plan: string;
}

// ─── Machine ─────────────────────────────────────────────────────────────────
export type MachineType = 'checkweigher' | 'temperature_sensor' | 'conveyor' | 'vision_camera';
export type MachineStatus = 'online' | 'offline' | 'maintenance' | 'error';

export interface MachineField {
  id: string;
  machineId: string;
  key: string;
  label: string;
  unit?: string;
  dataType: 'number' | 'boolean' | 'string' | 'enum';
  min?: number;
  max?: number;
  threshold?: number;   // target / nominal value
  upperLimit?: number;  // threshold + 10%  — data should stay below
  lowerLimit?: number;  // threshold - 10%  — data should stay above
  precision: number;
  isKey: boolean;
}

export interface Machine {
  id: string;
  productionLineId: string;
  name: string;
  type: MachineType;
  serialNumber?: string;
  model?: string;
  manufacturer?: string;
  status: MachineStatus;
  lastSeenAt?: string;
  metadata: Record<string, unknown>;
  fields: MachineField[];
  productionLine?: ProductionLine;
  _count?: { alerts: number };
}

export interface ProductionLine {
  id: string;
  factoryId: string;
  name: string;
  code?: string;
  status: string;
  factory?: Factory;
  _count?: { machines: number };
}

export interface Factory {
  id: string;
  organizationId: string;
  name: string;
  location?: string;
  timezone: string;
}

// ─── Telemetry ───────────────────────────────────────────────────────────────
export type TelemetryValue = number | string | boolean;

export interface TelemetrySnapshot {
  timestamp: string;
  data: Record<string, TelemetryValue>;
}

export interface TelemetryPoint {
  ts: string;
  value: number;
  avg?: number;
  min?: number;
  max?: number;
}

export interface TelemetrySeries {
  machineId: string;
  field: string;
  timeRange: string;
  from: string;
  to: string;
  data: TelemetryPoint[];
}

// ─── Dashboard ───────────────────────────────────────────────────────────────
export type WidgetType = 'line-chart' | 'gauge' | 'kpi-card' | 'status-card' | 'table' | 'alarm-panel' | 'daily-count' | 'chart';

export interface WidgetLayout {
  x: number;
  y: number;
  w: number;
  h: number;
}

export type AggregationPeriod = 'live' | '5m' | '15m' | '30m' | '1h' | '6h' | '24h' | '7d' | '15d' | '30d' | '3mo' | '6mo' | '1y';

export interface TelemetryAggregateSummary {
  avg: number;
  min: number;
  max: number;
  count: number;
}

export interface TelemetryAggregateResult {
  machineId: string;
  field: string;
  period: string;
  from: string;
  to: string;
  summary: TelemetryAggregateSummary | null;
}

export interface WidgetConfig {
  field?: string;
  timeRange?: string;
  bucket?: string;                        // count widget: bucket size, e.g. '30m' | '1h' | '1d'
  sku?: string;                           // count widget: filter to one SKU ('' = all SKUs)
  status?: 'all' | 'good' | 'reject';     // count widget: piece status filter
  aggregationPeriod?: AggregationPeriod;  // 'live' = real-time; anything else = periodic avg
  color?: string;
  fields?: string[];                      // custom chart: multiple field keys to overlay
  chartType?: 'line' | 'bar' | 'area';    // custom chart: render style
  colors?: string[];                      // custom chart: per-series colors
  points?: number;                        // custom chart: number of buckets/bars (window = bucket × points)
  scaling?: 'shared' | 'dual' | 'normalized'; // custom chart: y-axis scaling mode
  normalize?: boolean;                    // custom chart: deprecated — superseded by scaling='normalized'
  min?: number;
  max?: number;
  unit?: string;
  format?: string;
  precision?: number;
  thresholds?: Array<{ value: number; color: string }>;
  maxItems?: number;
  severities?: string[];
  startDateTime?: string;
  endDateTime?: string;
  [key: string]: unknown;
}

export interface DashboardWidget {
  id: string;
  dashboardId: string;
  machineId?: string;
  widgetType: WidgetType;
  title?: string;
  layout: WidgetLayout;
  config: WidgetConfig;
  machine?: Pick<Machine, 'id' | 'name' | 'type' | 'fields'>;
  order: number;
}

export interface Dashboard {
  id: string;
  organizationId: string;
  userId: string;
  name: string;
  description?: string;
  isPublic: boolean;
  isDefault: boolean;
  tags: string[];
  widgets?: DashboardWidget[];
  user?: { id: string; name: string };
  _count?: { widgets: number };
  widgetLayouts?: Array<{ type: WidgetType; x: number; y: number; w: number; h: number }>;
  createdAt: string;
  updatedAt: string;
}

// ─── Alert ───────────────────────────────────────────────────────────────────
export type AlertSeverity = 'info' | 'warning' | 'critical';
export type AlertCondition = 'gt' | 'lt' | 'eq' | 'gte' | 'lte' | 'neq' | 'between' | 'outside';

export interface Alert {
  id: string;
  machineId: string;
  name: string;
  description?: string;
  field: string;
  condition: AlertCondition;
  threshold: number;
  thresholdHi?: number;
  severity: AlertSeverity;
  isActive: boolean;
  cooldownSec: number;
  machine?: Pick<Machine, 'id' | 'name' | 'type'>;
  _count?: { events: number };
  createdAt: string;
}

export interface AlertEvent {
  id: string;
  alertId: string;
  value: number;
  message?: string;
  status: 'open' | 'acknowledged' | 'resolved';
  resolvedAt?: string;
  createdAt: string;
  alert: Alert & { machine: Pick<Machine, 'id' | 'name' | 'type'> };
}

// ─── WebSocket ───────────────────────────────────────────────────────────────
export interface WsMessage<T = unknown> {
  type: 'telemetry' | 'alert' | 'machine_status' | 'pong' | 'error' | 'subscribe' | 'unsubscribe' | 'ping';
  payload: T;
  timestamp: number;
}

export interface WsTelemetryPayload {
  machineId: string;
  machineName: string;
  timestamp: string;
  data: Record<string, TelemetryValue>;
}

export interface WsAlertPayload {
  alertId: string;
  alertName: string;
  machineId: string;
  machineName: string;
  field: string;
  value: number;
  threshold: number;
  condition: string;
  severity: AlertSeverity;
  message: string;
  timestamp: string;
}

// ─── AI ──────────────────────────────────────────────────────────────────────
export interface AiTool {
  name: string;
  description: string;
  parameters: Record<string, unknown>;
}

export interface AiConversation {
  id: string;
  userId: string;
  title: string;
  createdAt: string;
  updatedAt: string;
  _count?: { messages: number };
}

export interface AiMessage {
  id: string;
  conversationId: string;
  role: 'user' | 'assistant' | 'tool';
  content: string;
  toolName?: string;
  toolInput?: Record<string, unknown>;
  toolResult?: unknown;
  createdAt: string;
}

// Resolved intent + slots from the backend's classify_intent router (Chat()'s
// "intent" response field). ok:false with zero-value slots means the router
// declined/fell back — treat as "no ephemeral card", not as a text-parsing signal.
export interface AiChatIntent {
  ok: boolean;
  intent: string;
  machine: string;
  metric: string;
  fields: string[];
  bucket: string;
  dateRange: { start: string; end: string };
  targetWidget: string;
  status: string;
  sku: string;
  confidence: number;
}

// ─── Ask Data (NL → SQL → ECharts) ───────────────────────────────────────────
export interface AskScope {
  dataset: string; // 'telemetry' (demo/mock) | 'canonical' (a real factory)
  label: string;
  factoryId: string;
  slug?: string; // URL name — /ask/<slug>; empty falls back to /ask/<factoryId>
  sources?: number;
}

export interface AskDataResult {
  sql: string;
  // Canonical scopes return the query spec that produced the chart. Zoom and saved
  // boards replay the spec (recompiled server-side), not the SQL text, so a chart
  // keeps working when the relations underneath it change.
  spec?: string;
  columns: string[];
  rows: unknown[][];
  echartOption: Record<string, unknown>;
  caption?: string; // one-line prose the chart author wrote alongside the option
  analysis?: string; // short auto-analysis of what the chart shows (folded into the chart call)
  nextQuestion?: string; // one suggested follow-up the user might ask next
  answer?: string; // set instead of the fields above when the question is answered in prose
  clarification?: string; // set instead of the fields above when the question is under-specified
  // Window the SQL's $1/$2 were bound to. Zooming re-runs the same SQL over a
  // narrower from/to, which also buys a finer bucket (backend autoBucket).
  windowHours?: number;
  from?: string;
  to?: string;
}

export interface AskBoardSummary {
  id: string;
  name: string;
  updatedAt: string;
  chartCount: number;
}

export interface AskBoardChart {
  id: string;
  question: string;
  sql: string;
  // Canonical charts replay by recompiling this spec for the given factory; charts
  // saved from the legacy views have only sql and replay that instead.
  spec?: string;
  factoryId?: string;
  echartOption: Record<string, unknown>;
  windowHours?: number; // 0 for charts saved before windows became parameters
}

export interface AskBoard {
  id: string;
  name: string;
  charts: AskBoardChart[];
}

// ─── API ─────────────────────────────────────────────────────────────────────
export interface ApiResponse<T> {
  success: true;
  data: T;
  meta?: {
    total: number;
    page: number;
    limit: number;
    totalPages: number;
  };
}
