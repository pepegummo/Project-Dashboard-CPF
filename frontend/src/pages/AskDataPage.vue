<script setup lang="ts">
import { ref, computed, onMounted, reactive, nextTick, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { api } from '@/services/api.service';
import type { AskDataResult, AskBoardSummary, AskBoard, AskBoardChart, AskScope } from '@/types';
import { Sparkles, Loader2, Save, Trash2, RefreshCw, User, Plus, Pencil, ZoomOut } from 'lucide-vue-next';
import { marked } from 'marked';
import DOMPurify from 'dompurify';

// Which data this page asks against. 'canonical' + a factoryId is one real plant
// (registry-driven, server-compiled queries); 'telemetry' is the demo org data.
// The URL carries it: /ask/demo, or /ask/<factories.slug> for a plant. The picker
// below lists whatever the backend offers, so another plant needs no change here.
const route = useRoute();
const router = useRouter();

// 'demo' | <slug> | <factoryId> — a scope's URL name, and its inverse.
const scopeKey = (s: AskScope) => (s.dataset !== 'canonical' ? 'demo' : s.slug || s.factoryId);
const findScope = (key: string) =>
  scopes.value.find((s) =>
    key === 'demo' ? s.dataset !== 'canonical' : s.slug === key || s.factoryId === key);

// 'demo' resolves synchronously; a plant's slug does not — only /ai/scopes can turn
// it into a factoryId, so the scope starts unresolved (factoryId '') and `ready`
// below keeps the page from asking against nothing in the meantime.
const paramKey = computed(() => String(route.params.scope ?? 'demo'));
const scopes = ref<AskScope[]>([]);
const scope = ref<AskScope>(
  paramKey.value === 'demo'
    ? { dataset: 'telemetry', factoryId: '', label: 'Demo telemetry' }
    : { dataset: 'canonical', factoryId: '', label: 'Factory' },
);
const ready = computed(() => scope.value.dataset !== 'canonical' || !!scope.value.factoryId);

const heading = computed(() =>
  scope.value.dataset === 'canonical' ? `Ask ${scope.value.label}` : 'Ask your data',
);
const askPlaceholder = computed(() =>
  scope.value.dataset === 'canonical'
    ? 'e.g. IQF2 ผลิตได้เท่าไหร่ 7 วันย้อนหลัง / อุณหภูมิ IQF2 vs ยอดผลิตเดือนที่แล้ว'
    : 'e.g. average speed per machine over the last 24 hours, hourly',
);

// This is what turns the URL param into a real scope: /ask/nj5 → the factoryId and
// label behind slug 'nj5'. A slug that no longer has registered sources falls back to
// the first scope so the page is never stuck pointing at nothing.
async function loadScopes() {
  try {
    scopes.value = await api.askScopes();
  } catch {
    return; // keep the unresolved scope — the picker just stays hidden
  }
  const match = findScope(paramKey.value);
  if (match) scope.value = match;
  // Falling back goes through switchScope so the URL follows too — /nj5 must not stay
  // in the address bar while the page is showing something else.
  else if (scopes.value.length > 0) switchScope(scopes.value[0]);
}

// Switching plants invalidates everything on screen: different machines, different
// metrics, and a spec compiled for another factory.
//
// It also changes the URL, because the scope IS the page: leaving /ask/nj5 in the address
// bar while showing demo data highlights the wrong sidebar entry and bookmarks a lie.
function switchScope(next: AskScope) {
  scope.value = next;
  result.value = null;
  notes.value = [];
  prev.value = null;
  zoomStack.value = [];
  askError.value = '';

  const key = scopeKey(next);
  // Compare against the live route: switching A → B → A must still put A back in the
  // address bar, and this is a no-op when the param change is what triggered us.
  if (key !== route.params.scope) {
    router.replace(`/ask/${key}`); // replace, not push: toggling scope is not navigation history
  }
}

// The sidebar's /ask/demo and /ask/nj5 hit the SAME route record, so vue-router reuses
// this component instead of re-creating it; a param change is the only signal that the
// plant changed. Without this the URL and the nav highlight move but the data does not.
watch(paramKey, (key) => {
  if (key === scopeKey(scope.value)) return; // switchScope already moved us — don't loop
  const match = findScope(key);
  if (match) switchScope(match);
  else if (scopes.value.length > 0) switchScope(scopes.value[0]);
});

// What to send to /ai/run-sql to reproduce a chart: a canonical chart is defined by
// its spec (recompiled server-side, so it also picks a finer rollup when zoomed),
// a legacy chart by its stored SQL text.
function replayOf(x: { sql: string; spec?: string; factoryId?: string }) {
  return x.spec ? { spec: x.spec, factoryId: x.factoryId || scope.value.factoryId } : { sql: x.sql };
}

// Answers arrive as markdown (tables, headers, bold). Sanitize before v-html —
// this is LLM output crossing into the DOM.
function renderMd(text: string) {
  return DOMPurify.sanitize(marked.parse(text, { async: false }));
}

// ── Ask state ────────────────────────────────────────────────────────────────
const question = ref('');
const asking = ref(false);
const askError = ref('');
const result = ref<AskDataResult | null>(null);
// Prose/clarification follow-ups about the current chart — shown as a Q&A thread
// inside the result card instead of clearing the chart. Reset on a new data turn.
const notes = ref<{ q: string; text: string; kind: 'answer' | 'clarification' }[]>([]);
// Snapshot of the question the current result answered (textarea keeps changing).
const askedQuestion = ref('');
// Previous turn, so a follow-up ("make it a bar chart") refines it instead of being rejected.
// clarification set instead of sql when the previous turn asked back (B3) — the next
// message is the user's reply to that question.
const prev = ref<{ question: string; sql: string; spec?: string; clarification?: string; windowHours?: number } | null>(null);

// A bucket with no readings is a MISSING row, not a zero — so ECharts draws one
// straight segment across it and it reads as steady running. Insert a null point in
// each hole (upstream sent nothing for ~3 hours on 2026-07-15) so the line breaks
// there instead of inventing a trend. The step is taken from the data itself rather
// than the bucket string, so it works on every path.
// ponytail: one null per hole is enough to break a line — no need to fill the whole
// hole; cap keeps a pathological gap from generating thousands of points.
function padTimeGaps(cols: string[], rows: unknown[][], xCol: unknown): unknown[][] {
  const xi = typeof xCol === 'string' ? cols.indexOf(xCol) : 0;
  if (xi < 0 || rows.length < 4) return rows;
  const times = rows.map((r) => Date.parse(String(r[xi])));
  if (times.some((t) => !Number.isFinite(t))) return rows;

  const deltas = times.slice(1).map((t, i) => t - times[i]).filter((d) => d > 0).sort((a, b) => a - b);
  if (deltas.length === 0) return rows;
  const step = deltas[Math.floor(deltas.length / 2)]; // median: robust to the holes themselves
  const out: unknown[][] = [];
  let added = 0;
  rows.forEach((r, i) => {
    out.push(r);
    if (i + 1 < rows.length && times[i + 1] - times[i] > step * 1.75 && added < 500) {
      const hole = cols.map((_, c) => (c === xi ? new Date(times[i] + step).toISOString() : null));
      out.push(hole);
      added++;
    }
  });
  return out;
}

// Merge the LLM's ECharts option (no data) with the result rows as an ECharts
// dataset. Re-running the SQL just swaps the source — the encoding stays put.
// Long-format results (a text category column beside the encoded x/y, e.g.
// bucket/machine_name/avg_speed) would zigzag as the LLM's single series — split
// into one filter-transform series per category value so 1 line = 1 machine.
function withDataset(option: Record<string, unknown>, columns: string[], rows: unknown[][]) {
  // rows can be null (backend serializes an empty result set as null) — coerce so the
  // array spread never throws "not iterable".
  const safeCols = Array.isArray(columns) ? columns : [];
  const seriesRaw = option?.series;
  const seriesArr = Array.isArray(seriesRaw) ? seriesRaw : seriesRaw ? [seriesRaw] : [];
  const firstEncode = ((seriesArr[0] as Record<string, unknown>)?.encode ?? {}) as Record<string, unknown>;
  const safeRows = padTimeGaps(safeCols, Array.isArray(rows) ? rows : [], firstEncode.x);
  // Long series get a zoom slider + wheel/drag zoom. Client-side only: it re-frames
  // the rows already loaded, it does not fetch finer buckets.
  // ponytail: 60-row threshold, no refetch-on-zoom — add drill-down when a day/point
  // is genuinely too coarse for what people zoom into.
  const zoom = safeRows.length > 60;
  // Layout is ours, content is the model's: it writes the title text and the legend
  // labels, we decide where they sit. Left to itself the model puts both on row 0,
  // so a title like "IQF2: Evap Temp vs ยอดผลิต 14-19 ก.ค. (ค่าเฉลี่ยทุก 6 ชม.)"
  // runs straight under the legend. One row each, then the plot below both.
  const merged = {
    ...(option ?? {}),
    title: { ...(option?.title as object), top: 4, left: 8 },
    legend: { ...(option?.legend as object), top: 32, left: 'center', orient: 'horizontal' },
    grid: { containLabel: true, ...(option?.grid as object), top: 78, ...(zoom ? { bottom: 64 } : {}) },
    ...(zoom ? { dataZoom: [{ type: 'inside' }, { type: 'slider', bottom: 8, height: 18 }] } : {}),
    dataset: { source: [safeCols, ...safeRows] },
  };

  // Min–max envelope: the backend ships <field>_min/<field>_max beside a gauge mean
  // (compiler.go), because the mean of an hour that ran at −35 and defrosted to +25
  // is +1.6 — a value the machine was never at. Shade min..max behind the line and
  // both states are visible without zooming to the raw tier.
  const banded = seriesArr.flatMap((raw) => {
    const s = raw as Record<string, unknown>;
    const y = (s.encode as Record<string, unknown> | undefined)?.y;
    if (s.type !== 'line' || typeof y !== 'string') return [s];
    const lo = `${y}_min`, hi = `${y}_max`;
    if (!safeCols.includes(lo) || !safeCols.includes(hi)) return [s];
    // stackStrategy 'all' is load-bearing: by default ECharts stacks same-sign values
    // only, so with a min of −36 the span would stack from zero and the band floated
    // above the axis instead of hugging the line (and stretched the axis to +80).
    const base = { type: 'line', yAxisIndex: s.yAxisIndex ?? 0, symbol: 'none', silent: true,
      stack: `band_${y}`, stackStrategy: 'all', lineStyle: { width: 0 }, tooltip: { show: false }, z: 1 };
    return [
      // Two stacked series are how ECharts fills between curves: an invisible one up
      // to min, then the span on top of it carrying the area.
      { ...base, name: `${y} band`, encode: { x: (s.encode as Record<string, unknown>).x, y: lo } },
      { ...base, name: `${y} span`, encode: { x: (s.encode as Record<string, unknown>).x, y: `__span_${y}` },
        areaStyle: { opacity: 0.16 } },
      { ...s, z: 3 },
    ];
  });
  if (banded.length !== seriesArr.length) {
    // The span dimension has to be computed — ECharts can stack, not subtract.
    const spans = safeCols.flatMap((c) => (c.endsWith('_max') && safeCols.includes(c.replace(/_max$/, '_min'))
      ? [c.replace(/_max$/, '')] : []));
    const cols = [...safeCols, ...spans.map((f) => `__span_${f}`)];
    const rows = safeRows.map((r) => [...r, ...spans.map((f) => {
      const lo = Number(r[safeCols.indexOf(`${f}_min`)]), hi = Number(r[safeCols.indexOf(`${f}_max`)]);
      return Number.isFinite(lo) && Number.isFinite(hi) ? hi - lo : null;
    })]);
    // ECharts names an unnamed series after its encoded dimension, so the band
    // helpers would show up in the legend as "evap_temp_min"/"__span_…". List only
    // the model's own series.
    const names = seriesArr
      .map((raw) => {
        const s = raw as Record<string, unknown>;
        const y = (s.encode as Record<string, unknown> | undefined)?.y;
        return typeof s.name === 'string' ? s.name : typeof y === 'string' ? y : '';
      })
      .filter(Boolean);
    return {
      ...merged,
      legend: { ...(merged.legend as object), ...(names.length ? { data: names } : {}) },
      dataset: { source: [cols, ...rows] },
      series: banded,
    };
  }

  if (seriesArr.length !== 1) return merged;
  const s = seriesArr[0] as Record<string, unknown>;

  // Heatmap: the per-category split below is for x/y line series — instead fill
  // visualMap's min/max from the real value column so the color scale spans the
  // data (the model only sees a 20-row sample and would clip on outliers).
  // ponytail: single value column assumed; a non-string encode.value or no numeric
  // data keeps whatever visualMap the model authored (return merged).
  if (s.type === 'heatmap') {
    const enc = (s.encode ?? {}) as Record<string, unknown>;
    const vi = typeof enc.value === 'string' ? safeCols.indexOf(enc.value) : -1;
    if (vi < 0) return merged;
    const nums = safeRows.map((r) => Number(r[vi])).filter((n) => Number.isFinite(n));
    if (nums.length === 0) return merged;
    const vm = (option?.visualMap ?? {}) as Record<string, unknown>;
    return {
      ...merged,
      dataZoom: undefined, // a heatmap's axes are categories — the slider would clash with visualMap
      grid: { ...merged.grid, bottom: 60 }, // room for the horizontal visualMap
      visualMap: {
        calculable: true, orient: 'horizontal', left: 'center', bottom: 8,
        ...vm, min: Math.min(...nums), max: Math.max(...nums),
      },
    };
  }

  if (!['line', 'bar', 'scatter'].includes(s.type as string)) return merged;
  const enc = (s.encode ?? {}) as Record<string, unknown>;
  if (typeof enc.x !== 'string' || typeof enc.y !== 'string') return merged;

  // Category column = first column outside encode.x/y whose values are strings.
  // (encode.seriesName may also point at it — that names one series, it doesn't split.)
  const catIdx = safeCols.findIndex(
    (c, i) => c !== enc.x && c !== enc.y && typeof safeRows.find((r) => r[i] != null)?.[i] === 'string',
  );
  if (catIdx < 0) return merged;
  const vals = [...new Set(safeRows.map((r) => r[catIdx]).filter((v): v is string => typeof v === 'string'))];
  // ponytail: 20-category ceiling — beyond that the single-series fallback stands.
  if (vals.length < 2 || vals.length > 20) return merged;

  // Per-machine legend is a vertical list down the right side; grid reserves room for
  // it. It still starts below the title row so a long title cannot run into it.
  return {
    ...merged,
    legend: { ...(option.legend as object), left: undefined, top: 32, right: 8, orient: 'vertical' },
    grid: { ...merged.grid, right: 220 },
    dataset: [
      { source: [safeCols, ...safeRows] },
      ...vals.map((v) => ({ transform: { type: 'filter', config: { dimension: safeCols[catIdx], value: v } } })),
    ],
    series: vals.map((v, i) => ({ ...s, encode: { ...enc, seriesName: undefined }, name: v, datasetIndex: i + 1 })),
  };
}

// An empty option ({}) is the backend's "render as a table" signal (text-only result).
function isTabular(option: Record<string, unknown> | null | undefined) {
  return !option || Object.keys(option).length === 0;
}

const resultOption = computed(() =>
  result.value ? withDataset(result.value.echartOption, result.value.columns, result.value.rows) : null,
);
const resultIsTable = computed(() => !!result.value && isTabular(result.value.echartOption));
const resultIsEmpty = computed(() => !!result.value && (result.value.rows?.length ?? 0) === 0);

// ── Zoom → drill down ────────────────────────────────────────────────────────
// The dataZoom slider only re-frames rows already loaded. Once the user settles on
// a narrower range we re-run the SAME SQL bound to it, so the backend picks a finer
// bucket for it (a year of daily points becomes a week of hourly ones). Only a
// windowed query can drill — a listing has no $1 to re-bind.
type ZoomEvent = { start?: number; end?: number; startValue?: number; endValue?: number; batch?: ZoomEvent[] };

// zoomStack: breadcrumb of ranges we've drilled through. Last entry = what's on
// screen now; [0] = the originally-asked window. The slider can only pick a
// sub-range of loaded rows, so zoom-OUT can't be a gesture — it pops this stack.
const zoomStack = ref<{ from?: string; to?: string; windowHours?: number }[]>([]);
const drilled = computed(() => zoomStack.value.length > 1);
const drilling = ref(false);
let drillTimer: number | undefined;

// Index of the column the chart puts on the x axis, via the LLM's encode.
function xIndex(r: AskDataResult) {
  const raw = r.echartOption?.series;
  const s = (Array.isArray(raw) ? raw[0] : raw) as Record<string, unknown> | undefined;
  const enc = (s?.encode ?? {}) as Record<string, unknown>;
  return typeof enc.x === 'string' ? r.columns.indexOf(enc.x) : -1;
}

function onZoom(e: ZoomEvent) {
  const r = result.value;
  if (drilling.value || !(r?.spec || r?.sql?.includes('$1'))) return;
  const rows = r.rows ?? [];
  const xi = xIndex(r);
  if (xi < 0 || rows.length < 4) return;

  const b = e.batch?.[0] ?? e;
  const last = rows.length - 1;
  const i0 = Math.max(0, Math.floor(b.startValue ?? ((b.start ?? 0) / 100) * last));
  const i1 = Math.min(last, Math.ceil(b.endValue ?? ((b.end ?? 100) / 100) * last));
  // Whole range = not a zoom; a handful of points left = nothing finer to show.
  if ((i0 === 0 && i1 === last) || i1 - i0 < 2) return;

  const from = String(rows[i0][xi]);
  const to = String(rows[i1][xi]);
  if (Number.isNaN(Date.parse(from)) || Number.isNaN(Date.parse(to))) return; // x isn't time

  // Debounced — dragging a slider handle fires this continuously.
  clearTimeout(drillTimer);
  drillTimer = window.setTimeout(() => void drillTo({ from, to }), 400);
}

// Re-runs the current SQL over a range and swaps the rows in place — the LLM's
// option, and so the whole chart's look, is untouched.
async function fetchRange(range: { from?: string; to?: string; windowHours?: number }) {
  const r = result.value;
  if (!r) return;
  drilling.value = true;
  try {
    const d = await api.runSql(replayOf(r), range);
    result.value = { ...r, columns: d.columns, rows: d.rows, from: d.from, to: d.to };
  } catch (e) {
    askError.value = (e as Error).message;
  } finally {
    drilling.value = false;
  }
}

// Drill one level finer: push the range so zoom-out can walk back to it.
function drillTo(range: { from: string; to: string }) {
  zoomStack.value.push(range);
  void fetchRange(range);
}

// Zoom out one step — drop the current range, re-render the one beneath it.
function zoomOut() {
  if (zoomStack.value.length < 2) return;
  zoomStack.value.pop();
  void fetchRange(zoomStack.value[zoomStack.value.length - 1]);
}

// Jump straight back to the originally-asked window.
function resetZoom() {
  if (zoomStack.value.length < 2) return;
  zoomStack.value = [zoomStack.value[0]];
  void fetchRange(zoomStack.value[0]);
}

async function ask() {
  const q = question.value.trim();
  if (!q || asking.value || !ready.value) return;
  asking.value = true;
  askError.value = '';
  try {
    const res = await api.askData(q, prev.value ?? undefined, scope.value.dataset, scope.value.factoryId);
    // A data turn replaces the chart and resets the thread; prose/clarification
    // turns annotate the current chart instead of clearing it. Only a data turn
    // advances the SQL context.
    if (res.sql) {
      result.value = res;
      askedQuestion.value = q;
      notes.value = [];
      prev.value = { question: q, sql: res.sql, spec: res.spec, windowHours: res.windowHours };
      zoomStack.value = [{ windowHours: res.windowHours }];
    } else if (res.clarification) {
      notes.value.push({ q, text: res.clarification, kind: 'clarification' });
      prev.value = { question: q, sql: '', clarification: res.clarification };
    } else {
      notes.value.push({ q, text: res.answer ?? '', kind: 'answer' });
    }
    question.value = '';
  } catch (e) {
    askError.value = (e as Error).message;
  } finally {
    asking.value = false;
  }
}

// askSuggested fills the box with the chart's suggested follow-up and submits it —
// a one-click next step that runs through the same ask() path (a fresh data turn).
function askSuggested(q: string) {
  if (asking.value) return;
  question.value = q;
  void ask();
}

// newChat clears the whole ask thread — result, notes, the follow-up context, and any
// open board view — so the screen returns to a blank "ask" state. Saved boards stay.
function newChat() {
  question.value = '';
  askError.value = '';
  result.value = null;
  notes.value = [];
  askedQuestion.value = '';
  prev.value = null;
  activeBoard.value = null;
}

// ── Boards ───────────────────────────────────────────────────────────────────
const boards = ref<AskBoardSummary[]>([]);
const activeBoard = ref<AskBoard | null>(null);
// Per-chart live data fetched by re-running its stored SQL.
const chartData = reactive<Record<string, { columns: string[]; rows: unknown[][] } | 'loading' | 'error'>>({});

const saveTarget = ref<string>(''); // board id, or '__new__'
const newBoardName = ref('');
const saving = ref(false);

async function loadBoards() {
  boards.value = await api.listBoards();
}

async function openBoard(id: string) {
  activeBoard.value = await api.getBoard(id);
  for (const ch of activeBoard.value.charts) void runChart(ch);
}

async function runChart(ch: AskBoardChart) {
  chartData[ch.id] = 'loading';
  try {
    // Saved charts store their window, so reopening a board shows live data over the
    // span the chart was created with, not a default 24h.
    chartData[ch.id] = await api.runSql(replayOf(ch), { windowHours: ch.windowHours });
  } catch {
    chartData[ch.id] = 'error';
  }
}

// Loaded {columns, rows} for a board chart, or null while loading/errored.
function loadedData(ch: AskBoardChart) {
  const d = chartData[ch.id];
  return !d || d === 'loading' || d === 'error' ? null : d;
}

function boardChartOption(ch: AskBoardChart) {
  const d = loadedData(ch);
  return d ? withDataset(ch.echartOption, d.columns, d.rows) : null;
}

async function saveToBoard() {
  if (!result.value || saving.value) return;
  saving.value = true;
  try {
    let boardId = saveTarget.value;
    if (boardId === '__new__' || !boardId) {
      const name = newBoardName.value.trim() || 'My Board';
      const created = await api.createBoard(name);
      boardId = created.id;
      await loadBoards();
    }
    await api.addBoardChart(boardId, {
      question: askedQuestion.value,
      sql: result.value.sql,
      spec: result.value.spec,
      factoryId: result.value.spec ? scope.value.factoryId : undefined,
      echartOption: result.value.echartOption,
      windowHours: result.value.windowHours,
    });
    saveTarget.value = boardId;
    newBoardName.value = '';
    await openBoard(boardId);
    // Clear the compose result now that it lives on a board.
    result.value = null;
    notes.value = [];
    question.value = '';
    prev.value = null;
  } catch (e) {
    askError.value = (e as Error).message;
  } finally {
    saving.value = false;
  }
}

// ── Board rename (inline, in the board header) ──────────────────────────────
const renamingBoard = ref(false);
const renameText = ref('');
const renameInput = ref<HTMLInputElement | null>(null);

function startRename() {
  if (!activeBoard.value) return;
  renameText.value = activeBoard.value.name;
  renamingBoard.value = true;
  void nextTick(() => renameInput.value?.focus());
}

// Enter and blur both land here; Esc flips renamingBoard off first, so the
// following blur is a no-op via the guard.
async function saveRename() {
  if (!renamingBoard.value || !activeBoard.value) return;
  renamingBoard.value = false;
  const name = renameText.value.trim();
  if (!name || name === activeBoard.value.name) return;
  try {
    await api.renameBoard(activeBoard.value.id, name);
    activeBoard.value.name = name;
    await loadBoards(); // refresh the chips
  } catch (e) {
    askError.value = (e as Error).message;
  }
}

async function deleteChart(ch: AskBoardChart) {
  if (!activeBoard.value) return;
  await api.deleteBoardChart(activeBoard.value.id, ch.id);
  await openBoard(activeBoard.value.id);
}

async function removeBoard(id: string) {
  await api.deleteBoard(id);
  if (activeBoard.value?.id === id) activeBoard.value = null;
  await loadBoards();
}

onMounted(() => {
  void loadBoards();
  void loadScopes();
});
</script>

<template>
  <div class="flex h-full min-h-screen">
    <!-- Main — boards live as chips above the ask bar, no second sidebar -->
    <div class="flex-1 overflow-y-auto p-8 lg:p-10">
      <!-- Ask bar -->
      <div class="mx-auto max-w-7xl">
        <div class="flex items-center gap-3 text-white">
          <Sparkles class="h-7 w-7 text-primary-400" />
          <h1 class="text-2xl font-bold lg:text-3xl">{{ heading }}</h1>
        </div>
        <p class="mt-2 text-base text-gray-500">Ask in plain language — a chart is generated to answer you.</p>

        <!-- Scope picker: one plant per question, so the generated prompt stays the
             same size however many plants exist. Hidden until there is a choice. -->
        <div v-if="scopes.length > 1" class="mt-4 flex flex-wrap items-center gap-2">
          <span class="text-xs uppercase tracking-wide text-gray-500">Data</span>
          <button
            v-for="s in scopes" :key="s.dataset + s.factoryId"
            class="rounded-full border px-3 py-1.5 text-sm transition-colors"
            :class="scope.dataset === s.dataset && scope.factoryId === s.factoryId
              ? 'border-primary-500/60 bg-surface-200 text-white'
              : 'border-white/10 text-gray-400 hover:bg-surface-200/60 hover:text-gray-200'"
            :disabled="asking"
            @click="switchScope(s)"
          >
            {{ s.label }}
          </button>
        </div>

        <!-- Boards as chips: [+ New] [board ³] [board ⁵] — replaces the old second sidebar -->
        <div class="mt-5 flex flex-wrap items-center gap-2">
          <button
            class="flex items-center gap-1.5 rounded-full border border-primary-500/40 px-4 py-2 text-sm font-medium text-primary-300 transition-colors hover:bg-primary-500/10 disabled:opacity-50"
            :disabled="asking"
            title="Clear the screen and start a fresh question"
            @click="newChat"
          >
            <Plus class="h-4 w-4" /> New
          </button>
          <button
            v-for="b in boards" :key="b.id"
            class="group flex items-center gap-2 rounded-full border px-4 py-2 text-sm transition-colors"
            :class="activeBoard?.id === b.id
              ? 'border-primary-500/60 bg-surface-200 text-white'
              : 'border-white/10 text-gray-400 hover:bg-surface-200/60 hover:text-gray-200'"
            @click="openBoard(b.id)"
          >
            <span class="max-w-[10rem] truncate">{{ b.name }}</span>
            <span class="rounded-full bg-surface-300 px-1.5 text-xs text-gray-400">{{ b.chartCount }}</span>
            <Trash2 class="h-3.5 w-3.5 opacity-0 transition-opacity group-hover:opacity-100 hover:text-red-400" @click.stop="removeBoard(b.id)" />
          </button>
        </div>

        <div class="mt-6 flex gap-3">
          <textarea
            v-model="question"
            rows="3"
            :placeholder="askPlaceholder"
            class="flex-1 resize-none rounded-xl border border-white/10 bg-surface-100 px-5 py-4 text-base text-gray-200 outline-none focus:border-primary-500"
            @keydown.enter.exact.prevent="ask"
          />
          <button
            class="flex items-center gap-2 rounded-xl bg-primary-500 px-7 py-4 text-base font-semibold text-white transition-colors hover:bg-primary-600 disabled:opacity-50"
            :disabled="asking || !ready || !question.trim()"
            @click="ask"
          >
            <Loader2 v-if="asking" class="h-5 w-5 animate-spin" />
            <Sparkles v-else class="h-5 w-5" />
            Ask
          </button>
        </div>
        <p class="mt-2 text-sm text-gray-600">Press Enter to ask · Shift+Enter for a new line</p>

        <p v-if="askError" class="mt-4 rounded-lg bg-red-500/10 px-4 py-3 text-base text-red-300">{{ askError }}</p>

        <!-- Follow-up answers — own card, above the chart so fresh answers stay visible -->
        <div v-if="notes.length" class="mt-10 rounded-2xl border border-white/10 bg-surface-100 p-6 lg:p-8">
          <div class="mb-4 flex items-center gap-2 text-[11px] font-bold uppercase tracking-widest text-gray-500">
            <Sparkles class="h-3.5 w-3.5 text-primary-400" /> Answers
          </div>
          <div class="space-y-7">
            <div v-for="(n, i) in notes" :key="i">
              <div class="flex items-start gap-2.5">
                <User class="mt-0.5 h-4 w-4 flex-shrink-0 text-primary-400" />
                <p class="text-sm font-semibold text-primary-300">{{ n.q }}</p>
              </div>
              <div class="mt-2.5 flex items-start gap-2.5">
                <Sparkles class="mt-1 h-4 w-4 flex-shrink-0 text-gray-500" />
                <p v-if="n.kind === 'clarification'" class="whitespace-pre-wrap text-base italic leading-relaxed text-amber-300">{{ n.text }}</p>
                <div v-else class="md-answer min-w-0 flex-1 text-base leading-relaxed text-gray-200" v-html="renderMd(n.text)" />
              </div>
            </div>
          </div>
        </div>

        <!-- Chart / table result — its own card -->
        <div v-if="result" class="mt-10 rounded-2xl border border-white/10 bg-surface-100 p-6 lg:p-8">
          <h2 class="mb-1 text-lg font-semibold text-white">{{ askedQuestion }}</h2>
          <!-- Caption comes from the chart author itself (same call, no extra LLM round) —
               it names the ACTUAL bucket, which can be coarser than the question asked for. -->
          <p v-if="result.caption" class="mb-5 text-sm text-gray-400">{{ result.caption }}</p>
          <div v-else class="mb-5"></div>

          <template v-if="result">
          <div v-if="resultIsEmpty" class="rounded-lg border border-white/5 bg-surface-200/50 px-5 py-8 text-center text-base text-gray-500">No data matched — try a wider time range or check the machine name.</div>
          <div v-else-if="resultIsTable" class="max-h-[40rem] overflow-auto rounded-lg border border-white/5">
            <table class="w-full text-left text-sm text-gray-300">
              <thead class="sticky top-0 bg-surface-200 text-gray-400">
                <tr><th v-for="col in result!.columns" :key="col" class="px-4 py-2 font-semibold">{{ col }}</th></tr>
              </thead>
              <tbody>
                <tr v-for="(row, i) in result!.rows" :key="i" class="border-t border-white/5">
                  <td v-for="(cell, j) in row" :key="j" class="px-4 py-2">{{ cell }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <div v-else-if="resultOption">
            <!-- Zooming the slider re-queries the range at a finer bucket (see onZoom). -->
            <div v-if="drilled || drilling" class="mb-3 flex flex-wrap items-center gap-2.5 text-sm">
              <Loader2 v-if="drilling" class="h-4 w-4 animate-spin text-gray-400" />
              <span v-if="drilled" class="text-gray-400">Zoomed in — showing this range at a finer interval.</span>
              <button v-if="drilled" class="flex items-center gap-1.5 rounded-lg bg-primary-500 px-3.5 py-1.5 font-medium text-white transition-colors hover:bg-primary-600 disabled:opacity-50" :disabled="drilling"
                @click="zoomOut"><ZoomOut class="h-4 w-4" /> Zoom out</button>
              <button v-if="drilled" class="flex items-center gap-1.5 rounded-lg border border-white/15 px-3.5 py-1.5 font-medium text-gray-300 transition-colors hover:bg-surface-200 disabled:opacity-50" :disabled="drilling"
                @click="resetZoom"><RefreshCw class="h-4 w-4" /> Reset range</button>
            </div>
            <div class="h-[40rem] w-full">
              <v-chart :option="resultOption" theme="cpf-dark" autoresize @datazoom="onZoom" />
            </div>
          </div>

          <!-- Auto-analysis + suggested next question — folded into the chart's own LLM
               call (no extra round-trip), grounded in the per-machine summary. Both are
               optional; each hides when the model didn't return it. -->
          <div v-if="!resultIsEmpty && (result.analysis || result.nextQuestion)" class="mt-6 space-y-3">
            <p v-if="result.analysis" class="rounded-lg border border-white/5 bg-surface-200/40 px-4 py-3 text-sm leading-relaxed text-gray-300">{{ result.analysis }}</p>
            <button v-if="result.nextQuestion" type="button" :disabled="asking"
              class="inline-flex items-center gap-2 rounded-full border border-primary-500/40 bg-primary-500/10 px-4 py-2 text-sm text-primary-200 transition-colors hover:bg-primary-500/20 disabled:opacity-50"
              @click="askSuggested(result!.nextQuestion!)">
              <Sparkles class="h-4 w-4 shrink-0" /> {{ result.nextQuestion }}
            </button>
          </div>

          <!-- Save to board -->
          <div v-if="!resultIsEmpty" class="mt-6 flex flex-wrap items-center gap-3 border-t border-white/5 pt-6">
            <select v-model="saveTarget" class="rounded-lg border border-white/10 bg-surface-200 px-4 py-2.5 text-base text-gray-300 outline-none">
              <option value="__new__">＋ New board…</option>
              <option v-for="b in boards" :key="b.id" :value="b.id">{{ b.name }}</option>
            </select>
            <input
              v-if="saveTarget === '__new__' || !saveTarget"
              v-model="newBoardName"
              placeholder="Board name"
              class="rounded-lg border border-white/10 bg-surface-200 px-4 py-2.5 text-base text-gray-200 outline-none focus:border-primary-500"
            />
            <button
              class="flex items-center gap-2 rounded-lg bg-surface-200 px-5 py-2.5 text-base font-medium text-white hover:bg-surface-300 disabled:opacity-50"
              :disabled="saving"
              @click="saveToBoard"
            >
              <Save class="h-5 w-5" /> Save to board
            </button>
          </div>
          </template>
        </div>
      </div>

      <!-- Active board -->
      <div v-if="activeBoard" class="mx-auto mt-14 max-w-7xl">
        <div class="mb-6 flex items-center gap-2">
          <input
            v-if="renamingBoard"
            ref="renameInput"
            v-model="renameText"
            class="rounded-lg border border-primary-500/60 bg-surface-200 px-3 py-1.5 text-xl font-bold text-white outline-none"
            @keydown.enter.prevent="saveRename"
            @keydown.esc="renamingBoard = false"
            @blur="saveRename"
          />
          <template v-else>
            <h2 class="text-xl font-bold text-white">{{ activeBoard.name }}</h2>
            <button class="text-gray-500 transition-colors hover:text-gray-200" title="Rename board" @click="startRename">
              <Pencil class="h-4 w-4" />
            </button>
          </template>
        </div>
        <div v-if="activeBoard.charts.length === 0" class="text-base text-gray-500">This board is empty.</div>
        <div class="grid grid-cols-1 gap-8">
          <div v-for="ch in activeBoard.charts" :key="ch.id" class="rounded-2xl border border-white/10 bg-surface-100 p-6">
            <div class="mb-4 flex items-start gap-3">
              <p class="flex-1 text-base font-medium text-gray-200">{{ ch.question }}</p>
              <button class="text-gray-500 hover:text-gray-200" title="Re-run" @click="runChart(ch)"><RefreshCw class="h-5 w-5" /></button>
              <button class="text-gray-500 hover:text-red-400" title="Delete" @click="deleteChart(ch)"><Trash2 class="h-5 w-5" /></button>
            </div>
            <div v-if="loadedData(ch) && isTabular(ch.echartOption)" class="max-h-[34rem] overflow-auto rounded-lg border border-white/5">
              <table class="w-full text-left text-sm text-gray-300">
                <thead class="sticky top-0 bg-surface-200 text-gray-400">
                  <tr><th v-for="col in loadedData(ch)!.columns" :key="col" class="px-4 py-2 font-semibold">{{ col }}</th></tr>
                </thead>
                <tbody>
                  <tr v-for="(row, i) in loadedData(ch)!.rows" :key="i" class="border-t border-white/5">
                    <td v-for="(cell, j) in row" :key="j" class="px-4 py-2">{{ cell }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
            <div v-else-if="boardChartOption(ch)" class="h-[34rem] w-full">
              <v-chart :option="boardChartOption(ch)" theme="cpf-dark" autoresize />
            </div>
            <div v-else-if="chartData[ch.id] === 'loading'" class="flex h-[34rem] items-center justify-center text-gray-600">
              <Loader2 class="h-6 w-6 animate-spin" />
            </div>
            <div v-else class="flex h-[34rem] items-center justify-center text-base text-red-400">Failed to load data.</div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* Rendered markdown answers — v-html children need :deep(). Dark-theme, compact. */
.md-answer :deep(p) { margin: 0.5rem 0; }
.md-answer :deep(p:first-child) { margin-top: 0; }
.md-answer :deep(h1), .md-answer :deep(h2), .md-answer :deep(h3), .md-answer :deep(h4) {
  color: #f3f4f6; font-weight: 600; margin: 1.1rem 0 0.4rem; font-size: 1.05rem;
}
.md-answer :deep(strong) { color: #f3f4f6; }
.md-answer :deep(ul), .md-answer :deep(ol) { margin: 0.5rem 0; padding-left: 1.4rem; }
.md-answer :deep(ul) { list-style: disc; }
.md-answer :deep(ol) { list-style: decimal; }
.md-answer :deep(li) { margin: 0.3rem 0; }
.md-answer :deep(table) {
  display: block; overflow-x: auto; border-collapse: collapse;
  margin: 0.75rem 0; font-size: 0.875rem;
}
.md-answer :deep(th), .md-answer :deep(td) {
  border: 1px solid rgba(255, 255, 255, 0.1); padding: 0.4rem 0.8rem; text-align: left;
}
.md-answer :deep(th) { background: rgba(255, 255, 255, 0.05); color: #d1d5db; font-weight: 600; }
.md-answer :deep(code) {
  background: rgba(255, 255, 255, 0.08); border-radius: 0.25rem;
  padding: 0.1rem 0.35rem; font-size: 0.85em;
}
.md-answer :deep(hr) { border-color: rgba(255, 255, 255, 0.08); margin: 0.75rem 0; }
</style>
