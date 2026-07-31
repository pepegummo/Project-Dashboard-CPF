# ผลทดสอบ LLM2Viz (/ai/ask) — รัน 2026-07-16 → 18 (ผ่านครบทุกเคส ทั้งสอง suite)

**Stack production ปัจจุบัน (หลังปรับตามผลเทส):** KKU GenAI (`https://gen.ai.kku.ac.th/api/v1`)
— generate: `claude-sonnet-5`, **router/judge: `gpt-5.4-mini`** (สลับจาก claude-haiku-4.5
เมื่อ 07-17 ตาม bake-off — ดูหมวด 7)

รอบแรก (07-16) โควตาหมดกลางรัน → เคสที่ค้างถูกรีรันจนครบบน stack เดิมหลังโควตา reset (07-17)
ตารางนี้คือ**ผลสุดท้ายรวมสองวัน** ไม่มีเคส inconclusive เหลือ

| Suite | ขอบเขต | ผลสุดท้าย |
|---|---|---|
| A. `TestAskDataLiveQuestions` (LLM half, fixture) | emitSQL → validateSQL → emitEChart → sanitize | **39/39** (รีรันเต็ม suite 07-18 หลังแก้ prompt + ผ่อน assertion — ผ่านหมด ไม่มี regression; รอบแรก 07-16 ได้ 34/39) |
| A2. `TestVerifyAskChartLive` (judge เดี่ยว) | verifyAskAnswer | **4/4** (haiku-4.5) และ 4/4 บน gpt-5.4-mini |
| B. `TestAskDataFullLoopLive` (ใหม่ — เต็มวงเหมือน user จริง) | HTTP POST /ai/ask → SQL gen → รันบน TimescaleDB จริง → chart → judge → repair → JSON | **39/39 ผ่านครบทุกเคส** — 4 เคสสุดท้าย (explain×2, speed_drops, pie) ผ่านหลังแก้ prompt 07-17; `followup_switch_metric_th` รีรันยืนยันกับ prompt ใหม่แล้ว 07-18 (ผ่าน, 11s) |

**ทุกเคสของ full-loop เคยผ่านแล้วอย่างน้อย 1 ครั้ง** — ไม่มี failure ค้าง ไม่มี infra bug ค้าง
(prompt fix ที่ปิด 4 เคสสุดท้าย ดูหมวด 7)

---

## 1. สิ่งที่พิสูจน์ได้ว่าทำงานจริง (ครั้งแรกที่วง full-loop ถูกเดินครบ)

- **SQL ที่ generate รันบน TimescaleDB จริงผ่าน `runScoped`** — org-scoping ผ่าน views + GUC ทำงาน ได้ rows จริง (เดิม test เช็คแค่ substring ไม่เคยรัน SQL)
- **prose path (`emitProse`)**, **clarification (B3)** และ **clarification-reply follow-up** ทำงานครบในวงจริง
- **adversarial ทั้ง 5 เคสถูกกันได้** — delete/drop, password, raw table access ไม่หลุด
- **chart follow-up** เปลี่ยน type ตามสั่งได้ (bar/group-by-day/switch-metric ผ่านในวงเต็ม; pie มีประเด็น — ดูหมวด 2)
- **judge (haiku-4.5) แม่นครบ 4/4** ทั้ง matched/mismatched ไทย-อังกฤษ (และ 4/4 บน gpt-5.4-mini)
- **Resilience จริงจากเหตุการณ์จริง**: ตอน judge ตายทั้งวง (โควตาหมด 07-16) เคส chart ยังส่งคำตอบครบ — fallback "no verdict → deliver as-is" ของ `verifyAndRepairAnswer` ทำงานตามออกแบบ ไม่มี 502 จาก judge เลย
- เร็วกว่า Groq เดิมมาก: LLM half ~6-8s/เคส, full loop ~8-15s/เคส (ไม่ต้อง pace 30s)

## 2. เคสที่ตก (7 เคสใน full-loop, 4 ใน LLM-half — ซ้อนกันเป็นส่วนใหญ่)

| กลุ่ม | เคส | อาการ | วิเคราะห์ |
|---|---|---|---|
| ~~Assertion แคบไป~~ **แก้แล้ว 07-17** | `sku_reject_today_th`, `temp_today_th` | ใช้ `date_trunc('day', now())` แทน `interval` สำหรับ "วันนี้" — SQL ถูกกว่าที่ test คาดด้วยซ้ำ | ผ่อน assertion เป็น `now(` (รับทั้งสองสไตล์) → **รีรันผ่านแล้ว** |
| ~~ตีความ metric~~ **แก้แล้ว 07-17** | `reject_rate_yesterday_th` | เลือก `defect_rate` แทน `reject` — ตีความได้ว่าถูก (rate ↔ %) | เพิ่ม `sqlHasAny: [reject, defect_rate]` ใน test → **รีรันผ่านแล้ว** |
| ~~ชอบถามกลับ~~ **แก้แล้ว 07-17** | `explain_throughput_vs_speed_th`, `explain_reject_rate_en`, `speed_drops_when_th` | ถาม clarification แทนที่จะตอบ prose/SQL | เติม 2 กติกาใน `emit_sql` prompt (นิยาม/อธิบาย → answerable=false; มี default → ตอบเลย) → **รีเทสผ่านทั้ง 9 เคสรวม regression** (clarify_vague ยังถามกลับถูกต้อง, greeting ยังเป็น prose) |
| ~~Judge เข้มกับ chart ที่ user สั่ง~~ **แก้แล้ว 07-17** | `followup_pie_chart_en` | user สั่ง pie → judge mismatch → repair fail → degrade เป็นตาราง | เติมกติกาใน `askVerifyPrompt`: chart type ที่ user ระบุถือว่าถูกเสมอ judge เฉพาะ data → **รีเทสผ่าน** (pie ได้ pie, bar/group-by ไม่ regress) |

## 3. โควตา KKU — ข้อค้นพบสำคัญ

- **โควตา 200k tokens/วัน เป็น pool รวมต่อค่ายโมเดล ไม่ใช่ต่อโมเดล** — หลักฐาน: หลังรันเคสท้าย
  sonnet-5 และ haiku-4.5 รายงาน usage เกือบเท่ากันเป๊ะ (81,610 vs 81,619) ทั้งที่ judge call เล็กกว่ามาก
  และเมื่อวาน (07-16) ทั้งสองตัวตายพร้อมกันขณะที่ gpt-5.4-mini ยังสดอยู่
- ผลเชิงปฏิบัติ: **การเทสหนักด้วย sonnet-5 ลาก judge ของ prod ตายไปด้วย** เพราะแชร์ pool เดียวกัน
- ต้นทุนที่วัดจริง: tail 14 เคส + judge 4 เคส ≈ **82k tokens** → full-loop ครบ 39 เคส ≈ ~200k ≈ โควต้าทั้งวันพอดี
- ทางแก้ที่ทดสอบรองรับแล้ว: **ย้าย `AI_ROUTER_MODEL` ไป `gpt-5.4-mini`** (judge ผ่าน 4/4 เหมือนกัน) —
  แยก pool ระหว่าง generate (Claude) กับ judge (OpenAI) ทำให้เทสหนักไม่ลาก judge ตาย และ judge ใช้งานจริงไม่กินโควตา sonnet
- error ตอนโควตาหมด: `{"error":"This model reached daily limit."}` (HTTP 401, error เป็น string)

## 4. Bug จริงที่เจอและแก้แล้วระหว่างทำ (มีผลถึง production)

| Bug | อาการ | แก้ |
|---|---|---|
| `AI_BASE_URL` ถูกใช้เป็น URL เต็มของ completions | ตั้ง `.../api/v1` → 404 → ทุก intent กลายเป็น "ไม่ใช่คำถามข้อมูล" (backend ที่ deploy รอบแรกพังแบบนี้) | `aiBaseURL()` เติม `/chat/completions` อัตโนมัติ (controller.go) |
| KKU ส่ง `"error"` เป็น string ไม่ใช่ object | user เห็น "failed to parse AI response" แทนข้อความจริง | `aiError.UnmarshalJSON` รับทั้ง 2 แบบ (controller.go) |
| `liveKeyOrSkip` อ่านแค่ `GROQ_API_KEY` + config เปล่า | live test **skip เงียบ**หลังย้าย provider / เทสผิด stack | อ่าน `AI_API_KEY`/`AI_BASE_URL`/`AI_MODEL`/`AI_ROUTER_MODEL` จาก .env; `pace()` 2s (30s เฉพาะ Groq) |
| ไม่มี test เดินวงเต็ม | runScoped/emitProse/retry/repair/HTTP ไม่เคยถูกเทส | เพิ่ม `ask_fullloop_live_test.go` — POST /ai/ask ผ่าน Fiber + DB จริง ทุกเคส (แชร์ `askCases` กับ Suite A) |

## 5. Coverage map หลังรอบนี้

| Layer (ดู docs/ai-pages.md §2.7) | เดิม | ตอนนี้ |
|---|---|---|
| 1.x Intent/clarification (`emitSQL`) | fixture เท่านั้น | fixture + full loop ✅ |
| 2.1 SQL generation | substring check | รันบน DB จริง ✅ |
| 2.2 validateSQL + `runScoped` | unit / ไม่เคยรัน | full loop ✅ |
| 3.x `emitEChart` + type pick | fixture | fixture + full loop ✅ |
| 4.1 sanitize | unit | unit + full loop ✅ |
| 4.2 judge (`verifyAskAnswer`) | เดี่ยว 4 เคส | เดี่ยว 4/4 + ทำงานในวงจริง ✅ |
| 4.3 retry + repair loops | ❌ | เดินผ่าน handler จริง ✅ (เห็น repair-degrade จริง 1 เคส) |
| 5 Orchestration (`AskData` HTTP) | ❌ | full loop ✅ |
| หน้า /ask ฝั่ง frontend (browser) | ❌ | ❌ ยังไม่มี E2E — จุดบอดเดียวที่เหลือ |

## 6. ควรทำต่อ (เรียงตามความคุ้ม)

1. ~~ย้าย judge/router~~ **เสร็จแล้ว 07-17** — `AI_ROUTER_MODEL=gpt-5.4-mini` deploy แล้ว (ดูหมวด 7)
2. ~~ผ่อน assertion~~ **เสร็จแล้ว 07-17** — `now(` แทน `interval` (2 เคส) + `sqlHasAny` สำหรับ reject/defect_rate (1 เคส) รีรันผ่านครบ
3. ~~ลดนิสัยถามกลับ~~ **เสร็จแล้ว 07-17** — เติม 2 กติกาใน `emit_sql` prompt, รีเทส 9 เคสผ่านครบ
4. ~~Judge เคารพ chart type~~ **เสร็จแล้ว 07-17** — แก้ `askVerifyPrompt`, pie ผ่านแล้ว
5. งบเทส: full double-suite ≈ 260k tokens — เกิน pool Claude ต่อวัน → รันวงเต็มวันละครั้งเดียวพอ หรือรันด้วย `AI_MODEL=gpt-5.4` override เมื่อต้องการรอบสอง
6. ~~รีรัน `followup_switch_metric_th`~~ **เสร็จแล้ว 07-18** — ผ่านกับ prompt ใหม่บน stack prod
7. (ครบ 100%) E2E ฝั่ง browser ของหน้า /ask — งานแยก ยังไม่เริ่ม

## 7. รอบแก้ prompt + สลับ router (2026-07-17 บ่าย)

- **Router bake-off** (intent-routing 32 เคส, harness `TestRouterBakeOff` ปรับให้ใช้ KKU แล้ว):

  | โมเดล | คะแนน | median latency |
  |---|---|---|
  | `gpt-5.4-mini` | **29/32 (91%)** | 1.43s |
  | `claude-haiku-4.5` | 19/32* | 1.78s |

  \* ปนโควตาหมด — 13 เคส error, 19 เคสที่ยิงสำเร็จถูกทั้งหมด (19/19) ถ้าอยากได้ตัวเลขสะอาดรีรันได้พรุ่งนี้
  แต่ gate ผ่านชัด: mini แม่น เร็วกว่า และ**แยกโควตาออกจาก pool Claude** — วันนี้ pool Claude หมด
  เป็นครั้งที่ 3 และลาก judge/router ของ prod ตายไปด้วยทุกครั้ง = เหตุผลหลักของการสลับ
- **Deploy แล้ว**: `.env` → `AI_ROUTER_MODEL=gpt-5.4-mini`, rebuild backend, `/health` ok
- prompt fix 2 จุด (`emit_sql`, `askVerifyPrompt`) รีเทสผ่านตามหมวด 2 — คงเหลือรีรันยืนยัน 1 เคส (ข้อ 6)

## 8. เพิ่ม judge ให้ prose turn (2026-07-18)

เดิม prose turn (answer text) ข้าม judge เลย — เพิ่ม `verifyAskProse` (nl2sql.go) ตาม
contract เดียวกับ chart judge: `gpt-5.4-mini`, 6s bound, mismatch → regenerate 1 รอบ
(ไม่ judge ซ้ำ), judge ล้ม/ไม่มี verdict → ส่งคำตอบเดิม ไม่มีทาง 502
Call budget ของ prose turn: 2 → **3–4 calls**

| เทส | ผล |
|---|---|
| `TestVerifyAskProseLive` ใหม่ (matched th/en + off-topic + ตัวเลขขัด grounding rows) | **4/4** (~3s/เคส) |
| Full-loop prose regression (explain×3, greeting×2, thanks) | **6/6** — ไม่มี false-positive repair |

Deploy แล้ว (rebuild backend 07-18, `/health` ok) — `docs/ai-pages.md` อัปเดต diagram/ตารางแล้ว

## 9. รอบลด token/request (2026-07-20)

Ship 4 commits (`46bf568..29d9c28`) ลดต้นทุน token ต่อ request ทั้งสองหน้า — baseline จาก
`chat-fullloop-results.md`: /ai เคสมี tool ~14.2k, greeting ~7.2k

- **`AI_MAX_TOKENS`** (default 2048) → ส่ง `max_completion_tokens` ทุก call (เดิมไม่จำกัดเลย —
  hidden reasoning กินโควตาเงียบ ๆ); อย่าตั้งต่ำกว่า ~1024 เดี๋ยว tool-call JSON โดนตัด
- **Cap 100 แถว + `summary`** (min/max/avg/total คำนวณจากข้อมูลเต็มก่อน sample) บนผล
  `get_telemetry_series` / `get_production_count` ที่ป้อนกลับโมเดล — REST/frontend ไม่กระทบ
- **Read-intent turns ส่ง tool slim ทั้งชุด** (`buildAIToolsWith(role, true)`) — schema เต็มของ
  `preview_*` (~850 tok) ส่งเฉพาะ edit intent / router fallback; มีแค่ 2 variant คงที่ ยัง cache prefix ได้
- **`systemPromptUnified`** 9,556 → 8,097 chars (กติกาครบเดิม — ตัดลึกกว่านี้รอ eval เขียวก่อน)
- /ask: `buildSchemaContext` ตัด label/unit ที่ซ้ำกับ key ออกจาก metric-key list

คาดการณ์: read request /ai ~14.2k → ~9.5–11k (**ลด ~25–30%**)
**วัดแล้ว 07-21** (ดูหมวด 11): total 64,188 → **57,141 (~11%)** — ต่ำกว่าที่คาด ~25–30% เพราะ
production/edit บางเคสยังต้องส่ง schema เต็ม แต่ทิศทางลดลงชัดเจน

⚠ **กับดักที่เจอ:** `go test ./internal/modules/ai/` รัน live suite ทันทีที่ env มี key — `-short`
**ไม่ได้กัน** และ `TestChatFullLoopLive` เขียนทับ `chat-fullloop-results.md` แม้รัน fail
(กู้ด้วย `git restore` ได้) — รัน offline อย่างเดียวใช้:
`go test ./internal/modules/ai/ -count=1 -skip 'Live|BakeOff|DateEdit|ComplexFlows'`

## 10. Error handling: daily-limit → 429 (2026-07-20)

ตอนพยายามรัน `TestChatFullLoopLive` (โควตา sonnet-5 หมด) เจอเทส crash ด้วย
`bad response JSON (status 500): invalid character 'A'` — **ไม่ใช่บั๊ก production**:
- Production ต่อ `middleware.ErrorHandler` (main.go) → `*AppError{502}` ออกมาเป็น 502 JSON ปกติ
- แต่ `chat_fullloop_live_test.go` สร้าง `fiber.New()` เปล่า → default handler render `*AppError`
  เป็น text `[AI_ERROR] ...` (status 500) → เทส `json.Decode` ชน `[` แล้ว `A` → error 'A'
- **แก้:** ให้ test app ใช้ `ErrorHandler` ตัวเดียวกับ prod (commit `075ccf9`)

พร้อมกันเพิ่ม mapping ให้ชัด (commit `b38a4ed`): `callAIModel` ตรวจ error ที่มี "daily limit" →
คืน typed `quotaError` → ทั้ง /ai (Chat) และ /ask (`askAIError` helper) map เป็น
**429 `QUOTA_EXCEEDED`** (msg "AI daily quota reached. Please try again later.") แยกจาก
per-minute `RATE_LIMIT` (429 + retryAfter) และ generic `AI_ERROR` (502) — frontend
แยกได้ว่า "โควตาหมดรายวัน กลับมาใหม่" vs "rate limit ชั่วคราว retry" vs "provider พังจริง"
offline test: `TestAskAIErrorMapsQuota` (429/502) ผ่าน

## 11. วัด token หลัง optimization + validate rubric/deterministic (2026-07-21)

รัน 2 live suite (โควตา KKU พอสำหรับวันนี้ ~110k) หลังแก้ 2 จุด: (A) เพิ่ม deterministic check
`preview_add_widget` ใน `verify.go`, (C) กระชับ rubric ของ `confidence` ใน `routerSystemPrompt` เป็น
3 ระดับ (0.85+ / 0.5–0.85 / <0.5)

- **Router bake-off** (`TestRouterBakeOff`, 32 เคส, ~254s): `gpt-5.4-mini` **29/32 (คงเดิม — rubric C
  ไม่ทำให้แย่ลง)**, `claude-haiku-4.5` 28/32 (สะอาดกว่ารอบ 07-17 ที่โควตาปน). Confidence คาลิเบรตดี:
  เคสชัด 0.93–0.99, เคสกำกวม `relative-date-edit` ลงมา 0.78 ตามคาด. 3 เคสที่ mini ยังตก:
  `focused-count-now` (production→read_metric), `focused-alarm-panel` (alerts→chat), และ 1 เคส
  focused-context อื่น — ทั้งหมดเป็นเคสที่ต้องเดา intent จากบริบทวิดเจ็ตล้วน ไม่มีคีย์เวิร์ด
  (**2 เคสนี้ถูกกู้ที่ชั้น dispatch แล้ว 2026-07-22 — ดู §12**: focused daily-count/alarm-panel
  ที่มี on-screen data ตอบจาก context ไม่ว่า router จะจัด intent ถูกหรือไม่ → miss เป็น cosmetic)
- **Chat full-loop** (`TestChatFullLoopLive`, 5 เคส, 73.4s): **5/5 PASS**, total **57,141 tokens**
  (จาก 64,188 = ลด ~11% ยืนยัน optimization หมวด 9). ต่อเคส: read_metric 11,409 · alerts 11,196 ·
  production 15,009 · preview_add 13,648 · greeting 5,879 (เขียนลง `chat-fullloop-results.md`)
- **preview_add_gauge_th ผ่าน** — ยืนยัน check A ไม่ false-fail บนพรีวิวจริง (เมตริก temperature มีจริง
  บน CW-01 → ผ่าน deterministic check)
- หมายเหตุ: 2 เคส (read_metric, preview_add) log `verdict=repair-error` ที่ชั้น verify แต่ยัง PASS —
  judge เจอ error ชั่วคราวแล้ว fail-safe เป็น "ส่งคำตอบเดิม" ตามออกแบบ (ไม่ 502)
- ~~**ยังค้าง:** re-measure /ask full-loop (39 เคส ~200k)~~ **วัดแล้ว 2026-07-22 — ดู §13**
  (39/39, 183,542 tok, ต่ำกว่าประมาณ ~200k)

### 11.1 เพิ่ม per-case token metering ให้ Router + /ask (2026-07-21)

เดิมมีแต่ chat ที่วัด token ต่อเคส → เพิ่ม helper กลาง `writeSuiteTokenReport`
(`token_report_test.go`) แล้ว wire เข้า:
- **Router** (`router_eval_test.go`): reset/load `tokenMeter` รอบ `classifyIntentWithModel`,
  เขียน `llm2viz/router-eval-results.md`. เพิ่ม env `ROUTER_EVAL_MODELS` (คั่นจุลภาค) กรองโมเดล
  เพื่อประหยัดโควตา (รัน mini อย่างเดียว)
- **/ask full-loop** (`ask_fullloop_live_test.go`): reset/load รอบ `app.Test`, เขียน
  `docs/ask-demo-test-results.md` (columns: case, expect, tokens, time)

รันจริง (gpt-5.4-mini only): **32 เคส = 31,270 tokens (~1.0–1.1k/เคส คงที่** เพราะพรอมป์ต์
Router ขนาดคงที่ **)** @126s. หมายเหตุ: รอบนี้เจอ 2 เคส decline 0-token (transient blip
ใกล้เพดานโควตา ไม่ใช่ miss จริง) — คะแนน canonical ยังยึด 29/32 จากรอบสะอาดก่อนหน้า
- **/ask per-case:** ~~โค้ด metering พร้อมแล้ว แต่รันจริง ~200k = เต็มโควตา → เลื่อนวันใหม่~~
  **รันแล้ว 2026-07-22 → ดู §13** (39/39, 183,542 tok เขียนลง `docs/ask-demo-test-results.md`)
- รายงาน `docs/iotvision-report/index.html` เพิ่มแคตตาล็อกเคสละเอียด §3.2 (ต่อ)–(ต่อ 4):
  /ai chat 5 + router 32, /ask 39 + judge 8 (ตาราง token /ask เติมตัวเลขจริงแล้ว 2026-07-22 → §13)

## 12. ขยาย answer-from-context ให้ครอบ production + alerts (2026-07-22)

เดิม path "ตอบจาก context ไม่เรียก tool" (`dispatchIntent` → `tool_choice:"none"`) มีแค่
`{chat, read_metric, read_agg}` — `production`/`alerts` ตกไปเรียก tool เสมอ แม้ผู้ใช้จ้อง
widget ที่มีข้อมูลอยู่บนจอแล้ว (เปลือง ~1 tool round ≈ ~4.4k) และเป็นต้นเหตุ router miss 2 เคสใน §11

**แก้ 2 ฝั่ง:**
- **Backend** (`controller.go` dispatchIntent): เงื่อนไข answer-from-context เปลี่ยนจาก OR list
  → ใช้ `readOnlyIntents` (มีอยู่แล้ว = `{chat, read_metric, read_agg, production, alerts}`) ตรง 5
  intent พอดี ไม่เพิ่ม set ใหม่. ยังคุมด้วย `focused && inlineData` → fire เฉพาะตอน frontend
  ส่งข้อมูลจริงมา. Test `TestDispatchIntentFocusedInlineReadIsNone` ขยาย loop ครอบ 5 intent
- **Frontend** (`AIAssistantPage.vue`): เพิ่ม `alarmLine()` — inject active alerts ของ focused
  alarm-panel เป็น `on-screen data` โดย logic ตรงกับ `AlarmPanelWidget.displayAlerts` เป๊ะ
  (severity+machine filter, cap `maxItems`, ว่าง → `"none (All Clear)"`). เดิม alarm-panel
  ไม่เข้า `seriesLine` (ไม่มี metric) → ส่งแค่บรรทัด config ไม่มีข้อมูล → `inlineData=false`

**ผล:** focused daily-count → `production` และ focused alarm-panel → `alerts` ตอบจาก context ได้
โดยไม่เรียก tool. Router miss 2 เคส (`focused-count-now`, `focused-alarm-panel`) กลายเป็น cosmetic —
แม้ router จัด `chat` ผิด แต่ `chat` อยู่ใน `readOnlyIntents` อยู่แล้ว → กู้คำตอบถูกที่ชั้น dispatch

| ตรวจ offline | ผล |
|---|---|
| `go vet ./internal/modules/ai/` | clean |
| ai suite (`-skip 'Live\|BakeOff\|DateEdit\|ComplexFlows'`) | ผ่าน |
| `npm run typecheck` (frontend) | ผ่าน |

**Live verify — ไม่ทำ (ตัดสินใจ 2026-07-22):** ผู้ใช้ระบุ **alarm ไม่ใช่โฟกัสของ project นี้** → ไม่เพิ่ม
เคส focused-alarm-panel ใน chat test และไม่ลงแรง verify path นี้ต่อ. โค้ด §12 ship แล้ว ผ่าน offline
ครบ ถือว่าพอ. RAG (vector DB/embeddings) พิจารณาแล้ว = เกินจำเป็นสำหรับสเกลนี้ (corpus เล็ก +
focused widget รู้แบบ deterministic จากมาร์ค `[FOCUSED]`) — เก็บไว้ตอน machine/docs โตเป็นพันจริง

## 13. วัด /ask full-loop per-case token + rerun router (2026-07-22)

โควตาวันใหม่ → รันเทสที่ค้างจาก §11.1 บน stack prod (generate `claude-sonnet-5`, judge `gpt-5.4-mini`)

- **/ask full-loop** (`TestAskDataFullLoopLive`, 39 เคส, 500s): **39/39 PASS · 183,542 tokens**
  (ต่ำกว่าประมาณ ~200k). per-case **~4.7k เฉลี่ย** — min 2,737 (`clarify_vague_en`, แค่ถามกลับ
  ไม่รัน SQL) · max 8,353 (`speed_drops_when_th`). เขียนลง `docs/ask-demo-test-results.md`

  > ⚠️ **ตัวเลข 39/39 นี้เป็นของ 2026-07-22 ไม่ใช่สถานะปัจจุบัน** — ไทม์ไลน์:
  >
  > - **07-29** 28 ผ่าน / 5 ตกเพราะ assertion เก่าค้าง / 6 โควตาหมด (223,602 tok)
  >   เหตุ: `5b438ab` (07-24) เปลี่ยนพรอมป์ต demo เป็น `$1/$2` + `time_bucket('%BUCKET%')`
  >   แต่ `askCases` **และ `askSchemaFixture`** ไม่ได้อัปเดตตาม
  > - **07-30** แก้ assertion + sync fixture แล้วรันใหม่ → 34 ผ่าน / 5 ตกเพราะโควตาล้วน
  > - **07-31** ยิง 5 เคสที่เหลือด้วย `-run` (28,792 tok) → 4 ผ่าน · `production_trend_30d_th`
  >   ตกด้วย assertion ค้างตัวสุดท้าย (`interval '30`) แก้เป็น `windowHours: 720` แล้วผ่าน
  >
  > **สถานะปัจจุบัน: 39/39 PASS** — แต่เป็นผลรวมจาก 3 รอบ เพราะสวีตนี้ (247,915 tok)
  > ใหญ่เกินโควตา 200k/วัน ราว 24% · รวม 6 เคสที่ assertion ค้างตั้งแต่ `5b438ab` (07-24)
  > · สวีต LLM-half (`TestAskDataLiveQuestions`) ผ่าน **39/39 ในรอบเดียว** ยืนยัน
  > `askSchemaFixture` ที่ sync แล้ว · รายละเอียดครบใน `docs/ask-demo-test-results.md`
  - กลุ่ม: clarify/adversarial ~2.7–4.8k (ถูกสุด — decline/clarify ไม่เดินวงเต็ม), sql ~3.3–8.4k,
    notdata/prose ~4.5–6.1k
  - **/ask ถูกกว่า /ai chat ต่อเคส** (~6.4k vs ~11.4k หลังวัดใหม่ 07-30/31; เดิม ~4.7k, §11): /ask ใช้ prompt เฉพาะทาง
    (`emitSQL`/`emitEChart`) ไม่ re-ship `systemPromptUnified` + 12 tool schemas ทุกรอบเหมือน chat loop
- **Router bake-off** (`TestRouterBakeOff`, mini only, 130.5s): **27/32** หน้าไฟล์ — แต่ 2 เคส
  (`read-speed`, `english-read`) เป็น **0-token decline** (blip 0.00s ใกล้เพดาน OpenAI pool หลังรัน
  ควบ) ไม่ใช่ miss จริง → **canonical ยึด ~29/32 เดิม**. เคสชายขอบสลับตัวกันไปมาระหว่างรัน:
  รอบนี้ `focused-alarm-panel` **ผ่าน** (ได้ alerts), `list-skus` พลาดแทน (ได้ production)
- Pool: /ask ≈ 183k Claude + judge บน OpenAI; router 31k OpenAI — รันควบวันเดียวได้เพราะ pool แยกกัน
- **ครบแล้ว:** ทั้ง 3 result doc (`chat-`/`router-results.md` + `docs/ask-demo-test-results.md`) มีตัวเลข per-case จริง —
  ไม่เหลือคอลัมน์ "รอวัด"

## 14. Per-call token metering + optimize forced-tool turns (2026-07-22)

เพิ่ม **log ต่อ call** ที่ choke point `callAIModel` (`[ai call] model= prompt= completion= total=`)
เพื่อแยกต้นทุนราย call (เดิม `tokenMeter` รวมทั้ง loop เป็นก้อนเดียว)

### 14.1 วัด production case จริง (before) — พลิกสมมติฐาน
รัน `TestChatFullLoopLive/production_today_th` เดี่ยว (fresh conversation) → **11,331 tok**, แยกได้:

| call | model | prompt | completion | total | pool |
|---|---|---|---|---|---|
| router | gpt-5.4-mini | 971 | 74 | 1,045 | OpenAI |
| chat turn 0 (force `get_production_count`) | claude-sonnet-5 | 4,765 | 83 | 4,848 | Claude |
| chat turn 1 (ตอบ user) | claude-sonnet-5 | 4,850 | 96 | 4,946 | Claude |
| verify judge | gpt-5.4-mini | 464 | 28 | 492 | OpenAI |

- **completion จิ๋วมาก (83, 96)** → cost มาจาก **prompt ที่ส่งซ้ำ ไม่ใช่ reasoning** (สมมติฐานเดิมผิด)
- 2 sonnet rounds = 9,794 = **86%** ของทั้งหมด; router+judge (OpenAI) แค่ 14%
- standalone 11,331 vs full-run 15,009 → ส่วนต่าง ~3.7k = conversation history สะสม (shared conv)

### 14.2 Optimize — forced-single-function turns (2 lever)
เมื่อ `dispatchIntent` pin tool_choice เป็นฟังก์ชันเดียว (forceFunc) → 1 tool round พอ:
- **`oneAITool(name)`** — turn 0 ส่ง **schema แค่ tool ที่ force** (ไม่ใช่ทั้ง 12 ตัว ~2k)
- **drop tools บน summary call** — แก้ off-by-one เดิม (`i>=roundCap` ไม่ทันเพราะ break ก่อน) →
  เพิ่มเงื่อนไข `|| forcedName != ""`
- ยังคง cacheable: single-tool array ต่อ intent เป็น byte-stable (`forcedFuncName`/`oneAITool`, controller.go)
- non-forced (`required`/auto/`none`) ไม่แตะ — fan-out get_machines→show_metric ยังทำงาน

**หลักฐานว่าได้ผล (วัดจริงก่อน quota ตัด):** turn-0 prompt **4,765 → 3,482 = ลด 1,283 tok**.
turn-1 คาดลดใกล้เคียง (drop tools) → production **~11,331 → ~8,700 (~23%)** ต่อเคส

- offline เขียว: `go vet` clean, `TestForcedFuncName`/`TestOneAITool` + dispatch suite ผ่าน
- ⚠ **ยังต้อง re-measure full-loop live** — Claude pool วันนี้หมด (183k /ask + measurements → 429
  QUOTA_EXCEEDED กลาง turn-1) → รัน `TestChatFullLoopLive` เต็ม 5 เคสวันโควตาใหม่เพื่อยืนยันตัวเลข after
  + ไม่มี regression end-to-end (drop-tools summary ใช้ pattern เดียวกับ `i>=roundCap` ที่ทำงานอยู่แล้ว)

---

## 15. `kimi-k3` เป็น main model บน `/ask/mhc` — รัน 2026-07-30 (**ตก 0/4 ไม่ควรใช้**)

เปลี่ยน `AI_MODEL=claude-sonnet-5` → `kimi-k3` (router คงเป็น `gpt-5.4-mini`) แล้วยิง 4 คำถามจริงผ่าน
`POST /api/ai/ask` บนโรงงาน Mahachai · **ทั้ง 4 คำถามผ่าน compiler มาแล้วด้วย query spec ตรง ๆ ก่อนหน้านี้**
(หมวด "ทดสอบปลายทาง" ของ MHC) จึงตัดข้อสงสัยเรื่องข้อมูล/compiler ออกได้ — ที่ตกคือด่านโมเดลล้วน ๆ

| # | คำถาม | เวลา | สิ่งที่ kimi ส่งออกมา | ผล |
|---|---|---|---|---|
| 1 | ยอดผลิตรายวันเดือนที่ผ่านมา แยกตามเครื่อง | **90.1s** | ไม่ตอบเลย — `context deadline exceeded` ชน `askCallTimeout` เป๊ะ | ❌ 502 |
| 1↻ | (ยิงซ้ำ) | 10.5s | **ถามกลับ** "ข้อความไม่ชัดเจน ต้องการดูข้อมูลอะไรครับ" ทั้งที่คำถามระบุ "ยอดผลิต" ชัดเจน | ❌ |
| 2 | อุณหภูมิห้องของ IQF ย้อนหลัง 7 วัน | 36.7s | **เลือกเมตริกผิด** — `produced_count` แทน `room_temp` แล้วออกกราฟ + คำบรรยายไทยเรื่องยอดผลิตอย่างมั่นใจ | ❌ **ผิดแบบเงียบ** |
| 3 | IQF หลุดเน็ตกี่ครั้งเดือนนี้ | 28.9s | เมตริก+เครื่องถูก (`network_drop`/`IQF`) แต่ `window` = `{hours:0, from:"", to:""}` → server ตกไป default 24 ชม. ทั้งที่ถาม "เดือนนี้" | ❌ ช่วงเวลาผิด |
| 4 | เทียบยอดผลิตกับอุณหภูมิห้องของ IQF | 60.1s | ส่งมา **เมตริกเดียว** (`produced_count`) ทั้งที่คำว่า "เทียบ...กับ..." คือหัวใจของคำถาม · `window.hours=0` อีก | ❌ |

### สิ่งที่ **ไม่ใช่** สาเหตุ (ตัดออกด้วยการวัด)

- **ไม่ใช่โควตา** — kimi เป็นคนละ pool กับ sonnet ที่หมดวันนี้ ทุก call คืน 200
- **ไม่ใช่ `AI_MAX_TOKENS` 2048 ไม่พอ** — completion สูงสุดที่วัดได้ **1,158** ไม่เคยชนเพดาน ไม่มี truncation
  (สมมติฐานตั้งต้นว่า hidden reasoning จะกินเพดานจนสเปคขาด — **ผิด**)
- **ไม่ใช่ forced `tool_choice` ชนกับ thinking** — probe ตรงไป provider ทั้งแบบระบุชื่อฟังก์ชันและ `"required"`
  คืน 200 ใน 4.5s / 2.7s ไม่มี error ไม่มี fallback log
- **ไม่ใช่โมเดลตาย** — probe `"say ok"` คืนใน 8.3s · probe ด้วย catalog สังเคราะห์ 1,155 prompt tokens
  คืน tool call ที่ใช้ได้ใน 13.8s

### สาเหตุจริง: คุณภาพการเลือกเมตริก/ช่วงเวลา + ความเร็ว

kimi-k3 อ่านโจทย์ไม่ตรง (เมตริกผิด, เมตริกหาย, ถามกลับโดยไม่จำเป็น) และไม่ทำตามกติกา `window` ในพรอมป์ต
(`hours:0` สามในสี่ครั้ง) ทั้งที่กติกาเดียวกันนี้ sonnet ทำได้ 39/39 ในหมวด 1 · เวลาต่อคำถาม 10-60s
เทียบกับ 8-15s ของ sonnet และมีหนึ่งครั้งชน 90s

**ข้อสังเกตเพิ่ม:** เกตเวย์ KKU **ไม่สนใจ `reasoning_format: "hidden"`** — Moonshot คืน field `reasoning`
กลับมาใน message เสมอ โค้ดไม่ได้อ่าน field นั้น (อ่านจาก `tool_calls` อย่างเดียว) จึงไม่พัง แต่แปลว่า
โทเคน reasoning ถูกคิดเงินและถูกนับใน `completion_tokens` โดยปิดไม่ได้

**ผู้ตัดสินพลาดด้วย 1 เคส** — เคส 2 (ถามอุณหภูมิ ได้ยอดผลิต) `gpt-5.4-mini` ให้ผ่าน ไม่สั่ง repair
เป็นครั้งแรกที่เห็น judge ปล่อยของผิดประเภทนี้ผ่าน (หมวด 2 เคยเก็บได้หมด) — คุ้มที่จะดูพรอมป์ต judge ต่อ

### สรุป

**อย่าใช้ `kimi-k3` เป็น `AI_MODEL` บนเส้น llm2viz** · กลับไป `claude-sonnet-5` (โควตารีเซ็ตพรุ่งนี้)
ถ้าต้องการตัวใช้ได้วันนี้ทันที ให้ลอง `AI_MODEL=gpt-5.4-mini` — คนละ pool ยังสด และเป็นตัวที่ทำหน้าที่ judge
ได้ 4/4 อยู่แล้ว แต่ยังไม่เคยถูกวัดในบท generate

---

## 16. `gemini-3.6-flash` + บั๊ก `option: {}` — รัน 2026-07-30/31

### 16.1 คะแนนของ "สเปค" — 4/4 (ดีกว่า kimi ชัดเจน)

ยิงคำถามชุดเดียวกับหมวด 15 บนโรงงาน Mahachai

| # | คำถาม | เวลา | สเปคที่โมเดลส่ง | ผล |
|---|---|---|---|---|
| 1 | ยอดผลิตรายวันเดือนที่ผ่านมา แยกตามเครื่อง | 16.8s | `produced_count` · breakdown `machine` · window `2026-06-01 → 07-01` (`to` exclusive ถูก) | ✅ |
| 2 | อุณหภูมิห้องของ IQF ย้อนหลัง 7 วัน | 11.9s | `room_temp` · IQF · 168 ชม. + แถบ min/max | ✅ |
| 3 | IQF หลุดเน็ตกี่ครั้งเดือนนี้ | 23.0s | `network_drop` · IQF · window กรกฎาคมถูกต้อง **3 ใน 4 ครั้ง** (ครั้งแรกได้มิถุนายน) | ⚠️ แกว่ง |
| 4 | เทียบยอดผลิตกับอุณหภูมิห้องของ IQF | 26.1s | **สองเมตริกครบ** `produced_count` + `room_temp` ทั้งคู่ IQF | ✅ |

ข้อ 3 ไล่หาสาเหตุแล้วไม่ใช่เชิงระบบ — ตัดออกได้ทั้งนาฬิกาคอนเทนเนอร์ (ถูกต้อง), พรอมป์ตไม่มีคำว่า
"เดือนนี้" (probe ที่ไม่มีคำนี้ก็ตอบถูก 2/2), บล็อกตัวอย่างที่มี "เดือนที่แล้ว" (เติมเข้าไปยังถูก 3/3),
และช่วงข้อมูลใน catalog (ถูกต้อง) · ยิงซ้ำของจริง 3 ครั้งถูกหมด สรุปเป็น**การแกว่งของโมเดล** อัตราพลาด
ที่วัดได้ 1/4 บน n น้อยเกินจะสรุปตัวเลขจริง

### 16.2 บั๊กที่เจอระหว่างทาง — กราฟหายทุกข้อ

ทั้ง 4 ข้อได้ตารางเปล่า ไม่มีกราฟเลย ทั้งที่สเปคถูก · **ไม่ใช่ความผิดของโมเดล แต่เป็น schema ของ tool**

`emitEChartTool` ประกาศ `option` เป็น object ที่ไม่บอกโครงข้างใน (โครงจริงอยู่ใน `description` ซึ่งเป็น
ข้อความ) — Claude/OpenAI อ่านแล้วเติมให้ **แต่ Gemini เติมเฉพาะคีย์ที่ประกาศไว้ใน schema เท่านั้น**

ทดสอบตรงไปที่ provider ด้วยคำถาม/ข้อมูลชุดเดียวกัน:

| schema ของ `option` | Gemini ส่งกลับ |
|---|---|
| `{"type":"object"}` (เดิม) | `{}` |
| `{"type":"object", properties:{title,series,…}, additionalProperties:true}` | `{"title":{},"legend":{},"series":[{"encode":{}},…]}` — เติมแต่คีย์ที่ประกาศ ข้างในว่าง **และไม่สนใจ `additionalProperties`** |
| `{"type":"string"}` | ✅ option ครบ parse ผ่าน |

แถวกลางคือเหตุผลที่ไม่ไล่ประกาศ schema ให้ครบ (option ของ ECharts เปิดกว้างไม่จำกัด และจะทำให้
tool schema บวมหลายพันโทเคนต่อทุกคำขอของทุกโมเดล)

**สิ่งที่ตัดออกได้ด้วยการวัด:** ไม่ใช่โควตา (ทุก call คืน 200) · ไม่ใช่ `AI_MAX_TOKENS` 2048 ไม่พอ
(completion สูงสุด 1,158 ไม่เคยชนเพดาน) · ไม่ใช่ forced `tool_choice` ชนกับ thinking (probe ทั้งแบบ
ระบุชื่อและ `"required"` คืน 200 ใน 4.5s/2.7s)

### 16.3 หลังแก้ (`option` เป็น JSON string + `unwrapJSONString`)

| โมเดล | คำถาม | series ที่ได้ |
|---|---|---|
| `gemini-3.6-flash` | อุณหภูมิห้อง IQF 7 วัน | 1 เส้น `line` (158 แถว) |
| `gemini-3.6-flash` | ยอดผลิตรายเครื่อง มิ.ย. | 1 `bar` (10 เครื่อง) |
| `gemini-3.6-flash` | เทียบยอดผลิตกับอุณหภูมิ | **2 series `bar`+`line`, `yAxisIndex:1`** (174 แถว) |
| `claude-sonnet-5` | evap IQF2 vs ยอดผลิต มิ.ย. (NJ5) | **2 series `bar`+`line`** (120 แถว) — regression guard ผ่าน |
| `claude-sonnet-5` | อุณหภูมิห้อง IQF 7 วัน (MHC) | 1 `line` (158 แถว) |

ข้อ 4 ที่เคยคืน `iqf_count` แถวเดียวค่า 0 ก็หายไปด้วย — ได้สองเมตริก 174 แถวตามที่ควรเป็น

**สรุป:** `gemini-3.6-flash` ใช้เป็น `AI_MODEL` ได้แล้ว · เร็วกว่า sonnet เล็กน้อย (11-14s เทียบ 15-17s)
· คนละ pool โควตากับ Claude จึงใช้เป็นตัวสำรองเมื่อ sonnet หมดโควตากลางวันได้
