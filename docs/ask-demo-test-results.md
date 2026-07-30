# /ask/demo full-loop live results — 2026-07-30 11:28

Model: `claude-sonnet-5` · router/judge: `gpt-5.4-mini` · provider: `https://gen.ai.kku.ac.th/api/v1/chat/completions`

> ## 34 ผ่าน / 5 ตกเพราะโควตา — **ไม่มีเคสไหนตกเพราะ assertion**
>
> ทุก FAIL ในตารางคือ `429 QUOTA_EXCEEDED` โควตารายวันหมดตอน **11:28:26** ที่เคสที่ 35
> (`latest_all_machines_th` ยิงไปได้ 2 คอล = 7,567 tok แล้วคอลที่ 3 โดน
> `401 {"error":"This model reached daily limit."}`) อีก 4 เคสถัดมาได้ `0 tok / 0.1s` คือไม่เคยถูกทดสอบ
>
> ### assertion ที่แก้เมื่อ 2026-07-29 ผ่านครบ ✅
>
> | เคส | 07-29 | 07-30 | ยืนยันอะไร |
> |---|---|---|---|
> | `sku_reject_today_th` | FAIL | **PASS** | `$1` แทน `now(` |
> | `speed_24h_hourly_th` | FAIL | **PASS** | `$1` + `windowHours: 24` |
> | `avg_throughput_7d_en` | FAIL | **PASS** | **โมเดลรายงาน `window_hours: 168` จริง** — ช่วง 7 วันตรวจผ่าน response ได้ |
> | `temp_today_th` | FAIL | **PASS** | `$1` แทน `now(` |
> | `followup_group_by_day_th` | FAIL | **PASS** | assertion ที่ลดเหลือ `time_bucket`+`cw-01` |
>
> การ sync `askSchemaFixture` ให้ตรง `buildSchemaContext` และอัปเดต `prevSpeedCW01` เป็นทรง
> `%BUCKET%`/`$1`,`$2` ก็ได้ผล — 4 เคส `followup_*` ผ่านหมด
>
> ### ปัญหาที่เหลือ: สวีตนี้ใหญ่เกินโควตาไปแล้ว
>
> **226,690 โทเคน** เทียบโควตา 200k/วัน — เกิน ~13% จึงรันไม่จบเป็นรอบที่สองติดกัน
> (07-29 ใช้ 223,602 ตายที่เคส 34 · 07-30 ใช้ 226,690 ตายที่เคส 35)
>
> ต้นทุนต่อเคสสูงขึ้นจากรอบ 07-22 (183,542) ราว 23% — เห็นชัดในเคสที่ต้องคิดเรื่อง window:
> `temp_today_th` 3,799 → 7,999 · `sku_reject_today_th` 3,798 → 7,368 · เข้าใจได้เพราะ
> พรอมป์ตเรื่อง `$1/$2` + `window_hours` ยาวและบังคับมากกว่ากฎ `now() - interval` เดิม
>
> ### 5 เคสที่ยังไม่เคยได้รันจริงเลยทั้งสองรอบ
>
> `latest_all_machines_th` · `production_trend_30d_th` · `clarify_vague_th` · `clarify_vague_en` ·
> `clarify_followup_reply_th`
>
> รันเฉพาะกลุ่มนี้ได้ในราคา ~30k โทเคน โดยไม่ต้องรันทั้งสวีต:
> ```bash
> cd backend && go test ./internal/modules/ai/ -v -timeout 30m \
>   -run 'AskDataFullLoopLive/(latest_all_machines_th|production_trend_30d_th|clarify_vague_th|clarify_vague_en|clarify_followup_reply_th)'
> ```
> ⚠️ คำสั่งนี้เขียนทับไฟล์นี้ด้วยตาราง 5 แถว (harness เขียนทุกครั้งที่จบ) — สำเนาตาราง 39 แถว
> ไว้ก่อนรัน
>
> **สภาพแวดล้อม:** ชุด demo (`/ask/demo`) — เทสไม่ส่ง `dataset` จึงตกค่า default ของ
> `schemaFor` (`nl2sql.go:363`) · `telemetry_raw` 2,188,796 แถว · `go test` exit 1 (จาก FAIL ที่เป็นโควตา)

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
| latest_all_machines_th | sql | FAIL | 7567 | 18.6s |
| production_trend_30d_th | sql | FAIL | 0 | 0.1s |
| clarify_vague_th | clarify | FAIL | 0 | 0.2s |
| clarify_vague_en | clarify | FAIL | 0 | 0.0s |
| clarify_followup_reply_th | sql | FAIL | 0 | 0.1s |

**TOTAL: 39 rows · 226690 tokens · 564.3s**
