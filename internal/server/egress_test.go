package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/store"
)

// 出口漂移的行为契约：
//   1. 当前出口报「当日额度耗尽」→ 换下一出口并在新出口上立刻重试成功；
//   2. 所有出口都凉 → 账号标 exhausted，调度跳过，不当普通失败重试；
//   3. 冷却到期 → 后台回切到首选（直连），并清理冷却表。

// startConnectProxy 起一个最小 HTTP CONNECT 代理，用来验证「换出口后真的走代理」。
func startConnectProxy(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 明文 http 目标时 Go 走「绝对 URI 转发」，https 才走 CONNECT：两种都要支持。
		if r.Method != http.MethodConnect {
			outReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			outReq.Header = r.Header.Clone()
			resp, err := (&http.Transport{}).RoundTrip(outReq)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			for k, vs := range resp.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}
		dst, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			dst.Close()
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			dst.Close()
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() { _, _ = io.Copy(dst, conn); dst.Close() }()
		_, _ = io.Copy(conn, dst)
		conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

const dailyQuotaBody = `{"error":{"message":"Too many requests today. Try again tomorrow.","type":"rate_limit_exceeded","code":"rate_limit_exceeded"}}`

// newEgressServer 建一个带出口池的网关：账号初始直连。
func newEgressServer(t *testing.T, handlers map[string]http.HandlerFunc) (*Server, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.UpsertAccount(&deepai.Account{
		ID: "acc-1", Name: "acct", APIKey: "key-1", Enabled: true, Status: "ok",
	})
	if err := st.Save(); err != nil {
		t.Fatalf("保存: %v", err)
	}
	key := st.AddKey("test", "").Key
	// 直连与代理客户端都指向同一个假上游：NewClientWithProxy 读 DEEPAI_API_ORIGIN。
	u := newUpstream(t, handlers[""])
	t.Setenv("DEEPAI_API_ORIGIN", u.srv.URL)
	s := New(st)
	s.Up.Origin = u.srv.URL
	return s, key
}

// 直连报当日额度耗尽 → 漂到 proxy-a 并成功。
func TestEgressRotatesOnDailyQuota(t *testing.T) {
	proxy := startConnectProxy(t)
	var directCalls int
	s, key := newEgressServer(t, map[string]http.HandlerFunc{
		"": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/deepai_code/chat/completions" {
				directCalls++
				// 只有直连出口这条路会收到请求：假上游只服务「直连客户端」时
				// 才需要区分，这里靠调用计数即可。
			}
			w.Header().Set("Content-Type", "application/json")
			if directCalls <= 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(dailyQuotaBody))
				return
			}
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"glm-5.3-flash",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
		},
	})
	t.Setenv(EgressPoolEnv, "direct,"+proxy.URL)
	rec := do(t, s, http.MethodPost, "/v1/chat/completions", key,
		`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（应漂到可用出口后成功）: %s", rec.Code, rec.Body.String())
	}
	if directCalls == 0 {
		t.Fatal("直连出口没有被尝试过")
	}
	a := s.Store.Accounts()[0]
	if a.Proxy != proxy.URL {
		t.Fatalf("漂移后出口 = %q，期望 %s", a.Proxy, proxy.URL)
	}
	if a.Failures != 0 {
		t.Fatalf("出口切换不应累计失败次数，得到 %d", a.Failures)
	}
	if _, ok := a.EgressCooldown[egressDirectLabel]; !ok {
		t.Fatal("直连出口没有记录冷却")
	}
}

// 所有出口都凉 → 账号 exhausted、调度跳过、后台回切。
func TestEgressExhaustedThenRestore(t *testing.T) {
	t.Setenv(EgressPoolEnv, "direct")
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.UpsertAccount(&deepai.Account{
		ID: "acc-1", Name: "acct", APIKey: "key-1", Enabled: true, Status: "ok",
		// 直连已凉，且刷新时间在未来。
		Proxy:          "",
		EgressCooldown: map[string]int64{egressDirectLabel: time.Now().Add(2 * time.Hour).Unix()},
		Theos:          &deepai.Allowance{Balance: "1", RefreshAt: time.Now().Add(3 * time.Hour).Format(time.RFC3339)},
	})
	if err := st.Save(); err != nil {
		t.Fatalf("保存: %v", err)
	}
	s := New(st)
	a := s.Store.Accounts()[0]

	// 直连在冷却 → 不应被判成可调度。
	if egressExhausted(a) != true {
		t.Fatal("直连冷却期内应判定为耗尽")
	}
	// 漂移：没有别的出口 → false，调用方会把账号标 exhausted。
	if s.rotateEgressOnExhausted(a, "rate_limit_exceeded") {
		t.Fatal("没有可用出口时不应报告切换成功")
	}
	if a.Proxy != "" {
		t.Fatalf("没有可用出口时不应改出口，得到 %q", a.Proxy)
	}

	// 冷却到期 → RestoreEgress 应清理冷却并回到首选（直连）。
	_ = s.Store.UpdateAccount("acc-1", func(acc *deepai.Account) {
		acc.EgressCooldown[egressDirectLabel] = time.Now().Add(-time.Minute).Unix()
		acc.Status = deepai.StatusExhausted
		acc.Proxy = "http://proxy-a:3128"
	})
	if n := s.RestoreEgress(); n != 1 {
		t.Fatalf("RestoreEgress = %d，期望 1", n)
	}
	a = s.Store.Accounts()[0]
	if a.Proxy != EgressDirect {
		t.Fatalf("回切后出口 = %q，期望直连", a.Proxy)
	}
	if len(a.EgressCooldown) != 0 {
		t.Fatalf("过期冷却应被清理，仍有 %v", a.EgressCooldown)
	}
	if a.Status == deepai.StatusExhausted {
		t.Fatal("回切后应解除 exhausted 状态")
	}
}

// 直连可用时不应无端漂移，也不要给账号记冷却。
func TestEgressKeepsDirectWhenHealthy(t *testing.T) {
	proxy := startConnectProxy(t)
	t.Setenv(EgressPoolEnv, "direct,"+proxy.URL)
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"glm-5.3-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	})
	s, key := newTestServer(t, u)
	if rec := do(t, s, http.MethodPost, "/v1/chat/completions", key,
		`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	a := s.Store.Accounts()[0]
	if a.Proxy != EgressDirect || len(a.EgressCooldown) != 0 {
		t.Fatalf("健康请求不应漂移：proxy=%q cooldown=%v", a.Proxy, a.EgressCooldown)
	}
}

// 出口清单解析：direct 归一化、去重、保留顺序。
func TestEgressCandidatesOrder(t *testing.T) {
	t.Setenv(EgressPoolEnv, "")
	t.Setenv("PROXY_POOL", "http://p1:1, http://p2:2\nhttp://p1:1")
	got := EgressCandidates()
	want := []string{EgressDirect, "http://p1:1", "http://p2:2"}
	if len(got) != len(want) {
		t.Fatalf("候选 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("候选[%d] = %q，期望 %q（%v）", i, got[i], want[i], got)
		}
	}

	t.Setenv(EgressPoolEnv, "DIRECT,http://x:1")
	got = EgressCandidates()
	if len(got) != 2 || got[0] != EgressDirect || got[1] != "http://x:1" {
		t.Fatalf("显式 EGRESS_POOL 解析错误：%v", got)
	}

	// 没有代理池时只有直连。
	os.Unsetenv("PROXY_POOL")
	t.Setenv(EgressPoolEnv, "")
	if got := EgressCandidates(); len(got) != 1 || got[0] != EgressDirect {
		t.Fatalf("无代理池时应只有直连，得到 %v", got)
	}
	if !strings.EqualFold(EgressLabel(""), egressDirectLabel) {
		t.Fatal("直连标签错误")
	}
}
