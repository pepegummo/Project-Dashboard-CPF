# llm2viz — เส้นทางเต็ม จากฐานข้อมูลอื่น ถึงกราฟบนจอ

เอกสารนี้เป็น **แผนที่** ไม่ใช่คู่มือลงมือ — มีไว้ให้เห็นว่ากล่องที่เรา INSERT ลงทะเบียนไป ไปโผล่ตรงไหนของคำตอบ พอจะลงมือจริงให้ข้ามไป [`onboarding-new-source.md`](onboarding-new-source.md)

---

## §0 ภาพเดียวจบ

```mermaid
flowchart LR
  subgraph IN["ครึ่งซ้าย — พาข้อมูลเข้ามา"]
    direction LR
    EXT[("DB อื่น / ดัมป์")] --> LAND["landing<br/>ดิบ ไม่แก้ ไม่ลบ"]
    LAND --> REG["ทะเบียน<br/>source_tables + source_metrics"]
    REG --> NORM["normalizer"]
  end

  NORM --> CANON[("canonical<br/>series + readings")]

  subgraph OUT["ครึ่งขวา — ตอบคำถาม"]
    direction LR
    CAT["catalog → พรอมป์ต"] --> SPEC["🤖 query spec"]
    SPEC --> COMP["compiler เขียน SQL"]
    COMP --> VIZ["📈 กราฟ / ตาราง"]
  end

  CANON --> CAT
  COMP -.->|"อ่าน"| CANON
```

**จุดต่อคือ `series` / `readings`** — ครึ่งซ้ายมีหน้าที่เดียวคือพาข้อมูลมาถึงตรงนั้น ครึ่งขวาไม่รู้จักอะไรก่อนหน้านั้นเลย ไม่รู้ว่าต้นทางเป็น MySQL หรือ CSV ไม่รู้ว่า landing ชื่ออะไร รู้แค่ว่ามี series อะไรบ้างและแต่ละอันเป็นเมตริกชนิดไหน

เพราะฉะนั้น **การเพิ่มฐานข้อมูลใหม่ไม่แตะครึ่งขวาเลย** และการแก้ครึ่งขวาก็ไม่ต้องรู้เรื่องต้นทาง

---

## §1 ครึ่งซ้าย — ข้อมูลเข้าระบบ

```mermaid
flowchart TD
  S(["📦 มีข้อมูลชุดใหม่"]) --> Q1{"อยู่ในฐานของแอปแล้ว?"}

  Q1 -->|"ยังไม่อยู่<br/>เป็น Postgres อีกเครื่อง"| FDW["postgres_fdw<br/>IMPORT FOREIGN SCHEMA<br/>เช็ค EXPLAIN ว่า push down ลง"]
  Q1 -->|"ยังไม่อยู่<br/>MySQL · SQL Server · OPC-UA · CSV"| ETL["ETL ลงตาราง landing<br/>เป็นงานประจำ"]
  Q1 -->|"อยู่แล้ว"| Q2

  FDW --> Q2{"1 แถว = 1 เวลา + 1 เครื่อง<br/>+ ค่าหลายคอลัมน์?"}
  ETL --> Q2

  Q2 -->|"ไม่ใช่<br/>แถวละหนึ่งเมตริก / ชื่อเครื่องคนละตาราง"| VIEW["ครอบ view ให้ได้ทรง<br/>ส่งค่าดิบ ห้ามรวมค่าล่วงหน้า"]
  Q2 -->|"ใช่ — ทรงพอดี"| REG
  VIEW --> REG["INSERT source_tables<br/>+ source_metrics<br/>⚠️ kind ของแต่ละเมตริก"]

  REG --> RUN["./normalize-backfill"]
  RUN --> CANON[("series + readings<br/>+ rollup 1h / 1d")]
  CANON --> ASK(["scope โผล่ใน /ask เอง<br/>ไม่แตะโค้ดหน้าบ้าน"])

  REG -.->|"หลังจากนี้"| W["normalizer worker<br/>ไล่ต่อเองทุก 30 วินาที"]
```

**สองอย่างที่ต้องจำ**

- `kind` คือช่องเดียวในทะเบียนที่ต้องคิดจริง ๆ — compiler เลือกสูตรรวมค่าจากช่องนี้ (`counter` → ผลต่าง · `gauge` → เฉลี่ยถ่วงน้ำหนัก · `event` → นับ · `state` → ค่าท้ายช่วง) ลงผิดแล้วคำตอบผิดแบบเงียบ ๆ ไม่มี error ให้เห็น
- `source_tables.reader` มีค่าเดียวคือ `'poll'` — **ข้อมูลต้องมาอยู่ในฐานของแอปเสมอ** normalizer ไม่ต่อออกไปข้างนอก

📖 ขั้นตอนจริง + SQL + กับดัก + วิธีถอย → [`onboarding-new-source.md`](onboarding-new-source.md) §0 มีตารางตัดสินว่าตกเคสไหน

---

## §2 ครึ่งขวา — คำถามกลายเป็นกราฟ

เส้นนี้คือเส้น `canonical` (`/ask/<slug ของโรงงาน>`) ส่วน `/ask/demo` เดินคนละเส้น — เทียบกันไว้ที่ [`onboarding-new-source.md` §"สองเส้นทาง"](onboarding-new-source.md)

```mermaid
flowchart TD
  Q(["🙋 คำถามภาษาคน<br/>POST /ai/ask"]) --> CAT["loadCatalog<br/>อ่านทะเบียน + series ที่มีจริง"]
  CAT --> P["promptContext<br/>เครื่อง · เมตริก · kind · ป้ายกำกับ"]
  P --> E["🤖 emitSpec<br/>โมเดลส่ง query spec เป็น JSON<br/>บอกแค่ว่าอยากได้อะไร"]
  E --> C["catalog.compile<br/>เซิร์ฟเวอร์เขียน SQL เอง<br/>เลือกสูตรจาก kind + ความละเอียดจากช่วงเวลา"]
  C --> R["runScopedIn<br/>read-only · 5 วินาที · 5000 แถว<br/>RLS ตาม factory"]
  R --> DB[("series + readings")]
  DB --> N{"มีคอลัมน์ตัวเลข?"}
  N -->|"ไม่มี"| T
  N -->|"มี"| CH["🤖 emitEChart → sanitizeEChartOption"]
  CH --> J
  T["📋 ตาราง"] --> J{"verifyAskAnswer<br/>ผู้ตัดสิน 6 วินาที"}
  J -->|"ตรงกับที่ถาม"| OK(["📈 กราฟ / ตาราง"])
  J -->|"ไม่ตรง"| FIX["ซ่อม 1 รอบ<br/>ถ้าซ่อมไม่ได้ ส่งตารางเดิม"]
  FIX --> OK
  OK -.->|"กดบันทึก"| B["board"]

  C -.->|"เมตริกไม่มีในทะเบียน<br/>หรือ SQL พัง"| E
```

**โมเดลไม่เคยเขียน SQL บนเส้นนี้** จึงไม่มีด่าน `validateSQL` — ไม่มี SQL จากภายนอกให้ตรวจ ค่าทุกตัวใน spec ถูก bind หมด และการรวมค่าผิดสูตร "เขียนออกมาไม่ได้" ตั้งแต่แรก

📖 รายละเอียดครบ — retry, ถามต่อเนื่อง, prose path, การเลือก bucket → [`ai-pages.md`](ai-pages.md) §2 (อังกฤษ)

---

## §3 กล่องไหนอยู่ไฟล์ไหน

| กล่อง | อยู่ที่ |
|---|---|
| ทะเบียน | ตาราง `source_tables` · `source_metrics` · `source_state` — สร้างใน `internal/migrate/migrate.go`, ตัวอย่างจริง `scripts/nj5-registry.sql` |
| normalizer — worker ทุก 30 วิ | `backend/internal/normalizer/` |
| normalizer — ลากดัมป์ครั้งเดียว | `backend/cmd/normalize-backfill/` |
| รายชื่อ scope ในตัวเลือก | `catalog.go:211` `ListScopes` → `GET /ai/scopes` |
| catalog → พรอมป์ต | `catalog.go:83` `loadCatalog` · `catalog.go:244` `promptContext` |
| โมเดลส่ง spec | `queryspec.go:69` `emitSpec` · `queryspec.go:134` `parseSpecEmission` |
| spec → SQL | `compiler.go:214` `catalog.compile` |
| รัน SQL | `nl2sql.go:203` `runScopedIn` |
| จุดเข้า `/ai/ask` | `nl2sql.go:1138` `AskData` |
| กราฟ | `nl2sql.go:684` `emitEChart` · `nl2sql.go:838` `sanitizeEChartOption` |
| ผู้ตัดสิน + ซ่อม | `nl2sql.go:784` `verifyAskAnswer` · `nl2sql.go:1430` `verifyAndRepairAnswer` |
| routes ทั้งหมด | `backend/internal/modules/ai/routes.go:29-38` |
| หน้าเว็บ + URL `/ask/:scope` | `frontend/src/pages/AskDataPage.vue` |

ทุกไฟล์ในตารางอยู่ใต้ `backend/internal/modules/ai/` ยกเว้นที่เขียน path เต็มไว้

---

## §4 อ่านต่อ

- [`onboarding-new-source.md`](onboarding-new-source.md) — คู่มือลงมือ 4 เคส + SQL จริง + วิธีถอย (ไทย)
- [`ai-pages.md`](ai-pages.md) — เจาะลึกทั้ง `/ask` และ `/ai` chat ครบทุกด่าน (อังกฤษ)
- [`../llm2viz/test-results.md`](../llm2viz/test-results.md) — ผลทดสอบสด + เรื่องโควตาโทเคน
