package ai

// The canonical dataset's NL step: the model returns a query SPEC instead of SQL
// (compiler.go turns it into SQL). Same forced-tool-call shape, same retry/prose/
// clarification contract as emitSQL, so everything downstream — validate, run,
// chart, judge, zoom — is unchanged.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

var emitSpecTool = map[string]any{
	"name":        "emit_query_spec",
	"description": "Return a structured query spec describing which metrics, machine, dimensions and time window answer the question. The server compiles it to SQL and picks every aggregation.",
	"input_schema": map[string]any{
		"type":     "object",
		"required": []string{"answerable", "shape", "metrics"},
		"properties": map[string]any{
			"answerable": map[string]any{"type": "boolean", "description": "false ONLY for a greeting, chit-chat, a question about a previous chart itself, or a metric this factory does not record — then leave metrics empty. A \"which values exist\" question is answerable=true with shape=list."},
			"shape": map[string]any{"type": "string", "enum": []string{"timeseries", "bar", "table", "list"},
				"description": "timeseries for a trend or any 'over time' question (also for correlating two metrics — give both and the chart gets a second axis); bar to rank one number per machine or per label value; table for a plain listing of values; list to enumerate what exists (machines, metrics, label values)."},
			"metrics": map[string]any{
				"type":        "array",
				"description": "One entry per metric to plot. Use the field_key exactly as the catalog spells it. Two entries = two numeric columns on one chart.",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"field"},
					"properties": map[string]any{
						"field":        map[string]any{"type": "string", "description": "field_key from the catalog. Never invent one."},
						"machine":      map[string]any{"type": "string", "description": "Machine name from the catalog, e.g. IQF2. Omit for all machines."},
						"machine_type": map[string]any{"type": "string", "description": "Restrict to a machine family (e.g. IQF) instead of one machine."},
						"labels":       map[string]any{"type": "object", "description": "Label filters, e.g. {\"area\":\"OutFeed*\",\"sku\":\"Carb\"}. A trailing * is a prefix match."},
						"agg":          map[string]any{"type": "string", "enum": []string{"avg", "min", "max"}, "description": "Gauge metrics only, when the question asks for the lowest/highest rather than the typical value. Ignored for counters and events — those have exactly one correct aggregation."},
					},
				},
			},
			"breakdown": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "Split the result by these dimensions: a label key from the catalog, or \"machine\". For shape=list it names WHAT to enumerate: \"machine\", \"metric\", or a label key."},
			"window": map[string]any{
				"type":        "object",
				"description": "The time range. Use hours for a relative lookback ending now (last 24h → 24, 7 days → 168, a month → 720, a year → 8760). Use from/to for a NAMED calendar range instead, as ISO dates with to EXCLUSIVE (May 2026 → from 2026-05-01, to 2026-06-01). Omit entirely when no range is named (the server uses 24h) or for shape=list.",
				"properties": map[string]any{
					"hours": map[string]any{"type": "number"},
					"from":  map[string]any{"type": "string"},
					"to":    map[string]any{"type": "string"},
				},
			},
			"top_n":         map[string]any{"type": "number", "description": "For shape=bar/table: keep only the top N rows (default 30)."},
			"clarification": map[string]any{"type": "string", "description": "Set ONLY when the question is about this factory but no metric or dimension is identifiable at all. ONE short question in the user's language offering concrete choices from the catalog. Never set together with metrics."},
		},
	},
}

// specEmission mirrors sqlEmission: either a spec or a clarification, never both.
type specEmission struct {
	Spec          querySpec
	Clarification string
	WindowHours   float64
	From          *time.Time
	To            *time.Time
}

// emitSpec asks the model for a spec. prevSpec (as JSON) lets a follow-up refine
// the previous turn the same way prev.SQL does on the raw-SQL path.
func emitSpec(ctx context.Context, question, schema string, prev *prevTurn, fixup *sqlFixup) (specEmission, error) {
	ctx, cancel := context.WithTimeout(ctx, askCallTimeout)
	defer cancel()

	sp := "You translate a factory-analytics question into ONE query spec by calling emit_query_spec. Never reply in prose.\n\n" +
		"Today is " + time.Now().Format("2006-01-02") + ". Resolve every named or relative date against it.\n\n" +
		"You do NOT write SQL and you do NOT choose how a metric is aggregated — the server does both, from the\n" +
		"metric's kind in the catalog below. Your job is to identify WHAT to look at: which metric(s), which\n" +
		"machine, which labels, which shape, and over what window.\n\n" +
		"Examples:\n" +
		"- \"IQF2 ผลิตได้เท่าไหร่ 7 วันย้อนหลัง\" → shape=timeseries, metrics=[{field:produced_count, machine:IQF2, " +
		"labels:{area:\"OutFeed*\"}}], window={hours:168}\n" +
		"- \"ตู้ IQF2 อุ่นขึ้นแล้วยอดผลิตตกไหม\" → shape=timeseries with BOTH metrics: " +
		"[{field:produced_count, machine:IQF2, labels:{area:\"OutFeed*\"}}, {field:evap_temp, machine:IQF2}]\n" +
		"- \"SKU ไหนผลิตมากที่สุดเดือนที่แล้ว\" → shape=bar, metrics=[{field:produced_count}], breakdown=[\"sku\"], " +
		"window={from:…,to:…}\n" +
		"- \"มี SKU อะไรบ้าง\" → shape=list, breakdown=[\"sku\"], metrics=[]\n" +
		"- \"IQF3 network หลุดกี่ครั้งอาทิตย์นี้\" → shape=timeseries, metrics=[{field:network_drop, machine:IQF3}], " +
		"window={hours:168}\n\n" +
		"CALENDAR RANGE vs LOOKBACK — get this right or the window is wrong:\n" +
		"- A named calendar range (explicit dates, named months/quarters, \"last month\", \"เดือนที่แล้ว\") → set " +
		"window.from AND window.to as ISO dates, to EXCLUSIVE, and omit hours.\n" +
		"- window.hours is ONLY for a lookback ending now (\"last 7 days\", \"ย้อนหลัง 7 วัน\").\n" +
		// Measured: without this list, "มิถุนายน 2026" was answered with a January
		// window — confidently, and with no way for the user to tell from the chart.
		"- Thai months: มกราคม=01 กุมภาพันธ์=02 มีนาคม=03 เมษายน=04 พฤษภาคม=05 มิถุนายน=06 " +
		"กรกฎาคม=07 สิงหาคม=08 กันยายน=09 ตุลาคม=10 พฤศจิกายน=11 ธันวาคม=12 " +
		"(ย่อ: ม.ค. ก.พ. มี.ค. เม.ย. พ.ค. มิ.ย. ก.ค. ส.ค. ก.ย. ต.ค. พ.ย. ธ.ค.). " +
		"A Thai year over 2500 is Buddhist Era — subtract 543 (2569 → 2026).\n\n" +
		"A question asking to EXPLAIN or DEFINE something (\"what does X mean\", \"อธิบาย\", \"คืออะไร\"), or asking " +
		"about the previous chart itself, is answered in prose: set answerable=false and do NOT set clarification.\n" +
		"When a reasonable default exists, use it instead of asking (no range → last 24h). Set clarification ONLY " +
		"when no metric or dimension is identifiable at all.\n\n"

	switch {
	case prev != nil && prev.Spec != "":
		sp += "The user previously asked: \"" + prev.Question + "\"\nwhich produced this spec:\n" + prev.Spec +
			"\nIf the new message refines it (different grouping, window, machine, chart type) rather than starting a " +
			"new topic, adapt that spec. A pure chart-type change means the SAME spec with a different shape. " +
			"A question ABOUT the previous result (how it was computed, what a point means) is prose: answerable=false.\n\n"
	case prev != nil && prev.Clarification != "":
		sp += "The user originally asked: \"" + prev.Question + "\", you asked: \"" + prev.Clarification +
			"\", and this message is their reply. Combine both into ONE spec and do not ask again.\n\n"
	}
	if fixup != nil {
		sp += "Your previous spec:\n" + fixup.SQL + "\nfailed with:\n" + fixup.Err + "\nReturn a corrected spec.\n\n"
	}
	sp += schema

	msgs := []aiMessage{{Role: "system", Content: &sp}, {Role: "user", Content: strPtr(question)}}
	resp, _, err := callAIModel(ctx, aiModel(), msgs, []map[string]any{toAITool(emitSpecTool)}, forceFunc("emit_query_spec"))
	if err != nil {
		// Same stance as emitSQL: a declined forced call means "not a data question".
		if strings.Contains(strings.ToLower(err.Error()), "tool choice") {
			return specEmission{}, errNotDataQuestion
		}
		return specEmission{}, err
	}
	if len(resp.Choices) == 0 || len(resp.Choices[0].Message.ToolCalls) == 0 {
		return specEmission{}, errNotDataQuestion
	}
	return parseSpecEmission(resp.Choices[0].Message.ToolCalls[0].Function.Arguments)
}

// parseSpecEmission is split out so it is unit-testable without the network.
func parseSpecEmission(rawJSON string) (specEmission, error) {
	var spec querySpec
	if err := json.Unmarshal([]byte(rawJSON), &spec); err != nil {
		return specEmission{}, fmt.Errorf("emit_query_spec returned invalid JSON: %w", err)
	}

	out := specEmission{Spec: spec, Clarification: strings.TrimSpace(spec.Clarification)}
	if out.Clarification != "" && len(spec.Metrics) == 0 && spec.Shape != "list" {
		return out, nil
	}
	out.Clarification = ""
	if !spec.Answerable && len(spec.Metrics) == 0 && spec.Shape != "list" {
		return specEmission{}, errNotDataQuestion
	}

	if spec.Shape == "" {
		spec.Shape = "timeseries"
	}
	out.Spec = spec
	out.WindowHours = spec.Window.Hours
	if f, ok := parseEmissionTime(spec.Window.From); ok {
		if t, ok2 := parseEmissionTime(spec.Window.To); ok2 && t.After(f) {
			out.From, out.To = &f, &t
			out.WindowHours = t.Sub(f).Hours()
		}
	}
	return out, nil
}

// specJSON is what gets stored on a board chart and echoed back as the previous
// turn — a spec, unlike SQL text, stays valid when the relations underneath change.
func specJSON(spec querySpec) string {
	b, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return string(b)
}
