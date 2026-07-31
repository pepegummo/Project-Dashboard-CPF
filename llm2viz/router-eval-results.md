# Router eval (classify_intent) live results — 2026-07-31 13:59

Model: `claude-sonnet-5` · router/judge: `gpt-5.4-mini` · provider: `https://gen.ai.kku.ac.th/api/v1/chat/completions`

| model | label | message | want | got | pass | tokens | latency |
|---|---|---|---|---|---|---|---|
| gpt-5.4-mini | greeting | สวัสดีครับ | chat | (declined) | FAIL | 0 | 0.00s |
| gpt-5.4-mini | read-speed | speed ของ CW-01 เท่าไหร่ | read_metric | read_metric | PASS | 1120 | 3.12s |
| gpt-5.4-mini | english-read | what's the speed of CW-01 | read_metric | read_metric | PASS | 1118 | 1.63s |
| gpt-5.4-mini | all-metrics | ขอดูทุกค่าของ CW-01 หน่อย | read_metric | read_metric | PASS | 1119 | 2.33s |
| gpt-5.4-mini | detail-analytical-focused | @Speed Trend แนวโน้มเป็นยังไง วิเคราะห์หน่อย | read_agg | read_agg | PASS | 1147 | 2.42s |
| gpt-5.4-mini | change-preview-edit | เปลี่ยน metric เป็น temperature | edit_widget | edit_widget | PASS | 1138 | 3.33s |
| gpt-5.4-mini | add-preview-widget | เพิ่ม widget อุณหภูมิ CW-01 ด้วย | edit_widget | edit_widget | PASS | 1147 | 2.35s |
| gpt-5.4-mini | delete-preview-widget | ลบ widget Trend ออก | edit_widget | edit_widget | PASS | 1136 | 1.96s |
| gpt-5.4-mini | add-to-active-dashboard | เพิ่ม widget speed ของ CW-01 ด้วย | edit_widget | edit_widget | PASS | 1144 | 1.89s |
| gpt-5.4-mini | remove-from-active-dashboard | ลบ widget Speed Gauge ออก | edit_widget | edit_widget | PASS | 1138 | 1.97s |
| gpt-5.4-mini | add-custom-chart | เพิ่มกราฟรวม speed กับ throughput ของ CW-01 | edit_widget|compare | compare | PASS | 1149 | 1.55s |
| gpt-5.4-mini | create | สร้าง dashboard ของ CW-01 ให้หน่อย | create_dashboard | create_dashboard | PASS | 1117 | 2.18s |
| gpt-5.4-mini | typo-create | ส้างแดชบอด cw-01 ให้หน่อย | create_dashboard | create_dashboard | PASS | 1120 | 1.47s |
| gpt-5.4-mini | list-dashboards | มี dashboard อะไรบ้าง | chat | chat | PASS | 1110 | 1.63s |
| gpt-5.4-mini | list-skus | CW-01 มี SKU อะไรบ้าง | chat | chat | PASS | 1117 | 3.26s |
| gpt-5.4-mini | active-alerts | ตอนนี้มีแจ้งเตือนอะไรบ้าง | alerts | alerts | PASS | 1113 | 3.73s |
| gpt-5.4-mini | alert-rule-trap | ตั้ง alert ให้หน่อย ถ้า speed ของ CW-01 เกิน 100 ให้เตือน | alerts | alerts | PASS | 1128 | 1.69s |
| gpt-5.4-mini | trap-action-but-read | ถ้าฉันอยากสร้าง dashboard แล้วตอนนี้มีเครื่องอะไรบ้าง | chat | (declined) | FAIL | 0 | 0.00s |
| gpt-5.4-mini | ambiguous-fix | แก้ให้หน่อย | not-ok (ambiguous, declining is correct) | (declined) | PASS | 0 | 0.00s |
| gpt-5.4-mini | read-no-machine | speed เท่าไหร่ | read_metric | read_metric | PASS | 1112 | 1.54s |
| gpt-5.4-mini | focused-gauge-analytical | แนวโน้มเป็นยังไง วิเคราะห์หน่อย | read_agg | read_agg | PASS | 1137 | 1.50s |
| gpt-5.4-mini | focused-count-now | ตอนนี้เท่าไหร่ | production | read_metric | FAIL | 1136 | 2.16s |
| gpt-5.4-mini | focused-alarm-panel | ตอนนี้เป็นยังไงบ้าง | alerts | alerts | PASS | 1130 | 5.56s |
| gpt-5.4-mini | compound-read-write | เพิ่ม widget อุณหภูมิ CW-01 ด้วย แต่ก่อนอื่นบอกหน่อยตอนนี้ speed เท่าไหร่ | read_metric|edit_widget | chat | FAIL | 1161 | 2.17s |
| gpt-5.4-mini | typo-create-th | ส้างแดชบอด cw-01 | create_dashboard | create_dashboard | PASS | 1117 | 3.54s |
| gpt-5.4-mini | typo-create-en | creat dashbord for cw-01 | create_dashboard | create_dashboard | PASS | 1116 | 4.27s |
| gpt-5.4-mini | synonym-read | how fast is CW-01 running | read_metric | (declined) | FAIL | 0 | 0.00s |
| gpt-5.4-mini | bucket-edit | อยากดู 22 นาที | edit_widget | edit_widget | PASS | 1141 | 1.87s |
| gpt-5.4-mini | relative-date-edit | ดูของเมื่อวาน | edit_widget | edit_widget | PASS | 1140 | 1.61s |
| gpt-5.4-mini | agg-production-read | ผลิตกี่ชิ้นใน 22 นาที | production | production | PASS | 1118 | 5.16s |
| gpt-5.4-mini | compare-metrics | เปรียบเทียบ speed กับ temp | compare | compare | PASS | 1115 | 3.10s |
| gpt-5.4-mini | greeting-short | สวัสดี | chat | chat | PASS | 1107 | 5.01s |
| gpt-5.4-mini | multi-widget-edit | เปลี่ยน Trend กับ Speed Gauge เป็นเมื่อวานทั้งคู่ | edit_widget | (declined) | FAIL | 0 | 0.00s |

**TOTAL: 33 rows · 31591 tokens · 170.0s**
