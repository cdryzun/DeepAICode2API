package server

import (
	"encoding/json"
	"net/http"
	"time"

	"deepaicode2api/internal/deepai"
)

// --- Anthropic 请求模型 ---

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	TopK          *int               `json:"top_k"`
	StopSequences []string           `json:"stop_sequences"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
	Thinking      json.RawMessage    `json:"thinking"`
	// OutputConfig 是 Anthropic 自适应思考的强度旋钮（{"effort":"low"}）。
	OutputConfig *struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
	Metadata json.RawMessage `json:"metadata"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// handleAnthropicMessages 是 POST /v1/messages 的入口。
//
// 把 Anthropic Messages 请求转换为 OpenAI Chat Completions，调用上游后再
// 映射回 Anthropic 的 message / SSE 事件模型（含 tool_use、thinking）。
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	start := time.Now()

	raw, err := readBodyWithLimit(w, r)
	if err != nil {
		writeAnthropicErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "请求体过大")
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}

	model, modelOK := s.resolveModelOK(req.Model)
	if !modelOK {
		writeAnthropicErr(w, http.StatusNotFound, "invalid_request_error", s.unknownModelMessage(req.Model))
		return
	}
	payload, err := anthropicToChatPayload(&req, model.ID)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	affinity := affinityFor(r, payload)
	key := extractKey(r)
	promptLen := estimatePromptLen(payload)

	rl := s.newReqLog(key, nil, model.ID, "messages", req.Stream, start, promptLen)
	if req.Stream {
		s.streamAnthropic(w, r, payload, affinity, rl)
		return
	}
	s.blockingAnthropic(w, r, payload, affinity, rl)
}

// blockingAnthropic 收集完整回复后按 Anthropic message 结构返回。
func (s *Server) blockingAnthropic(w http.ResponseWriter, r *http.Request, payload map[string]any, affinity string, rl reqLog) {
	ctx, cancel := contextWithTimeout(r, s.requestTimeout())
	defer cancel()

	out, acct, err := s.callJSON(ctx, deepai.PathChat, payload, affinity, s.requestGroup(r))
	rl.acct = acct
	if err != nil {
		rl.done(0, Usage{}, err)
		writeAnthropicUpstreamErr(w, err)
		return
	}
	usage := parseUsage(out["usage"])
	msg := chatToAnthropicMessage(out, rl.model)
	outLen := anthropicTextLen(msg)

	rl.done(outLen, usage, nil)
	writeJSON(w, http.StatusOK, msg)
}

// streamAnthropic 以 Anthropic SSE 事件模型流式返回。
func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, payload map[string]any, affinity string, rl reqLog) {
	ctx, cancel := contextWithTimeout(r, s.requestTimeout())
	defer cancel()

	body, err := json.Marshal(payload)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "请求序列化失败")
		return
	}
	resp, acct, err := s.openModel(ctx, deepai.PathChat, body, affinity, s.requestGroup(r))
	rl.acct = acct
	if err != nil {
		rl.done(0, Usage{}, err)
		writeAnthropicUpstreamErr(w, err)
		return
	}
	defer resp.Body.Close()

	sw := newSSEWriter(w)

	msgID := "msg_" + deepai.RandomHex(12)
	send := func(event string, v any) {
		if raw := jsonEvent(v); raw != nil {
			sw.event(event, raw)
		}
	}

	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": rl.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			// 流式开始前还不知道真实用量（它在尾帧），但缓存字段必须存在：
			// Claude Code 这类客户端会在首帧就把 cache_read 记入上下文预算，
			// 缺字段时它会把整段 prompt 当成未命中、重复计数。
			"usage": Usage{}.anthropicUsage(0),
		},
	})

	block := newAnthropicBlockWriter(send)
	var (
		usage      Usage
		outLen     int
		stopReason = "end_turn"
		streamErr  error
	)

	streamErr = forEachChatChunk(resp.Body, func(chunk map[string]any) bool {
		if u := parseUsage(chunk["usage"]); u.TotalTokens > 0 || u.Cost > 0 {
			usage = u
		}
		d, finish := chatChunkDelta(chunk)
		if finish != "" {
			stopReason = anthropicStopReason(finish)
		}
		if d.Reasoning != "" {
			block.thinking(d.Reasoning)
			outLen += len([]rune(d.Reasoning))
		}
		if d.Content != "" {
			block.text(d.Content)
			outLen += len([]rune(d.Content))
		}
		for _, tc := range d.ToolCalls {
			block.toolCall(tc.Index, tc.ID, tc.Name, tc.Args)
		}
		return true
	})

	block.close()
	if streamErr != nil {
		send("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": streamErr.Error()},
		})
	}
	outputTokens := usage.CompletionTokens
	if outputTokens == 0 {
		outputTokens = estTokens(outLen)
	}
	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usage.anthropicUsage(outputTokens),
	})
	send("message_stop", map[string]any{"type": "message_stop"})

	rl.done(outLen, usage, streamErr)
}

// handleAnthropicCountTokens 返回粗略的输入 token 估算。
//
// Anthropic 客户端（如 Claude Code）会在发送消息前调用它做预算控制；
// 上游不提供该能力，这里用字符数近似，只需单调合理即可。
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	raw, err := readBodyWithLimit(w, r)
	if err != nil {
		writeAnthropicErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "请求体过大")
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败")
		return
	}
	payload, err := anthropicToChatPayload(&req, s.resolveModel(req.Model).ID)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	rawPayload, _ := json.Marshal(payload)
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": estTokens(len(rawPayload)),
	})
}

// --- 工具函数 ---

// estTokens 用字符数粗略估算 token（1 token ≈ 2 字符）。
func estTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return chars/2 + 1
}

func firstString(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// writeAnthropicErr 按 Anthropic 的错误结构返回。
func writeAnthropicErr(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// requestTimeout 返回设置里的请求超时。
func (s *Server) requestTimeout() time.Duration {
	sec := s.Store.Settings().RequestTimeout
	if sec <= 0 {
		sec = 600
	}
	return time.Duration(sec) * time.Second
}
