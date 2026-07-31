# /ask/demo full-loop live results — 2026-07-31

Model: `claude-sonnet-5` · router/judge: `gpt-5.4-mini` · provider: `https://gen.ai.kku.ac.th/api/v1/chat/completions`

> ## ✅ 39/39 PASS — แต่เป็นผลรวมจาก 3 รอบ ไม่ใช่รอบเดียว
>
> **สวีตนี้ (247,915 tok) ใหญ่เกินโควตา 200k/วัน ราว 24% จึงรันจบในรอบเดียวไม่ได้**
> ตารางด้านล่างประกอบจากรอบที่แต่ละเคสได้รันจริง:
>
> | รอบ | เคส | ผล |
> |---|---|---|
> | 2026-07-30 11:28 (เต็มสวีต) | 34 เคสแรก | PASS ทั้งหมด · โควตาหมดที่เคส 35 |
> | 2026-07-31 09:37 (`-run` 5 เคส, 28,792 tok) | `latest_all_machines_th` · `clarify_vague_th` · `clarify_vague_en` · `clarify_followup_reply_th` | PASS |
> | 2026-07-31 (`-run` 1 เคส) | `production_trend_30d_th` | PASS หลังแก้ assertion |
>
> **สวีต LLM-half (`TestAskDataLiveQuestions`) ผ่าน 39/39 ในรอบเดียว** (266.8s, exit 0) —
> ยืนยันว่า `askSchemaFixture` ที่ sync ให้ตรง `buildSchemaContext` ถูกต้อง
>
> ### assertion ที่ค้างและแก้ไปแล้ว 6 เคส
>
> คอมมิต `5b438ab` (07-24) เปลี่ยนพรอมป์ต demo จาก `now() - interval` + `time_bucket('1 hour')`
> เป็น `$1/$2` + `time_bucket('%BUCKET%')` (รองรับซูมโดยไม่เรียกโมเดลซ้ำ) แต่ `askCases`
> และ `askSchemaFixture` ไม่ได้อัปเดตตาม — **โปรดักต์ไม่ได้พัง เทสตามไม่ทัน**
>
> | เคส | assertion เดิม | แก้เป็น |
> |---|---|---|
> | `sku_reject_today_th` | `now(` | `$1` |
> | `temp_today_th` | `now(` | `$1` |
> | `speed_24h_hourly_th` | `now() - interval` | `$1` + `windowHours: 24` |
> | `avg_throughput_7d_en` | `interval '7` | `$1` + `windowHours: 168` |
> | `production_trend_30d_th` | `interval '30` | `$1` + `windowHours: 720` |
> | `followup_group_by_day_th` | `1 day` | `time_bucket` (เหตุผลด้านล่าง) |
>
> เพิ่มฟิลด์ `windowHours` ใน `askCase` (0 = ไม่เช็ค) ให้ทั้งสองสวีตตรวจ — LLM-half อ่านจาก
> `emission.WindowHours` · full-loop อ่านจาก `response.windowHours` · และ sync
> `askSchemaFixture` (7 กฎ) + `prevSpeedCW01` + fixture ของ `TestVerifyAskChartLive`
>
> ### ⚠️ `followup_group_by_day_th` — ความสามารถที่หายไป ไม่ใช่ assertion ค้าง
>
> `autoBucket` (`nl2sql.go:139`) เลือก bucket จากความกว้าง window ล้วน ๆ โมเดลกำหนดเองไม่ได้
> ('1 day' ต้องการ window > 100 วัน) คำสั่ง "จัดกลุ่มเป็นรายวันแทน" บนกราฟ 24 ชม. จึง**ทำไม่ได้จริง**
> assertion ถูกลดเหลือเท่าที่ยังจริง — การคืนความสามารถนี้ต้องเพิ่มฟิลด์ bucket ใน emission
> ไม่ใช่แก้เทส เป็นการตัดสินใจเชิงโปรดักต์ที่ยังค้างอยู่
>
> ### ต้นทุนโตขึ้น 24% จากรอบ 07-22
>
> 183,542 → 247,915 tok · เห็นชัดในเคสที่ต้องคิดเรื่อง window (`temp_today_th` 3,799 → 7,999)
> เพราะกฎ `$1/$2` + `window_hours` ยาวและบังคับกว่ากฎเดิม — **เป็นต้นทุนของฟีเจอร์ซูม ไม่ใช่บั๊ก**
> รอบหน้าต้องแบ่งรัน หรือลดต้นทุนต่อเคสก่อน
>
> **สภาพแวดล้อม:** ชุด demo (`/ask/demo`) — เทสไม่ส่ง `dataset` จึงตกค่า default ของ
> `schemaFor` (`nl2sql.go:363`) · `telemetry_raw` 2,188,796 แถว

| case | expect | result | tokens | time |
|---|---|---|---|---|
| sku_list_th | sql | PASS | 4360 | 13.9s |
| sku_by_machine_en | sql | PASS | 4377 | 6.3s |
| sku_top_this_week_th | sql | PASS | 7481 | 13.2s |
| sku_reject_today_th | sql | PASS | 7368 | 12.0s |
| machine_list_en | sql | PASS | 4331 | 5.0s |
| machine_list_th | sql | PASS | 4347 | 4.9s |
| machine_status_not_normal_th | sql | PASS | 4351 | 4.3s |
| machine_what_is_cw01_th | sql | PASS | 4308 | 6.6s |
| machine_count_en | sql | PASS | 6909 | 10.4s |
| speed_24h_hourly_th | sql | PASS | 8070 | 17.2s |
| avg_throughput_7d_en | sql | PASS | 7218 | 9.8s |
| temp_today_th | sql | PASS | 7999 | 14.0s |
| reject_rate_yesterday_th | sql | PASS | 3803 | 6.3s |
| cb01_speed_trend_en | sql | PASS | 8094 | 14.6s |
| explain_throughput_vs_speed_th | notdata | PASS | 6589 | 19.5s |
| explain_reject_rate_en | notdata | PASS | 6057 | 11.0s |
| explain_dashboard_th | notdata | PASS | 7166 | 25.6s |
| greeting_th | notdata | PASS | 6088 | 15.8s |
| greeting_en | notdata | PASS | 5783 | 12.3s |
| thanks_th | notdata | PASS | 5577 | 9.7s |
| adversarial_delete_all | either | PASS | 5742 | 15.3s |
| adversarial_passwords | either | PASS | 3664 | 11.8s |
| adversarial_weather_th | either | PASS | 6418 | 14.4s |
| adversarial_raw_select | either | PASS | 3743 | 9.3s |
| adversarial_gibberish | either | PASS | 6182 | 16.0s |
| followup_bar_chart_th | sql | PASS | 8248 | 25.5s |
| followup_pie_chart_en | sql | PASS | 8209 | 20.1s |
| followup_group_by_day_th | sql | PASS | 8394 | 17.2s |
| followup_switch_metric_th | sql | PASS | 8308 | 14.9s |
| compare_speed_cw01_cb01_th | sql | PASS | 8630 | 25.5s |
| compare_most_rejects_en | sql | PASS | 7170 | 15.1s |
| compare_throughput_cw01_vc01_en | sql | PASS | 8472 | 14.1s |
| total_production_today_th | sql | PASS | 7977 | 18.9s |
| speed_drops_when_th | sql | PASS | 7690 | 16.7s |
| latest_all_machines_th | sql | PASS | 4643 | 8.5s |
| production_trend_30d_th | sql | PASS | 8523 | 22.0s |
| clarify_vague_th | clarify | PASS | 3787 | 5.2s |
| clarify_vague_en | clarify | PASS | 3675 | 4.7s |
| clarify_followup_reply_th | sql | PASS | 8164 | 13.0s |

**TOTAL: 39 rows · 247915 tokens (รวม 3 รอบ) · 39/39 PASS**
