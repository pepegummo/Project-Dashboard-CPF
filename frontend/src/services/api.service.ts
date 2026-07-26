import axios, { AxiosInstance, AxiosError } from 'axios';
import type { ApiResponse, Dashboard, DashboardWidget, Machine, MachineField, Alert, AlertEvent, AiConversation, AiMessage, AiChatIntent, AiTool, TelemetrySeries, TelemetrySnapshot, OrgOption, AskDataResult, AskBoardSummary, AskBoard, AskScope } from '@/types';

const BASE_URL = import.meta.env.VITE_API_URL ?? 'http://localhost:4000';

class ApiService {
  private client: AxiosInstance;
  private overrideToken: string | null = null;

  constructor() {
    this.client = axios.create({
      baseURL: `${BASE_URL}/api`,
      timeout: 15_000,
      headers: { 'Content-Type': 'application/json' },
    });

    // Request interceptor — attach token (LED override takes priority over localStorage)
    this.client.interceptors.request.use((config) => {
      const token = this.overrideToken ?? localStorage.getItem('auth_token');
      if (token) config.headers.Authorization = `Bearer ${token}`;
      return config;
    });

    // Response interceptor — unwrap or throw
    this.client.interceptors.response.use(
      (res) => res,
      (err: AxiosError<{ success: false; error: { code: string; message: string } }>) => {
        if (err.response?.status === 401) {
          localStorage.removeItem('auth_token');
          const path = window.location.pathname;
          // Don't redirect from public pages (/led kiosk) or from /login itself —
          // a failed login is a 401 and must show its error, not reload the page.
          if (!path.startsWith('/led') && !path.startsWith('/login')) {
            window.location.href = '/login';
          }
        }
        let message: string;
        if (!err.response) {
          // No HTTP response → offline, server down, or timeout.
          if (!navigator.onLine) message = 'You appear to be offline. Check your connection.';
          else if (err.code === 'ECONNABORTED') message = 'The server took too long to respond. Please try again.';
          else message = 'Cannot reach the server. Please try again.';
        } else {
          message = err.response.data?.error?.message
            ?? (err.response.status >= 500 ? 'Something went wrong on our side. Please try again.' : err.message);
        }
        return Promise.reject(new Error(message));
      },
    );
  }

  setToken(token: string | null) {
    if (token) {
      localStorage.setItem('auth_token', token);
    } else {
      localStorage.removeItem('auth_token');
    }
  }

  // Used by LED kiosk pages to authenticate REST + WS without a user session.
  setOverrideToken(token: string | null) {
    this.overrideToken = token;
  }

  // ─── Auth ──────────────────────────────────────────────────────────────────
  async login(email: string, password: string) {
    const { data } = await this.client.post<ApiResponse<{ token: string; user: any; organizations: OrgOption[] }>>('/auth/login', { email, password });
    return data.data;
  }

  async switchOrg(organizationId: string) {
    const { data } = await this.client.post<ApiResponse<{ token: string }>>('/auth/switch-org', { organizationId });
    return data.data;
  }

  async getProfile() {
    const { data } = await this.client.get<ApiResponse<any>>('/auth/me');
    return data.data;
  }

  // ─── Machines ─────────────────────────────────────────────────────────────
  async getMachines(filters?: Record<string, string>) {
    const { data } = await this.client.get<ApiResponse<Machine[]>>('/machines', { params: filters });
    return data.data;
  }

  async getMachine(id: string) {
    const { data } = await this.client.get<ApiResponse<Machine>>(`/machines/${id}`);
    return data.data;
  }

  async createMachine(payload: Partial<Machine> & { productionLineId: string }) {
    const { data } = await this.client.post<ApiResponse<Machine>>('/machines', payload);
    return data.data;
  }

  async updateMachine(id: string, payload: Partial<Machine>) {
    const { data } = await this.client.patch<ApiResponse<Machine>>(`/machines/${id}`, payload);
    return data.data;
  }

  async deleteMachine(id: string) {
    await this.client.delete(`/machines/${id}`);
  }

  async getMachineFields(machineId: string) {
    const { data } = await this.client.get<ApiResponse<MachineField[]>>(`/machines/${machineId}/fields`);
    return data.data;
  }

  async upsertMachineField(machineId: string, field: Partial<MachineField>) {
    const { data } = await this.client.put<ApiResponse<MachineField>>(`/machines/${machineId}/fields`, field);
    return data.data;
  }

  async deleteField(machineId: string, fieldKey: string) {
    await this.client.delete(`/machines/${machineId}/fields/${fieldKey}`);
  }

  async getProductionLines() {
    const { data } = await this.client.get<ApiResponse<any[]>>('/machines/production-lines');
    return data.data;
  }

  async getFactories() {
    const { data } = await this.client.get<ApiResponse<any[]>>('/machines/factories');
    return data.data;
  }

  // ─── Telemetry ────────────────────────────────────────────────────────────
  async getLatestTelemetry(machineId: string) {
    const { data } = await this.client.get<ApiResponse<TelemetrySnapshot>>(`/telemetry/${machineId}/latest`);
    return data.data;
  }

  async getTelemetrySeries(
    machineId: string,
    field: string,
    options: { timeRange?: string; startTime?: string; endTime?: string; bucket?: string; points?: number } = {},
  ) {
    const params: Record<string, string> = { field };
    if (options.bucket) {
      // Explicit bucket size + bar count → exactly `points` gap-filled buckets.
      params.bucket = options.bucket;
      if (options.points) params.points = String(options.points);
    } else if (options.startTime && options.endTime) {
      params.startTime = options.startTime;
      params.endTime   = options.endTime;
    } else {
      params.timeRange = options.timeRange ?? '1h';
    }
    const { data } = await this.client.get<ApiResponse<TelemetrySeries>>(`/telemetry/${machineId}/series`, { params });
    return data.data;
  }

  async getTelemetryAggregate(machineId: string, field: string, period: string) {
    const { data } = await this.client.get<ApiResponse<import('@/types').TelemetryAggregateResult>>(
      `/telemetry/${machineId}/aggregate`,
      { params: { field, period } },
    );
    return data.data;
  }

  async getTotalCount(machineId: string) {
    const { data } = await this.client.get<ApiResponse<{ total: number; since: string | null }>>(`/telemetry/${machineId}/total-count`);
    return data.data;
  }

  async getTelemetryDailyCount(machineId: string, days: number) {
    const { data } = await this.client.get<ApiResponse<{
      machineId: string;
      days: number;
      data: Array<{ date: string; count: number }>;
    }>>(`/telemetry/${machineId}/daily-count`, { params: { days } });
    return data.data;
  }

  async getHourlyCount(machineId: string, hours = 8) {
    const { data } = await this.client.get<ApiResponse<{
      machineId: string;
      hours: number;
      data: Array<{ bucket: string; count: number }>;
    }>>(`/telemetry/${machineId}/hourly-count`, { params: { hours } });
    return data.data;
  }

  // Count pieces per time bucket, optionally filtered by SKU and good/reject status. bucket = '<n><m|h|d>'.
  async getTelemetryCount(
    machineId: string,
    bucket: string,
    opts: { sku?: string; status?: 'all' | 'good' | 'reject'; points?: number } = {},
  ) {
    const params: Record<string, string | number> = { bucket, points: opts.points ?? 48 };
    if (opts.sku) params.sku = opts.sku;
    if (opts.status) params.status = opts.status;
    const { data } = await this.client.get<ApiResponse<{
      machineId: string;
      sku: string;
      status: string;
      bucket: string;
      data: Array<{ bucket: string; count: number }>;
    }>>(`/telemetry/${machineId}/count`, { params });
    return data.data;
  }

  // Distinct SKU values seen for a machine (last 30 days) — populates the Count widget's SKU dropdown.
  async getMachineSkus(machineId: string) {
    const { data } = await this.client.get<ApiResponse<string[]>>(`/telemetry/${machineId}/skus`);
    return data.data;
  }

  async getMultiLatestTelemetry(machineIds: string[]) {
    const { data } = await this.client.get<ApiResponse<Record<string, TelemetrySnapshot>>>('/telemetry/latest', {
      params: { ids: machineIds.join(',') },
    });
    return data.data;
  }

  // ─── Dashboards ───────────────────────────────────────────────────────────
  async getDashboards() {
    const { data } = await this.client.get<ApiResponse<Dashboard[]>>('/dashboards');
    return data.data;
  }

  async getDashboard(id: string) {
    const { data } = await this.client.get<ApiResponse<Dashboard>>(`/dashboards/${id}`);
    return data.data;
  }

  async createDashboard(payload: { name: string; description?: string; isPublic?: boolean; tags?: string[] }) {
    const { data } = await this.client.post<ApiResponse<Dashboard>>('/dashboards', payload);
    return data.data;
  }

  async updateDashboard(id: string, payload: Partial<Dashboard>) {
    const { data } = await this.client.patch<ApiResponse<Dashboard>>(`/dashboards/${id}`, payload);
    return data.data;
  }

  async deleteDashboard(id: string) {
    await this.client.delete(`/dashboards/${id}`);
  }

  async addWidget(dashboardId: string, widget: {
    machineId?: string;
    widgetType: string;
    title?: string;
    layout: { x: number; y: number; w: number; h: number };
    config: Record<string, unknown>;
  }) {
    const { data } = await this.client.post<ApiResponse<DashboardWidget>>(`/dashboards/${dashboardId}/widgets`, widget);
    return data.data;
  }

  async updateWidget(dashboardId: string, widgetId: string, payload: Partial<DashboardWidget>) {
    const { data } = await this.client.patch<ApiResponse<DashboardWidget>>(`/dashboards/${dashboardId}/widgets/${widgetId}`, payload);
    return data.data;
  }

  async bulkUpdateLayout(dashboardId: string, widgets: Array<{ id: string; layout: any }>) {
    await this.client.patch(`/dashboards/${dashboardId}/layout`, widgets);
  }

  async deleteWidget(dashboardId: string, widgetId: string) {
    await this.client.delete(`/dashboards/${dashboardId}/widgets/${widgetId}`);
  }

  // ─── Alerts ───────────────────────────────────────────────────────────────
  async getAlerts(machineId?: string) {
    const { data } = await this.client.get<ApiResponse<Alert[]>>('/alerts', { params: machineId ? { machineId } : {} });
    return data.data;
  }

  async getActiveAlertEvents() {
    const { data } = await this.client.get<ApiResponse<AlertEvent[]>>('/alerts/events/active');
    return data.data;
  }

  async createAlert(payload: Partial<Alert> & { machineId: string }) {
    const { data } = await this.client.post<ApiResponse<Alert>>('/alerts', payload);
    return data.data;
  }

  async updateAlert(id: string, payload: Partial<Alert>) {
    const { data } = await this.client.patch<ApiResponse<Alert>>(`/alerts/${id}`, payload);
    return data.data;
  }

  async deleteAlert(id: string) {
    await this.client.delete(`/alerts/${id}`);
  }

  async acknowledgeAlertEvent(eventId: string) {
    const { data } = await this.client.patch<ApiResponse<AlertEvent>>(`/alerts/events/${eventId}/acknowledge`);
    return data.data;
  }

  async resolveAlertEvent(eventId: string) {
    const { data } = await this.client.patch<ApiResponse<AlertEvent>>(`/alerts/events/${eventId}/resolve`);
    return data.data;
  }

  // ─── AI ───────────────────────────────────────────────────────────────────
  async getAiTools() {
    const { data } = await this.client.get<ApiResponse<AiTool[]>>('/ai/tools');
    return data.data;
  }

  async executeAiTool(toolName: string, params: Record<string, unknown> = {}) {
    const { data } = await this.client.post<ApiResponse<unknown>>('/ai/tools/execute', { toolName, params });
    return data.data;
  }

  // ─── Ask Data (NL → SQL → ECharts) ──────────────────────────────────────────
  // Which datasets this user can ask against: the demo telemetry org plus one entry
  // per factory with registered sources, so a new plant appears without a UI change.
  async askScopes() {
    const { data } = await this.client.get<ApiResponse<AskScope[]>>('/ai/scopes');
    return data.data;
  }

  async askData(
    question: string,
    context?: { question: string; sql: string; spec?: string; clarification?: string; windowHours?: number },
    dataset?: string,
    factoryId?: string,
  ) {
    // Up to three sequential LLM round-trips (SQL → prose/chart → judge) → far
    // more than the default 15s. Outermost rung of the timeout ladder: it must
    // stay at/above nginx's proxy_read_timeout (210s) and above the backend's own
    // askHandlerTimeout (200s, nl2sql.go) so the backend is what gives up first —
    // its 502 carries a real error.message, a proxy cut carries only HTML.
    // context = previous turn, so a follow-up ("make it a bar chart") refines it.
    const { data } = await this.client.post<ApiResponse<AskDataResult>>(
      '/ai/ask', { question, context, dataset, factoryId }, { timeout: 210_000 },
    );
    return data.data;
  }

  // Doubles as the zoom endpoint: from/to re-runs the same query over a narrower
  // range (and a finer bucket). Without them the window is windowHours back from now.
  // Pass spec+factoryId for a canonical chart — the server recompiles it, which is
  // what lets a zoom drop to a finer rollup tier; sql is the legacy replay path.
  async runSql(
    query: { sql?: string; spec?: string; factoryId?: string },
    range?: { from?: string; to?: string; windowHours?: number },
  ) {
    const { data } = await this.client.post<ApiResponse<{ columns: string[]; rows: unknown[][]; from: string; to: string }>>(
      '/ai/run-sql', { ...query, ...range },
    );
    return data.data;
  }

  async listBoards() {
    const { data } = await this.client.get<ApiResponse<AskBoardSummary[]>>('/ai/boards');
    return data.data;
  }

  async createBoard(name: string) {
    const { data } = await this.client.post<ApiResponse<{ id: string; name: string }>>('/ai/boards', { name });
    return data.data;
  }

  async getBoard(id: string) {
    const { data } = await this.client.get<ApiResponse<AskBoard>>(`/ai/boards/${id}`);
    return data.data;
  }

  async renameBoard(id: string, name: string) {
    const { data } = await this.client.patch<ApiResponse<{ id: string; name: string }>>(`/ai/boards/${id}`, { name });
    return data.data;
  }

  async deleteBoard(id: string) {
    await this.client.delete(`/ai/boards/${id}`);
  }

  async addBoardChart(boardId: string, payload: { question: string; sql: string; spec?: string; factoryId?: string; echartOption: Record<string, unknown>; windowHours?: number }) {
    const { data } = await this.client.post<ApiResponse<{ id: string }>>(`/ai/boards/${boardId}/charts`, payload);
    return data.data;
  }

  async deleteBoardChart(boardId: string, chartId: string) {
    await this.client.delete(`/ai/boards/${boardId}/charts/${chartId}`);
  }

  async getConversations() {
    const { data } = await this.client.get<ApiResponse<AiConversation[]>>('/ai/conversations');
    return data.data;
  }

  async createConversation(title?: string) {
    const { data } = await this.client.post<ApiResponse<AiConversation>>('/ai/conversations', { title });
    return data.data;
  }

  async getMessages(conversationId: string) {
    const { data } = await this.client.get<ApiResponse<AiMessage[]>>(`/ai/conversations/${conversationId}/messages`);
    return data.data;
  }

  async addMessage(conversationId: string, payload: { role: string; content: string; toolName?: string; toolInput?: object; toolResult?: object }) {
    const { data } = await this.client.post<ApiResponse<AiMessage>>(`/ai/conversations/${conversationId}/messages`, payload);
    return data.data;
  }

  async getPreviewDraft() {
    const { data } = await this.client.get<ApiResponse<{ conversationId: string; dashboardId?: string; data: any } | null>>(`/ai/preview-draft?_t=${Date.now()}`);
    return data.data;
  }

  async putPreviewDraft(payload: { conversationId: string | null; data: unknown }) {
    await this.client.put('/ai/preview-draft', payload);
  }

  async deletePreviewDraft() {
    await this.client.delete('/ai/preview-draft');
  }

  async putSelectedDashboard(dashboardId: string) {
    await this.client.put('/ai/selected-dashboard', { dashboardId });
  }

  async chat(conversationId: string, message: string, context?: string): Promise<{ messages: AiMessage[]; intent?: AiChatIntent }> {
    const { data } = await this.client.post<ApiResponse<AiMessage[]> & { intent?: AiChatIntent }>('/ai/chat', { conversationId, message, ...(context ? { context } : {}) }, { timeout: 120_000 });
    return { messages: data.data, intent: data.intent };
  }

  // ─── LED Token ────────────────────────────────────────────────────────────
  async getLedToken(): Promise<{ token: string | null }> {
    const { data } = await this.client.get<ApiResponse<{ token: string | null }>>('/led/token');
    return data.data!;
  }

  async generateLedToken(): Promise<{ token: string }> {
    const { data } = await this.client.post<ApiResponse<{ token: string }>>('/led/token');
    return data.data!;
  }

  async revokeLedToken(): Promise<void> {
    await this.client.delete('/led/token');
  }
}

export const api = new ApiService();
