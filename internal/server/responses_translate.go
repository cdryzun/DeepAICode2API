package server

import (
	"encoding/json"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

// --- Responses → Chat ---

// responsesReasoningEffort 提取 Responses 请求里的推理强度。
//
// 兼容两种写法：官方形状 {"reasoning":{"effort":"low"}}，以及直接给字符串。
func responsesReasoningEffort(payload map[string]any) string {
	switch v := payload["reasoning"].(type) {
	case map[string]any:
		return strings.ToLower(strings.TrimSpace(strOf(v["effort"])))
	case string:
		return strings.ToLower(strings.TrimSpace(v))
	}
	return ""
}

// responsesToChatPayload 把 Responses 请求翻译成 Chat Completions。
func responsesToChatPayload(payload map[string]any, modelID string) (map[string]any, error) {
	out := map[string]any{
		"model":  modelID,
		"stream": boolOf(payload["stream"]),
	}
	if v, ok := payload["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := payload["top_p"]; ok {
		out["top_p"] = v
	}
	if v := intOf(payload["max_output_tokens"]); v > 0 {
		out["max_tokens"] = v
	}
	if v, ok := payload["prompt_cache_key"]; ok {
		out["prompt_cache_key"] = v
	}
	// Responses 的 reasoning.effort 与 chat 的 reasoning_effort 是同一个旋钮。
	// 不映射就是静默把调用方的推理强度丢掉（上游会按默认值跑）。
	if effort := responsesReasoningEffort(payload); effort != "" {
		out["reasoning_effort"] = effort
	}

	messages := make([]any, 0, 4)
	if instructions, ok := payload["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	switch input := payload["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": input})
	case []any:
		for _, itemv := range input {
			item, ok := itemv.(map[string]any)
			if !ok {
				if s, ok := itemv.(string); ok {
					messages = append(messages, map[string]any{"role": "user", "content": s})
				}
				continue
			}
			switch strOf(item["type"]) {
			case "message", "":
				role := strOf(item["role"])
				if role == "" {
					role = "user"
				}
				messages = append(messages, map[string]any{
					"role": role, "content": responsesInputContent(item["content"]),
				})
			case "function_call":
				callID := firstString(item["call_id"], item["id"])
				messages = append(messages, map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{map[string]any{
						"id": callID, "type": "function",
						"function": map[string]any{
							"name":      strOf(item["name"]),
							"arguments": strOf(item["arguments"]),
						},
					}},
				})
			case "function_call_output":
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": firstString(item["call_id"], item["id"]),
					"content":      responsesToolOutput(item["output"]),
				})
			case "reasoning":
				// 历史推理内容不再回喂上游。
			}
		}
	}
	if len(messages) == 0 {
		return nil, errNoMessages
	}
	out["messages"] = messages

	if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, tv := range tools {
			t, ok := tv.(map[string]any)
			if !ok || strOf(t["type"]) != "function" {
				continue
			}
			fn := map[string]any{"name": strOf(t["name"])}
			if d := strOf(t["description"]); d != "" {
				fn["description"] = d
			}
			if p, ok := t["parameters"]; ok {
				fn["parameters"] = p
			}
			converted = append(converted, map[string]any{"type": "function", "function": fn})
		}
		if len(converted) > 0 {
			out["tools"] = converted
			if tc, ok := payload["tool_choice"]; ok {
				out["tool_choice"] = convertResponsesToolChoice(tc)
			}
		}
	}
	return out, nil
}

// responsesInputContent 转换 Responses 的 content 字段。
func responsesInputContent(v any) any {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		parts := make([]any, 0, len(c))
		for _, pv := range c {
			p, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			switch strOf(p["type"]) {
			case "input_text", "output_text", "text":
				if t := strOf(p["text"]); t != "" {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "input_image":
				if u := strOf(p["image_url"]); u != "" {
					parts = append(parts, map[string]any{
						"type": "image_url", "image_url": map[string]any{"url": u},
					})
				}
			case "refusal":
				if t := strOf(p["refusal"]); t != "" {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return parts
	}
	return ""
}

func responsesToolOutput(v any) string {
	switch o := v.(type) {
	case string:
		return o
	case nil:
		return ""
	default:
		raw, err := json.Marshal(o)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

func convertResponsesToolChoice(v any) any {
	switch tc := v.(type) {
	case string:
		return tc
	case map[string]any:
		if strOf(tc["type"]) == "function" && strOf(tc["name"]) != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": strOf(tc["name"])}}
		}
	}
	return nil
}

// --- Chat → Responses ---

// chatToResponsesObject 把 Chat Completions 响应合成为 Responses 对象。
func chatToResponsesObject(chat map[string]any, publicModel string) map[string]any {
	output := make([]any, 0, 3)
	outputText := ""
	reasoningText := ""

	choices, _ := chat["choices"].([]any)
	if len(choices) > 0 {
		if cm, ok := choices[0].(map[string]any); ok {
			if msg, ok := cm["message"].(map[string]any); ok {
				if r := strOf(msg["reasoning"]); r != "" {
					reasoningText = r
					output = append(output, map[string]any{
						"type": "reasoning", "id": "rs_" + deepai.RandomHex(12),
						"summary": []any{map[string]any{"type": "summary_text", "text": r}},
					})
				}
				if t := strOf(msg["content"]); t != "" {
					outputText = t
					output = append(output, map[string]any{
						"type": "message", "id": "msg_" + deepai.RandomHex(12),
						"status": "completed", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": t, "annotations": []any{}}},
					})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tcv := range tcs {
						tc, ok := tcv.(map[string]any)
						if !ok {
							continue
						}
						callID := strOf(tc["id"])
						name, args := "", ""
						if fn, ok := tc["function"].(map[string]any); ok {
							name = strOf(fn["name"])
							args = strOf(fn["arguments"])
						}
						output = append(output, map[string]any{
							"type": "function_call", "id": "fc_" + deepai.RandomHex(12),
							"call_id": callID, "name": name, "arguments": args,
							"status": "completed",
						})
					}
				}
			}
		}
	}

	usage := parseUsage(chat["usage"])
	resp := map[string]any{
		"id":                  "resp_" + deepai.RandomHex(12),
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              "completed",
		"model":               publicModel,
		"output":              output,
		"output_text":         outputText,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"usage": map[string]any{
			"input_tokens":          usage.PromptTokens,
			"output_tokens":         usage.CompletionTokens,
			"total_tokens":          usage.TotalTokens,
			"input_tokens_details":  usage.cacheReadDetails(),
			"output_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens},
		},
	}
	if reasoningText != "" {
		resp["reasoning"] = map[string]any{"summary": reasoningText}
	}
	return resp
}
