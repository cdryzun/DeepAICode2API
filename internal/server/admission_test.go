package server

import (
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// 槽位全满时先短暂排队，而不是立刻 429（docs/performance.md §14：
// 14 号 / 28 并发时只有 48~70% 成功，根因之一就是秒拒）。
func TestAllBusyQueuesInsteadOfRejecting(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okChatResponse)
	})
	s, key := newTestServer(t, u)
	s.admissionWait = 2 * time.Second

	// 占满唯一账号的全部在途槽位。
	if !s.acquireAccount("acc-1") || !s.acquireAccount("acc-1") {
		t.Fatal("预占槽位失败")
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		s.releaseAccount("acc-1")
	}()

	begin := time.Now()
	rec := do(t, s, http.MethodPost, "/v1/chat/completions", key, chatProbeBody)
	elapsed := time.Since(begin)
	if rec.Code != http.StatusOK {
		t.Fatalf("排到空槽后应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("应在排队，不能“恰好”立刻成功（耗时 %v）", elapsed)
	}
}

// 排队有上限：一直没空槽就回 429（可重试背压），不能把调用方挂死。
func TestAllBusyRejectsAfterAdmissionWait(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okChatResponse)
	})
	s, key := newTestServer(t, u)
	s.admissionWait = 120 * time.Millisecond
	if !s.acquireAccount("acc-1") || !s.acquireAccount("acc-1") {
		t.Fatal("预占槽位失败")
	}

	begin := time.Now()
	rec := do(t, s, http.MethodPost, "/v1/chat/completions", key, chatProbeBody)
	elapsed := time.Since(begin)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("应回 429，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("排队必须有上限，实际 %v", elapsed)
	}
}

// TestSetAdmissionRaisesCapacity 校验槽位上限可配：默认 2 之外，
// 号池缩小时可以抬高单号并发，避免「一次 agent 扇出」直接撞成 429。
func TestSetAdmissionRaisesCapacity(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"glm-5.3-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	s, key := newTestServer(t, u)
	if got := s.inflightCap(); got != DefaultMaxInflight {
		t.Fatalf("默认上限 = %d，期望 %d", got, DefaultMaxInflight)
	}
	s.SetAdmission(6, 15*time.Second)
	if got := s.inflightCap(); got != 6 {
		t.Fatalf("设置后上限 = %d，期望 6", got)
	}

	// 6 个并发在单账号上必须全部 200（默认值 2 的情况下会有 4 个 429）。
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := do(t, s, http.MethodPost, "/v1/chat/completions", key,
				`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("第 %d 个并发请求 status = %d，期望 200", i, c)
		}
	}

	// 上限归零时回落到默认值，不能变成「0 槽位」。
	s.SetAdmission(0, 0)
	if got := s.inflightCap(); got != DefaultMaxInflight {
		t.Fatalf("归零后上限 = %d，期望回落 %d", got, DefaultMaxInflight)
	}
}
