# เช็คลิสต์ก่อนรัน `normalize-backfill`

ใช้เมื่อมีดัมป์มากองอยู่ตรงหน้าแล้วจะเอาเข้าระบบ — ไล่จากบนลงล่าง **ห้ามสลับลำดับ**

> ต้องการ SQL ทะเบียนเต็ม ๆ พร้อมค่าจริง → [`onboarding-new-source.md`](onboarding-new-source.md) §3 · อยากเห็นภาพรวมทั้งเส้นทางก่อน → [`llm2viz-flow.md`](llm2viz-flow.md)

**ทำไมสลับไม่ได้:** `source_tables.factory_id` มี FK ไป `factories` และ **normalizer จะยิง `SELECT` ใส่ตาราง landing ทันทีที่ลงทะเบียนเสร็จ** (ตื่นทุก 30 วินาที) — ลงทะเบียนก่อนโหลดข้อมูล = `source_state.last_error` เต็มไปหมด

```
0. เปิดดัมป์ดู  →  1. ฐานพร้อม  →  2. โหลดเข้า landing  →  3. index
→  4. org/factory  →  5. source_tables  →  6. source_metrics  →  7. source_state
→  8. ซ้อมอ่าน  →  9. รัน
```

**ถ้าเป็นตารางที่ลงทะเบียนไว้แล้ว** (ดัมป์ก้อนที่ 2 ของ `nj5_machines`) ข้ามข้อ 4–8 ได้ทั้งหมด เหลือแค่ 2 → 9

---

## 0 · เปิดดัมป์ดูก่อน อย่าเพิ่งโหลด

- [ ] `head -50 dump.sql` — ดัมป์จะ `INSERT INTO` ตารางชื่ออะไร
- [ ] `grep -c "^INSERT" dump.sql` — กี่แถวคร่าว ๆ
- [ ] คอลัมน์เวลาเป็นชนิดอะไร — epoch `bigint` หรือ `timestamptz`
- [ ] 1 แถวหมายถึงอะไร — 1 เวลา + 1 เครื่อง + ค่าหลายคอลัมน์ หรือเปล่า

ชื่อตารางในดัมป์มักเป็น `public.machines` ซึ่ง **ชนกับตารางจริงของแอป** ต้องเปลี่ยนชื่อระหว่างโหลด (ข้อ 2)

ถ้า 1 แถวไม่ใช่ทรง wide → ต้องครอบ view ก่อน ([§4A / §4B](onboarding-new-source.md))

## 1 · ฐานต้องพร้อม

- [ ] `docker compose up -d db backend` — ต้องเคย start อย่างน้อย 1 ครั้ง
- [ ] `docker compose exec db psql -U iot_user -d iot_dashboard -c "\d source_tables"` — ถ้าไม่มีตารางนี้แปลว่า `migrate.RunAll()` ยังไม่เคยวิ่ง ทะเบียนถูกสร้างโดย migration ไม่ใช่สคริปต์
- [ ] `docker compose exec backend ls -la ./normalize-backfill` — ไม่มีไฟล์ ให้ `docker compose build backend` ใหม่หนึ่งครั้ง (อิมเมจที่ build ก่อน 2569-07-27 ยังไม่มี)

> ชื่อผู้ใช้/ฐานมาจาก `.env` (`POSTGRES_USER` / `POSTGRES_DB`) ค่าปัจจุบันคือ `iot_user` / `iot_dashboard` — **ไม่ใช่ `postgres`/`iotdb`** ถ้าเจอ `FATAL: role "postgres" does not exist` แปลว่าลอกคำสั่งเก่ามา
> อยู่บน PowerShell: ไม่มี heredoc และ `<` เป็น operator สงวน — SQL หลายบรรทัดต้องเซฟเป็นไฟล์แล้ว `Get-Content x.sql -Raw | docker compose exec -T db psql …`

## 2 · โหลดเข้าตาราง landing

```bash
sed 's/public\.machines/bn2_oven/g' dump.sql | docker compose exec -T db psql -U iot_user -d iot_dashboard
```

- [ ] เปลี่ยนชื่อตารางระหว่างทาง ไม่ต้องแก้ไฟล์ดัมป์
- [ ] ตาราง landing เก็บ **ค่าดิบ** เท่านั้น — ไม่แก้ ไม่ลบ ไม่กรอง sentinel ทิ้ง

## 3 · index — สร้าง**ทีหลัง**เสมอ

```sql
CREATE INDEX bn2_oven_ts ON bn2_oven (ts);
-- ถ้า ts_expr = 'to_timestamp(time_stamp)' ต้องเป็น functional index ให้ตรงกัน:
CREATE INDEX bn2_oven_ts ON bn2_oven (to_timestamp(time_stamp));
```

- [ ] index ตรงกับ `ts_expr` ที่จะลงทะเบียน **เป๊ะ ๆ**

ผิดข้อนี้ = ตัวอ่าน seq scan ทั้งตารางทุก 30 วินาที ตลอดไป · สร้าง index ก่อนโหลด = จ่ายค่า maintain ทุกแถว

## 4–7 · ลงทะเบียน

- [ ] `organizations` + `factories` (+ `production_lines` ไม่บังคับ — normalizer สร้าง `Auto` ให้เอง)
- [ ] `source_tables` — `ts_expr`, `machine_expr`, `label_exprs`, `overlap_seconds`, `batch_rows`
- [ ] `source_metrics` — หนึ่งแถวต่อคอลัมน์ที่จะเสิร์ฟ **คอลัมน์ที่ไม่ลงทะเบียน = โมเดลมองไม่เห็น** (นั่นคือวิธีบอกว่า "คอลัมน์นี้ว่างทั้งดัมป์")
- [ ] `source_state` — หนึ่งแถวต่อ source

SQL เต็มก๊อปได้จาก [§3](onboarding-new-source.md) หรือดูของจริงที่ `scripts/nj5-registry.sql`

**ช่องเดียวที่ต้องคิดจริงคือ `kind`** — compiler อ่านช่องนี้ช่องเดียวเพื่อเลือกสูตรรวมค่า:

| `kind` | ใช้เมื่อ | ได้อะไร | เลือกผิดแล้ว |
|---|---|---|---|
| `counter` | ตัวนับสะสม | ค่าท้าย − ค่าแรกของช่วง | ใส่ `gauge` → เฉลี่ยยอดสะสม ไร้ความหมาย |
| `gauge` | ค่าวัด ณ ขณะนั้น | เฉลี่ยถ่วงน้ำหนัก | ใส่ `counter` → ผลต่างอุณหภูมิหัว-ท้ายช่วง ดูเหมือนจริงแต่ผิด |
| `event` | ธงว่าเกิดเหตุ | นับจำนวนครั้ง | ใส่ `gauge` → "ค่าเฉลี่ยของการหลุดเน็ต" |
| `state` | สถานะที่คงอยู่จนเปลี่ยน | ค่าท้ายช่วง | ใส่ `gauge` → เฉลี่ยรหัสสถานะเข้าด้วยกัน |

> `shape` (`wide_counter` / `wide_sensor`) เป็น**คำอธิบายล้วน ๆ** — `reader.go` โหลดค่านี้เข้ามาแต่ไม่ได้ใช้ตัดสินใจอะไร ใส่ให้คนอ่านเข้าใจ เลือกผิดไม่พัง

## 8 · ซ้อมอ่านก่อนกดรัน — ด่านที่คุ้มที่สุด

**ยิง query ที่ตัวอ่านจะประกอบ ด้วยมือตัวเอง** (เทียบ `backend/internal/normalizer/reader.go:66-85`) ถ้า query นี้ error ตัวอ่านก็ error เหมือนกัน:

```sql
SELECT (ts) AS __ts, (device_code)::text AS __machine,
       (zone_temp)::double precision   AS __v0,
       (total_count)::double precision AS __v1
FROM "bn2_oven" WHERE (ts) > '1970-01-01' ORDER BY 1 LIMIT 5;
```

แล้วต่ออีก 4 ข้อ:

```sql
-- ① เวลา/ชื่อเครื่องว่าง → reader ข้ามเงียบ ๆ ไม่มี error ให้เห็น
SELECT count(*) FILTER (WHERE ts IS NULL)                           AS ts_null,
       count(*) FILTER (WHERE coalesce(trim(device_code),'') = '')  AS mach_empty
FROM bn2_oven;

-- ② คีย์ซ้ำ → ON CONFLICT DO NOTHING ทิ้งเงียบ ๆ  (ต้องได้ 0 แถว)
SELECT ts, device_code, count(*) FROM bn2_oven
GROUP BY 1,2 HAVING count(*) > 1 LIMIT 5;

-- ③ ช่วงเวลาของดัมป์ + ความถี่ข้อมูล
SELECT min(ts), max(ts), count(*),
       count(*) / GREATEST(EXTRACT(epoch FROM max(ts)-min(ts)), 1) AS rows_per_sec
FROM bn2_oven;

-- ④ ค่าประหลาดที่ควรลงเป็น sentinel
SELECT DISTINCT zone_temp FROM bn2_oven ORDER BY 1 DESC LIMIT 5;
```

อ่านผลยังไง:

| เจอ | ต้องทำ |
|---|---|
| ① ไม่เป็น 0 | ถ้าเป็นส่วนน้อยถือว่าปกติ · ถ้าเยอะแปลว่าเลือก `machine_expr` ผิดคอลัมน์ |
| ② มีแถวออกมา | **หยุด** — ต้องเพิ่ม label ให้ครบมิติ ไม่งั้นข้อมูลหายเงียบ ๆ |
| ③ `max(ts)` เก่ากว่า 30 วัน | **ต้องรัน `normalize-backfill`** ปล่อยให้ worker ทำไม่พอ ค่าสรุปจะไม่ถูก materialize แล้วถามได้ศูนย์ |
| ③ `rows_per_sec` > `batch_rows / overlap_seconds` (ค่า default = 50000/300 ≈ **167**) | เพิ่ม `batch_rows` — เกินเพดานนี้ watermark จะไม่ขยับแล้ววนอ่านชุดเดิมไม่จบ |
| ④ เจอ 9999 / -999 | ลง `sentinel` ในทะเบียน**ก่อน**นำเข้า แก้ทีหลังต้องนำเข้าใหม่ |

## 9 · รัน

```bash
docker compose exec backend ./normalize-backfill
```

- [ ] ไม่ต้องหยุดเซิร์ฟเวอร์ ไม่ต้องหยุด worker — watermark เดินหน้าอย่างเดียว อ่านซ้ำถูก `ON CONFLICT` ทิ้ง
- [ ] ⚠️ **ตัวเลขหลัง `✅` คือ "แถวที่ process นี้อ่านเอง" ไม่ใช่ "แถวที่เข้าระบบ"** ถ้าเซิร์ฟเวอร์รันอยู่ worker จะเห็นทะเบียนใหม่ภายใน 30 วินาทีและลากไปก่อน `batch_rows` ละรอบ · backfill ที่เริ่มทีหลังจึงออกตัวจาก `watermark − overlap_seconds` ไม่ใช่จากศูนย์ แล้วพิมพ์ยอดที่ขาดไปเท่าที่ worker กินไป
      ที่ MHC: พิมพ์ `mhc_ais: 741,649` / `mhc_iqf: 158,421` แต่เข้าครบ 791,049 / 258,371 — ส่วนต่างคือ 1 batch และ 2 batch ที่ worker ลากไปก่อนพอดี (เศษอีก 560 / 40 คือ overlap 300 วิ ที่อ่านซ้ำทุกรอยต่อ batch ของตัวเอง)
      **ยืนยันด้วย SQL เสมอ** — `count(*)` ของ metric ที่ไม่มี NULL ต้องเท่ากับจำนวนแถวใน landing เป๊ะ ๆ นั่นคือหลักฐานว่าไม่มีแถวถูกข้าม ไม่ใช่บรรทัดที่พิมพ์ออกมา
- [ ] ⚠️ **อย่าพิมพ์ `./backfill`** — คนละเครื่องมือ ตัวนั้นเติมข้อมูลสาธิตชุด demo ไม่เกี่ยวกับทะเบียนเลย

ผลที่ควรเห็น:

```
🔄  Backfilling canonical model from registered sources…
   bn2_oven: 500000 rows…
✅ bn2_oven: 1043221 readings
↻  refreshing readings_1h over all time…
↻  refreshing readings_1d over all time…
👍 Done in 14m22s
```

## หลังรันเสร็จ

- [ ] ตรวจตาม [§5 ของ `onboarding-new-source.md`](onboarding-new-source.md) — เส้นข้อมูลที่เกิดขึ้น, สัดส่วนธงคุณภาพ, สถานะการนำเข้า
- [ ] เปิด `/ask/<slug>` แล้วถามจริงหนึ่งคำถาม
- [ ] ลงทะเบียนผิด → [§6](onboarding-new-source.md) มีวิธีถอย

---

# ตัวอย่างเดินครบทุกข้อ

ใช้ **IQF2 ของ NJ5** — SQL ทะเบียนทั้งหมดก๊อปตรงมาจาก `scripts/nj5-registry.sql` ที่ใช้งานอยู่จริง และโครงตาราง landing มาจาก `scripts/nj5-schema.sql` ส่วนค่าในแถวข้อมูลกับผลลัพธ์ที่แสดงเป็นตัวอย่างประกอบ

## ข้อ 0 — เปิดดัมป์ดู

```bash
$ head -25 NJ5_machineIQF2.sql
CREATE TABLE public.iqf2 (
    time_stamp    bigint,          -- epoch seconds
    country       text,
    plant         text,
    plant_id      integer,
    machine_type  text,
    machine_name  text,            -- 'IQF_2'
    area          text,
    air_b_fan1a   real,
    air_b_fan2a   real,
    evap_temp     real,
    rail_temp     real,
    room_temp     real,
    freezing_time real,
    speed_belts   real,
    fail_network  boolean,
    created_at_ts timestamptz,     -- เวลาอีกตัว
    created_at    bigint
);
INSERT INTO public.iqf2 VALUES (1772341200,'TH','Nongjok5',4046,'IQF','IQF_2','Freezing',
    1420, 0, -36.2, NULL, -28.0, 9999, 150, false, '2026-03-01 08:00:00+07', 1772341200);
```

(โครงตารางจริงอยู่ที่ `scripts/nj5-schema.sql` · ค่าในแถวเป็นตัวอย่างประกอบ)

อ่านได้ 5 อย่าง:

| เห็นอะไร | สรุป |
|---|---|
| `public.iqf2` | ต้องเปลี่ยนชื่อเป็น `nj5_iqf2` ตอนโหลด |
| **มีคอลัมน์เวลา 2 ตัว** | เลือก `created_at_ts` (timestamptz) เป็น `ts_expr` — ไม่ต้องแปลง และ index ทำง่ายกว่า `to_timestamp(time_stamp)` |
| 1 แถว = 1 เวลา + 1 เครื่อง + ค่าหลายคอลัมน์ | ทรงพอดี ไม่ต้องครอบ view |
| `country` / `plant` / `plant_id` / `machine_type` ซ้ำทุกแถว | ไม่ต้องลงเป็น label — โรงงานถูกกำหนดที่ `source_tables.factory_id` แล้ว |
| `rail_temp` NULL, `freezing_time` = 9999 | ตัวแรกไม่ลงทะเบียน ตัวหลังลง `sentinel` |

## ข้อ 2–3 — โหลด + index

```bash
sed 's/public\.iqf2/nj5_iqf2/g' NJ5_machineIQF2.sql \
  | docker compose exec -T db psql -U iot_user -d iot_dashboard
```

```sql
CREATE INDEX nj5_iqf2_ts ON nj5_iqf2 (created_at_ts);
```

## ข้อ 4–7 — ลงทะเบียน

```sql
INSERT INTO source_tables (factory_id, table_name, shape, ts_expr, machine_expr,
                           label_exprs, overlap_seconds)
VALUES ('00000000-0000-0000-0001-000000004046', 'nj5_iqf2', 'wide_sensor',
        'created_at_ts', '''IQF2''', '{}'::jsonb, 300);
```

จุดที่คนพลาดบ่อย: `machine_expr` เป็น **ค่าคงที่ `'IQF2'`** ไม่ใช่คอลัมน์ `machine_name` (ที่เก็บ `'IQF_2'`) ตั้งใจให้ resolve ไปเจอ **เครื่องตัวเดียวกัน** กับที่ตัวนับผลิตใน `nj5_machines` ใช้ ถามว่า "อุณหภูมิเทียบกับยอดผลิตของ IQF2" จึงเป็นสองเส้นข้อมูลของเครื่องเดียว ไม่ใช่สูตร join ข้ามตาราง

```sql
INSERT INTO source_metrics (source_table_id, column_name, value_expr, field_key,
                            kind, unit, sentinel, valid_min, valid_max, llm_note)
SELECT st.id, v.* FROM source_tables st, LATERAL (VALUES
  ('evap_temp',     NULL::text,          'evap_temp',     'gauge', '°C', NULL::float, -60.0,  40.0,
   'Evaporator temperature. Normal -34..-41; brief POSITIVE readings up to ~+28 are defrost cycles, not errors.'),
  ('room_temp',     NULL,                'room_temp',     'gauge', '°C', NULL,        -60.0,  40.0, 'Chamber temperature.'),
  ('air_b_fan1a',   NULL,                'fan1_speed',    'gauge', NULL, NULL,          0.0, 2000.0, 'Fan 1 reading; 0 means that fan is idle.'),
  ('air_b_fan2a',   NULL,                'fan2_speed',    'gauge', NULL, NULL,          0.0, 2000.0, 'Fan 2 reading; frequently 0.'),
  ('speed_belts',   NULL,                'belt_speed',    'gauge', NULL, NULL,          0.0, 1000.0, 'Conveyor belt speed, normally around 150.'),
  ('freezing_time', NULL,                'freezing_time', 'gauge', 's',  9999.0,        0.0, 9000.0,
   'Dwell time in seconds. 9999 is a sentinel for idle/no reading and is excluded automatically.'),
  ('fail_network',  'fail_network::int', 'network_drop',  'event', NULL, NULL,          0.0,    1.0,
   'One per reading where the machine lost its network connection. Sum it to count dropouts.')
) AS v(column_name, value_expr, field_key, kind, unit, sentinel, valid_min, valid_max, llm_note)
WHERE st.table_name = 'nj5_iqf2';

INSERT INTO source_state (source_table_id)
SELECT id FROM source_tables WHERE table_name = 'nj5_iqf2';
```

สามอย่างที่ตารางนี้ "พูด" โดยไม่ต้องเขียนพรอมป์ตสักบรรทัด:

- **`rail_temp` ไม่มีในรายการ** → โมเดลมองไม่เห็น ถามถึงไม่ได้ นั่นคือวิธีบอกว่า "คอลัมน์นี้ว่างทั้งดัมป์"
- **`fail_network` มี `value_expr`** → ต้นทางเป็น boolean ต้อง `::int` ก่อน แล้วลง `kind='event'` เพื่อให้ "นับครั้ง" ไม่ใช่ "เฉลี่ย"
- **`freezing_time` มี `sentinel=9999`** → ค่านี้จะถูก**เก็บพร้อมธง** ไม่ใช่ทิ้ง

## ข้อ 8 — ซ้อมอ่าน

```sql
SELECT (created_at_ts) AS __ts, ('IQF2')::text AS __machine,
       (evap_temp)::double precision        AS __v0,
       (freezing_time)::double precision    AS __v5,
       (fail_network::int)::double precision AS __v6
FROM "nj5_iqf2" WHERE (created_at_ts) > '1970-01-01' ORDER BY 1 LIMIT 5;
```

```sql
SELECT min(created_at_ts), max(created_at_ts), count(*),
       count(*) / GREATEST(EXTRACT(epoch FROM max(created_at_ts)-min(created_at_ts)),1) AS rows_per_sec
FROM nj5_iqf2;
--        min        |        max        |  count  | rows_per_sec
-- 2025-08-01 00:00  | 2026-03-01 08:00  | 8920100 |     0.43
```

อ่านผล: `rows_per_sec` 0.43 ห่างจากเพดาน 167 มาก ไม่ต้องแตะ `batch_rows` · `min` ย้อนไป 7 เดือน **เกิน 30 วัน → ต้องรัน `normalize-backfill` แน่นอน** ปล่อยให้ worker ทำไม่พอ

## ข้อ 9 — รัน แล้วได้อะไร

แถวตัวอย่างข้างบนแถวเดียว กลายเป็น **7 แถวใน `readings`**:

| series | value | quality | ทำไม |
|---|---|---|---|
| IQF2 / evap_temp | -36.2 | 0 | อยู่ในช่วง -60..40 |
| IQF2 / room_temp | -28.0 | 0 | |
| IQF2 / fan1_speed | 1420 | 0 | |
| IQF2 / fan2_speed | 0 | 0 | 0 คือ "พัดลมไม่ทำงาน" ไม่ใช่ค่าเสีย |
| IQF2 / belt_speed | 150 | 0 | |
| IQF2 / freezing_time | **9999** | **1** | ตรง `sentinel` — เก็บค่าไว้ แต่ติดธง |
| IQF2 / network_drop | 0 | 0 | `false` → 0 |

`rail_temp` ไม่มีแถว เพราะไม่ได้ลงทะเบียน

**ธงคุณภาพคือหัวใจ** — 9999 ไม่ได้ถูกทิ้ง มันถูกเก็บพร้อมธง `quality=1` แล้ว compiler กรองออกให้ตอนตอบคำถาม ลงทะเบียน sentinel ผิดจึงแก้ได้ด้วยการแก้ทะเบียนแล้วนำเข้าใหม่ ไม่ใช่ข้อมูลหายไปแล้ว

---

# ตัวอย่างที่สอง — ดัมป์ก้อนที่ 2 ของตารางเดิม

`NJ5_machineIQF2_batch2.sql` โครงสร้างเดิม ข้อมูลใหม่ — **แถวทะเบียนที่ต้องเพิ่ม = 0**

```bash
# ข้อ 2
sed 's/public\.iqf2/nj5_iqf2/g' NJ5_machineIQF2_batch2.sql \
  | docker compose exec -T db psql -U iot_user -d iot_dashboard

# ข้อ 9 — เฉพาะกรณีเป็นข้อมูลย้อนหลัง ถ้าเป็นข้อมูลต่อท้ายวันนี้ worker เก็บให้เองใน 30 วินาที
docker compose exec backend ./normalize-backfill
```

จบ · เครื่องใหม่ที่โผล่มาในดัมป์ (เช่น `Packing3`) `resolveMachine` สร้างให้เอง ติดธง `discovery=auto` · SKU/area ใหม่แตกเป็นเส้นข้อมูลใหม่เพราะ `label_exprs` ลงทะเบียนไว้แล้ว
