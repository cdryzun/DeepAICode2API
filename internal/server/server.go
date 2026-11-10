// Package server 暴露 OpenAI 兼容的 HTTP 接口。
package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/registry"
	"deepaicode2api/internal/store"
)

// Server 是网关 HTTP 服务。
type Server struct {
	Store *store.Store
	Up    *deepai.Client
	// Login 保存进行中的浏览器登录会话（内存态）。
	Login *loginSessions
	// Registers 保存批量注册任务（内存态）。
	Registers *registerJobs

	refreshMu    sync.Mutex
	proxyMu      sync.Mutex
	proxyClients map[string]*deepai.Client

	// inflight 限制单账号同时进行的上游请求数。
	// 调度只保证「这一轮选到谁」，不保证不会把并发全压在少数账号上：
	// 压测时 12 并发可能全落在同一两个号上，放大上游限流后的长尾。
	inflightMu sync.Mutex
	inflight   map[string]int

	// pins 是「会话 → 账号」亲和表：同一会话的上文固定落在同一个号上。
	//
	// 上游的前缀缓存按账号（及其背后的推理后端）存放，每次请求重新轮询会把
	// 同一条对话的缓存摊薄到 N 个号上，命中率大致除以 N。
	pinMu sync.Mutex
	pins  map[string]pinEntry

	// quotaReprobeDelays 是入池后 0 额度的复探退避序列（测试可注入短值）。
	quotaReprobeDelays []time.Duration

	// laneMu / laneCheckedAt 是车道探测的节流：426 会成批出现，
	// 没有冷却就会把一次车道升级变成一轮探测风暴。
	laneMu        sync.Mutex
	laneCheckedAt time.Time

	// admissionWait 是「槽位全满」时的排队上限（测试可注入 0 立即 429）。
	admissionWait time.Duration

	// maxInflight 是单账号同时在途的上游请求上限（0 表示用默认值）。
	maxInflight int

	// quotaWG 跟踪已排期的异步配额复探。
	//
	// 复探 goroutine 在写完内存后还要 Save() 落盘；测试必须等它完全收尾
	// 再结束，否则 t.TempDir() 的清理会和落盘撞上（CI 上表现为
	// "TempDir RemoveAll cleanup: directory not empty"）。
	quotaWG sync.WaitGroup

	// probing 是配额探测的在途去重表（按账号 ID）。
	//
	// 三条路径会同时探同一批账号：手动 checkin、入池后延迟复探、
	// 推理时的按需复探。没有它时同一个号会被连打多次，既浪费额度
	// 也容易触发上游风控。
	probeMu sync.Mutex
	probing map[string]struct{}

	// probeSem 是全局在途探测上限（手动批次 / 后台守护 / 按需复探共用）。
	//
	// 653 个号的额度快照会同时变旧；若按需复探不加限制，一波流量就能
	// 叠出几十上百个并行的 /theos 请求——那是最容易被上游风控的形态。
	probeSem chan struct{}
}

// New 构造服务。
func New(st *store.Store) *Server {
	s := &Server{
		Store: st, Up: deepai.NewClient(), Login: newLoginSessions(),
		Registers: newRegisterJobs(), proxyClients: map[string]*deepai.Client{},
		inflight:           map[string]int{},
		pins:               map[string]pinEntry{},
		probing:            map[string]struct{}{},
		probeSem:           make(chan struct{}, probeWorkers),
		quotaReprobeDelays: quotaReprobeDelays,
		admissionWait:      admissionWait,
		maxInflight:        DefaultMaxInflight,
	}
	// 上次探测到的车道优先（跨重启保留），否则用编译期默认值。
	if lane := strings.TrimSpace(st.Settings().Lane); lane != "" {
		s.Up.SetLane(lane)
	}
	return s
}

// SetAdmission 覆盖并发槽位与排队上限（<=0 表示沿用默认）。
//
// 号池总槽位 = 账号数 × maxInflight。号池缩小时必须相应抬高单号并发，
// 否则一次 agent 扇出就会撞上「所有账号并发已满」。
func (s *Server) SetAdmission(maxInflight int, wait time.Duration) {
	// <=0 视为「回到默认」，而不是「保留上一次的值」：调用方传 0 时
	// 期待的是一份干净的默认配置，不是上一个配置的残留。
	if maxInflight > 0 {
		s.maxInflight = maxInflight
	} else {
		s.maxInflight = DefaultMaxInflight
	}
	if wait >= 0 {
		s.admissionWait = wait
	}
}

// inflightCap 返回当前生效的单账号在途上限。
func (s *Server) inflightCap() int {
	if s.maxInflight > 0 {
		return s.maxInflight
	}
	return DefaultMaxInflight
}

// Handler 返回完整路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 探活
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("pong"))
	})
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)

	// 推理（OpenAI 兼容）
	mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChatCompletions))
	mux.HandleFunc("/v1/responses", s.withAuth(s.handleResponses))

	// Anthropic 兼容
	mux.HandleFunc("/v1/messages", s.withAuth(s.handleAnthropicMessages))
	mux.HandleFunc("/v1/messages/count_tokens", s.withAuth(s.handleAnthropicCountTokens))

	// 音频
	mux.HandleFunc("/v1/audio/transcriptions", s.withAuth(s.handleTranscriptions))
	mux.HandleFunc("/v1/audio/translations", s.withAuth(s.handleTranscriptions))

	// DeepAI 原生能力扩展
	mux.HandleFunc("/v1/deepai/web_search", s.withAuth(s.handleWebSearch))
	mux.HandleFunc("/v1/deepai/find_service", s.withAuth(s.handleFindService))
	mux.HandleFunc("/v1/deepai/theos", s.withAuth(s.handleTheos))
	mux.HandleFunc("/v1/deepai/usage", s.withAuth(s.handleUsage))
	// 兼容简写
	mux.HandleFunc("/v1/web_search", s.withAuth(s.handleWebSearch))
	mux.HandleFunc("/v1/theos", s.withAuth(s.handleTheos))
	mux.HandleFunc("/v1/usage", s.withAuth(s.handleUsage))

	// 控制台 API（统一走管理鉴权 + 同源校验）
	mux.HandleFunc("/admin/api/state", s.withAdminAuth(s.handleAdminState))
	mux.HandleFunc("/admin/api/overview", s.withAdminAuth(s.handleAdminOverview))
	mux.HandleFunc("/admin/api/accounts", s.withAdminAuth(s.handleAdminAccounts))
	mux.HandleFunc("/admin/api/keys", s.withAdminAuth(s.handleAdminKeys))
	mux.HandleFunc("/admin/api/logs", s.withAdminAuth(s.handleAdminLogs))
	mux.HandleFunc("/admin/api/stats", s.withAdminAuth(s.handleAdminStats))
	mux.HandleFunc("/admin/api/settings", s.withAdminAuth(s.handleAdminSettings))
	mux.HandleFunc("/admin/api/checkin", s.withAdminAuth(s.handleAdminCheckin))
	mux.HandleFunc("/admin/api/models", s.withAdminAuth(s.handleAdminModels))
	mux.HandleFunc("/admin/api/login/start", s.withAdminAuth(s.handleLoginStart))
	mux.HandleFunc("/admin/api/login/poll", s.withAdminAuth(s.handleLoginPoll))
	mux.HandleFunc("/admin/api/register", s.withAdminAuth(s.handleAdminRegister))
	// 浏览器登录跳转：session id 是一次性随机数，服务端只跳到固定的上游登录页。
	mux.HandleFunc("/login/open", s.handleLoginOpen)

	mux.HandleFunc("/", s.handleRoot)

	return s.withCORS(mux)
}

// RefreshModels 从上游拉取模型目录并落盘。
func (s *Server) RefreshModels(ctx context.Context) ([]deepai.Model, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	models, def, err := s.Up.FetchModels(ctx)
	if err != nil {
		return nil, err
	}
	s.Store.SetModels(models, def)
	if err := s.Store.Save(); err != nil {
		log.Printf("保存模型目录失败: %v", err)
	}
	return models, nil
}

// --- 基础端点 ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	accts := s.Store.Accounts()
	ok := 0
	fresh, due := 0, 0
	now := time.Now()
	for _, a := range accts {
		// 尚未探测过的账号按“可用”计，避免刚导入时健康度显示为 0。
		if a.Enabled && a.Status != "error" {
			ok++
		}
		// 额度时效：fresh = 15 分钟内探过；due = 后台守护会去重探的。
		if a.Theos != nil && a.Theos.CheckedAt > 0 &&
			now.Sub(time.Unix(a.Theos.CheckedAt, 0)) < quotaStaleAfter {
			fresh++
		}
		if quotaNeedsRefresh(a, now, quotaGuardStaleAfter) {
			due++
		}
	}
	models := s.catalog()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"accounts":      len(accts),
		"ready":         ok,
		"quota_fresh":   fresh,
		"quota_due":     due,
		"models":        len(models),
		"default_model": s.Store.DefaultModel(),
		"strict_model":  s.Store.Settings().StrictModel,
		"machine_id":    shortMachine(s.Store.MachineID()),
		"machine_ready": s.Store.MachineID() != "",
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	catalog := s.catalog()
	models := registry.List(catalog, s.Store.Settings().ModelAliases)
	data := make([]map[string]any, 0, len(models))
	now := time.Now().Unix()
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  now,
			"owned_by": "deepai",
			"deepai": map[string]any{
				"name":              m.Name,
				"reasoning":         m.Reasoning,
				"input":             m.Input,
				"context_window":    m.ContextWindow,
				"max_tokens":        m.MaxTokens,
				"premier":           m.Premier,
				"free":              m.Free(),
				"api":               m.API,
				"theos_per_request": m.TheosPerRequest,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleRoot 返回控制台单页；其余未知路径返回 404。
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeErr(w, http.StatusNotFound, "未找到该路径", "invalid_request_error")
		return
	}
	serveConsole(w, r)
}
