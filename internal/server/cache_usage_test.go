package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/store"
)

// 本文件锁住「缓存用量透传」与「会话亲和钉账号」两条行为。
//
// 背景：上游 api.deepai.org 是个多后端路由器，每个请求随机落到 Modal /
// Wafer / CoreWeave / InferenceNet 之一，前缀缓存按后端独立存放。网关能做
// 的两件事是（1）别把同一条会话的上下文摊到多个号上，（2）把上游真实的
// 缓存用量如实交给调用方——否则调用方看到的命中率恒为 0。

func TestParseUsageCachedTokens(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want Usage
	}{
		{
			name: "OpenRouter 风格 chat 用量",
			raw: map[string]any{
				"prompt_tokens":     float64(6028),
				"completion_tokens": float64(8),
				"total_tokens":      float64(6036),
				"prompt_tokens_details": map[string]any{
					"cached_tokens":      float64(5888),
					"cache_write_tokens": float64(0),
					"audio_tokens":       float64(0),
					"video_tokens":       float64(0),
				},
			},
			want: Usage{PromptTokens: 6028, CompletionTokens: 8, TotalTokens: 6036, CachedTokens: 5888},
		},
		{
			name: "Responses 形状",
			raw: map[string]any{
				"input_tokens":         float64(1000),
				"output_tokens":        float64(20),
				"total_tokens":         float64(1020),
				"input_tokens_details": map[string]any{"cached_tokens": float64(900)},
			},
			want: Usage{PromptTokens: 1000, CompletionTokens: 20, TotalTokens: 1020, CachedTokens: 900},
		},
		{
			name: "Anthropic 形状",
			raw: map[string]any{
				"input_tokens":                float64(100),
				"output_tokens":               float64(10),
				"cache_read_input_tokens":     float64(500),
				"cache_creation_input_tokens": float64(7),
			},
			want: Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, CachedTokens: 500, CacheWriteTokens: 7},
		},
		{
			name: "上游没给缓存字段",
			raw:  map[string]any{"prompt_tokens": float64(12), "completion_tokens": float64(3)},
			want: Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseUsage(c.raw); got != c.want {
				t.Fatalf("parseUsage = %+v，期望 %+v", got, c.want)
			}
		})
	}
}

func TestAnthropicUsageExcludesCached(t *testing.T) {
	// Anthropic 的 input_tokens 不含缓存读写，否则客户端会把上下文数两遍。
	u := Usage{PromptTokens: 1000, CachedTokens: 900, CacheWriteTokens: 50}
	got := u.anthropicUsage(20)
	if v := intOf(got["input_tokens"]); v != 50 {
		t.Fatalf("input_tokens = %d，期望 1000-900-50=50", v)
	}
	if v := intOf(got["cache_read_input_tokens"]); v != 900 {
		t.Fatalf("cache_read_input_tokens = %d，期望 900", v)
	}
	if v := intOf(got["cache_creation_input_tokens"]); v != 50 {
		t.Fatalf("cache_creation_input_tokens = %d，期望 50", v)
	}
	if v := intOf(got["output_tokens"]); v != 20 {
		t.Fatalf("output_tokens = %d，期望 20", v)
	}

	// 缓存量超过 prompt_tokens（上游口径不一致时的防御）：不能出现负数。
	over := Usage{PromptTokens: 10, CachedTokens: 100}
	if v := intOf(over.anthropicUsage(1)["input_tokens"]); v != 0 {
		t.Fatalf("input_tokens = %d，期望 0", v)
	}
}

// TestCacheUsageReachesClient 校验三种出口都不再吞掉缓存用量。
func TestCacheUsageReachesClient(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-1","object":"chat.completion","model":"glm-5.3-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,
				"prompt_tokens_details":{"cached_tokens":900,"cache_write_tokens":40}}
		}`))
	})
	s, key := newTestServer(t, u)

	t.Run("anthropic 阻塞", func(t *testing.T) {
		rec := do(t, s, http.MethodPost, "/v1/messages", key,
			`{"model":"glm-5.3-flash","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
		body := decodeJSON[map[string]any](t, rec.Body.Bytes())
		usage, _ := body["usage"].(map[string]any)
		if intOf(usage["cache_read_input_tokens"]) != 900 {
			t.Fatalf("cache_read_input_tokens = %v，期望 900", usage["cache_read_input_tokens"])
		}
		if intOf(usage["cache_creation_input_tokens"]) != 40 {
			t.Fatalf("cache_creation_input_tokens = %v，期望 40", usage["cache_creation_input_tokens"])
		}
		if intOf(usage["input_tokens"]) != 60 {
			t.Fatalf("input_tokens = %v，期望 1000-900-40=60", usage["input_tokens"])
		}
	})

	t.Run("responses 阻塞", func(t *testing.T) {
		rec := do(t, s, http.MethodPost, "/v1/responses", key,
			`{"model":"glm-5.3-flash","input":"hi","max_output_tokens":8}`)
		body := decodeJSON[map[string]any](t, rec.Body.Bytes())
		usage, _ := body["usage"].(map[string]any)
		details, _ := usage["input_tokens_details"].(map[string]any)
		if intOf(details["cached_tokens"]) != 900 {
			t.Fatalf("cached_tokens = %v，期望 900", details["cached_tokens"])
		}
		// Responses 口径里 input_tokens 含缓存，保持上游原值。
		if intOf(usage["input_tokens"]) != 1000 {
			t.Fatalf("input_tokens = %v，期望 1000", usage["input_tokens"])
		}
	})
}

// TestCacheUsageLogged 校验缓存量进了请求日志（否则控制台/统计看不到命中率）。
func TestCacheUsageLogged(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"glm-5.3-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":500,"completion_tokens":5,"total_tokens":505,
				"prompt_tokens_details":{"cached_tokens":480}}}`))
	})
	s, key := newTestServer(t, u)
	do(t, s, http.MethodPost, "/v1/chat/completions", key,
		`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)

	logs := s.Store.QueryLogs(store.QueryFilter{Limit: 5})
	if len(logs) == 0 {
		t.Fatal("没有请求日志")
	}
	if logs[0].TokensCached != 480 {
		t.Fatalf("日志 tokens_cached = %d，期望 480", logs[0].TokensCached)
	}
	if got := s.Store.Stats().TotalTokensCached; got != 480 {
		t.Fatalf("统计 total_tokens_cached = %d，期望 480", got)
	}
}

// TestAffinityPinsAccount 校验同一会话始终回到同一个号。
//
// 上游的前缀缓存按号存放，两条会话各自滚动时各占一个号，才是命中率
// 不被轮询摄薄的前提；不声明亲和键时仍然按轮询分配（容量优先）。
func TestAffinityPinsAccount(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	for _, id := range []string{"acc-1", "acc-2"} {
		st.UpsertAccount(&deepai.Account{ID: id, Name: id, APIKey: "k-" + id, Enabled: true, Status: "ok"})
	}
	if err := st.Save(); err != nil {
		t.Fatalf("保存状态: %v", err)
	}
	key := st.AddKey("test", "").Key

	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"glm-5.3-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`))
	})
	s := New(st)
	s.Up.Origin = u.srv.URL

	// asked 发一次请求，返回它落到的账号 ID；user 决定上下文指纹。
	asked := func(affinity, user string) string {
		t.Helper()
		body := `{"model":"glm-5.3-flash","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"` + user + `"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:34567"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		if affinity != "" {
			req.Header.Set("X-Session-Affinity", affinity)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d，body=%s", rec.Code, rec.Body.String())
		}
		logs := s.Store.QueryLogs(store.QueryFilter{Limit: 1})
		if len(logs) == 0 {
			t.Fatal("没有请求日志")
		}
		return logs[0].Account
	}

	// 同一亲和键：第一条锁定账号，后两条必须复用（不跟随轮询迁移）。
	a1 := asked("conv-1", "hi")
	a2 := asked("conv-1", "hi")
	a3 := asked("conv-1", "hi")
	if a1 != a2 || a2 != a3 {
		t.Fatalf("同一会话落到了不同账号：%s → %s → %s", a1, a2, a3)
	}

	// 另一个会话在两条请求内也保持自洽（不必与 conv-1 同号）。
	b1 := asked("conv-2", "hello")
	b2 := asked("conv-2", "hello")
	if b1 != b2 {
		t.Fatalf("第二个会话也在漂移：%s → %s", b1, b2)
	}

	// 什么亲和标识都不给的客户端：靠上下文指纹钉住同一个号。
	// 这是 magpie / 多数 SDK 的真实形态，不兼容它的话多号池就等于自动打散缓存。
	f1 := asked("", "fingerprinted")
	f2 := asked("", "fingerprinted")
	if f1 != f2 {
		t.Fatalf("相同上下文没有落在同一个账号：%s → %s", f1, f2)
	}

	// 不同对话（首条消息不同）依然轮询：容量优先。
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		seen[asked("", "conv-"+string(rune('a'+i)))] = true
	}
	if len(seen) < 2 {
		t.Fatalf("不同对话应该分到多个账号，实际只用到 %v", seen)
	}
}

// TestConversationFingerprint 校验上下文指纹：同对话稳定、跨对话不撞、
// 且逐轮追加的消息不改变它。
func TestConversationFingerprint(t *testing.T) {
	user := func(s string) map[string]any {
		return map[string]any{"role": "user", "content": s}
	}
	sys := map[string]any{"role": "system", "content": "be brief"}

	// 同一对话末轮（只追加了后文）：指纹不变。
	turn1 := map[string]any{"messages": []any{sys, user("q1")}}
	turn3 := map[string]any{"messages": []any{
		sys, user("q1"),
		map[string]any{"role": "assistant", "content": "a1"},
		user("q2"),
	}}
	if conversationFingerprint(turn1) != conversationFingerprint(turn3) {
		t.Fatal("同一对话逐轮追加后指纹变了")
	}

	// 不同对话：首条用户消息不同 → 指纹不同。
	other := map[string]any{"messages": []any{sys, user("q2")}}
	if conversationFingerprint(turn1) == conversationFingerprint(other) {
		t.Fatal("不同对话撞了同一个指纹")
	}

	// Responses 形状（instructions + 字符串 input）也能算出指纹。
	fp := conversationFingerprint(map[string]any{"instructions": "be brief", "input": "q1"})
	if fp == "" || len(fp) != len("conv-")+12 {
		t.Fatalf("指纹格式不对：%q", fp)
	}

	// 空请求体不产生指纹（据此退回纯轮询）。
	if got := conversationFingerprint(nil); got != "" {
		t.Fatalf("nil payload 指纹应为空，得到 %q", got)
	}
}
