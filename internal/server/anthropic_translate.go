package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"deepaicode2api/internal/deepai"
)

// --- Anthropic → OpenAI ---

// anthropicToChatPayload 把 Anthropic 请求转换为 OpenAI Chat Completions。
func anthropicToChatPayload(req *anthropicRequest, modelID string) (map[string]any, error) {
	payload := map[string]any{
		"model":  modelID,
		"stream": req.Stream,
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		payload["stop"] = req.StopSequences
	}
	if effort := anthropicEffort(req); effort != "" {
		payload["reasoning_effort"] = effort
	}

	messages := make([]any, 0, len(req.Messages)+1)
	if sys := anthropicSystemText(req.System); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}

	for _, m := range req.Messages {
		if err := appendAnthropicMessage(&messages, m); err != nil {
			return nil, err
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("消息内容为空")
	}
	payload["messages"] = messages

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				var schema any
				if json.Unmarshal(t.InputSchema, &schema) == nil {
					fn["parameters"] = schema
				}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		payload["tools"] = tools
		if tc := anthropicToolChoice(req.ToolChoice); tc != nil {
			payload["tool_choice"] = tc
		}
	}
	return payload, nil
}

// anthropicEffort 把 Anthropic 的 thinking / output_config.effort 映射为
// 上游 chat 端点的 reasoning_effort。
//
// 上游只认 OpenAI 形状；此处前 thinking 被解析后直接丢弃，结果就是
// Anthropic 客户端（包括模型指纹类检测）显式要求的推理强度被静默降级成
// 上游默认值——测出来的“模型指纹”自然与客户端不一致。
//
// 优先级：output_config.effort > thinking.type > thinking.budget_tokens。
func anthropicEffort(req *anthropicRequest) string {
	if req == nil {
		return ""
	}
	if req.OutputConfig != nil {
		if e := strings.ToLower(strings.TrimSpace(req.OutputConfig.Effort)); e != "" {
			return e
		}
	}
	if len(req.Thinking) == 0 {
		return ""
	}
	var th struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	if json.Unmarshal(req.Thinking, &th) != nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(th.Type)) {
	case "disabled":
		return "none"
	case "enabled":
		switch {
		case th.BudgetTokens <= 0:
			return "" // 只说了“开”，没给预算：交给上游默认
		case th.BudgetTokens < 4096:
			return "low"
		case th.BudgetTokens < 16384:
			return "medium"
		default:
			return "high"
		}
	}
	return ""
}

// appendAnthropicMessage 把一个 Anthropic 消息展开为一条或多条 OpenAI 消息。
func appendAnthropicMessage(messages *[]any, m anthropicMessage) error {
	role := strings.ToLower(strings.TrimSpace(m.Role))
	if role == "" {
		role = "user"
	}

	// content 为字符串时直接透传。
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		if role == "assistant" {
			*messages = append(*messages, map[string]any{"role": "assistant", "content": text})
		} else {
			*messages = append(*messages, map[string]any{"role": role, "content": text})
		}
		return nil
	}

	var blocks []map[string]any
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return fmt.Errorf("content 字段格式不支持")
	}

	if role == "assistant" {
		var textParts []string
		var toolCalls []any
		for _, b := range blocks {
			switch b["type"] {
			case "text":
				if t, ok := b["text"].(string); ok && t != "" {
					textParts = append(textParts, t)
				}
			case "tool_use":
				id, _ := b["id"].(string)
				name, _ := b["name"].(string)
				args, _ := json.Marshal(b["input"])
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": string(args),
					},
				})
			}
		}
		msg := map[string]any{"role": "assistant"}
		if len(textParts) > 0 {
			msg["content"] = strings.Join(textParts, "\n")
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		if msg["content"] != nil || len(toolCalls) > 0 {
			*messages = append(*messages, msg)
		}
		return nil
	}

	// user：文本 / 图片合并为一条消息，tool_result 单独成为 role=tool 消息。
	var parts []any
	flush := func() {
		if len(parts) == 0 {
			return
		}
		*messages = append(*messages, map[string]any{"role": role, "content": parts})
		parts = nil
	}
	for _, b := range blocks {
		switch b["type"] {
		case "text":
			if t, ok := b["text"].(string); ok && t != "" {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image":
			if u := anthropicImageURL(b); u != "" {
				parts = append(parts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": u},
				})
			}
		case "document":
			if u := anthropicDocumentDataURL(b); u != "" {
				parts = append(parts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": u},
				})
			} else {
				name := firstString(b["title"], b["name"])
				if name == "" {
					name = "document"
				}
				parts = append(parts, map[string]any{"type": "text", "text": "[document] " + name})
			}
		case "tool_result":
			flush()
			id, _ := b["tool_use_id"].(string)
			content := anthropicToolResultText(b)
			if content == "" {
				content = "(no output)"
			}
			*messages = append(*messages, map[string]any{
				"role": "tool", "tool_call_id": id, "content": content,
			})
		}
	}
	flush()
	return nil
}

// anthropicSystemText 提取 system 字段（字符串或文本块数组）。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b["type"] == "text" {
			if t, ok := b["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func anthropicImageURL(block map[string]any) string {
	src, ok := block["source"].(map[string]any)
	if !ok {
		return ""
	}
	switch src["type"] {
	case "base64":
		media, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if data == "" {
			return ""
		}
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + data
	case "url":
		u, _ := src["url"].(string)
		return u
	}
	return ""
}

func anthropicDocumentDataURL(block map[string]any) string {
	src, ok := block["source"].(map[string]any)
	if !ok || src["type"] != "base64" {
		return ""
	}
	media, _ := src["media_type"].(string)
	if !strings.HasPrefix(media, "image/") {
		return ""
	}
	data, _ := src["data"].(string)
	if data == "" {
		return ""
	}
	return "data:" + media + ";base64," + data
}

func anthropicToolResultText(block map[string]any) string {
	switch c := block["content"].(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			case "image":
				parts = append(parts, "[image omitted]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func anthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var body struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	switch body.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if body.Name == "" {
			return nil
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": body.Name}}
	}
	return nil
}

// --- OpenAI → Anthropic ---

// chatToAnthropicMessage 把 OpenAI chat 响应转换为 Anthropic message。
func chatToAnthropicMessage(out map[string]any, publicModel string) map[string]any {
	content := make([]any, 0, 3)
	stopReason := "end_turn"

	choices, _ := out["choices"].([]any)
	if len(choices) > 0 {
		if cm, ok := choices[0].(map[string]any); ok {
			if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
				stopReason = anthropicStopReason(fr)
			}
			if msg, ok := cm["message"].(map[string]any); ok {
				if reasoning, ok := msg["reasoning"].(string); ok && reasoning != "" {
					content = append(content, map[string]any{
						"type": "thinking", "thinking": reasoning, "signature": "",
					})
				}
				if text, ok := msg["content"].(string); ok && text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tcv := range tcs {
						tc, ok := tcv.(map[string]any)
						if !ok {
							continue
						}
						id, _ := tc["id"].(string)
						name := ""
						input := map[string]any{}
						if fn, ok := tc["function"].(map[string]any); ok {
							name, _ = fn["name"].(string)
							if args, ok := fn["arguments"].(string); ok && args != "" {
								var parsed map[string]any
								if json.Unmarshal([]byte(args), &parsed) == nil {
									input = parsed
								} else {
									input = map[string]any{"_raw": args}
								}
							}
						}
						content = append(content, map[string]any{
							"type": "tool_use", "id": id, "name": name, "input": input,
						})
					}
				}
			}
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	usage := parseUsage(out["usage"])
	return map[string]any{
		"id":            "msg_" + deepai.RandomHex(12),
		"type":          "message",
		"role":          "assistant",
		"model":         publicModel,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage.anthropicUsage(usage.CompletionTokens),
	}
}

// anthropicStopReason 映射 OpenAI finish_reason → Anthropic stop_reason。
func anthropicStopReason(fr string) string {
	switch fr {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func anthropicTextLen(msg map[string]any) int {
	total := 0
	content, _ := msg["content"].([]any)
	for _, c := range content {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		switch cm["type"] {
		case "text":
			total += lenOfString(cm["text"])
		case "thinking":
			total += lenOfString(cm["thinking"])
		}
	}
	return total
}
