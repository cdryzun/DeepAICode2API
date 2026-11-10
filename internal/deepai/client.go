package deepai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// maxJSONBytes 限制上游 JSON 响应体大小（模型目录 / 配额 / 搜索）。
const maxJSONBytes = 8 << 20

// HTTPError 是上游返回的非 2xx 响应。
type HTTPError struct {
	Op     string
	Status int
	Body   []byte
}

// Error 返回可读错误信息（优先使用上游 error.message）。
func (e *HTTPError) Error() string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(e.Body, &payload) == nil && payload.Error.Message != "" {
		return fmt.Sprintf("%s: %s", e.Op, payload.Error.Message)
	}
	body := strings.TrimSpace(string(e.Body))
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	if body == "" {
		return fmt.Sprintf("%s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Op, e.Status, body)
}

// Code 返回上游 error.code。
func (e *HTTPError) Code() string {
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(e.Body, &payload) == nil {
		return payload.Error.Code
	}
	return ""
}

// Retryable 报告该错误是否值得换账号 / 重试。
//
// 400 / 404 / 422 是请求本身的问题，重试无意义；
// 401 / 403 / 409 / 429 / 5xx 与账号或容量相关，可以重试。
// 例外：策略类错误码（social_login_required / machine_required /
// lane_version_unsupported）是账号或客户端终态，换号 / 重试都没用。
func (e *HTTPError) Retryable() bool {
	switch e.Code() {
	case CodeSocialLogin, CodeMachineRequired, CodeLaneUnsupported:
		return false
	}
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict,
		http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return e.Status >= 500
}

// AuthFailed 报告是否为账号鉴权失败。
func (e *HTTPError) AuthFailed() bool {
	if e.Status == http.StatusUnauthorized {
		return true
	}
	code := e.Code()
	return code == CodeAuthRequired || code == CodeInvalidAPIKey || code == CodeEmailUnverified
}

// SocialLoginRequired 报告账号是否被上游要求改用 Google/Apple 登录。
//
// 0.3.19 起上游对“邮箱 + 密码”注册的账号在 /theos 与推理端点返回 403；
// 这是账号级策略拦截，不是鉴权失败，也不是可重试错误。
func (e *HTTPError) SocialLoginRequired() bool { return e.Code() == CodeSocialLogin }

// MachineRequired 报告是否因缺少机器指纹被拒（计量模型专属）。
func (e *HTTPError) MachineRequired() bool { return e.Code() == CodeMachineRequired }

// EmailUnverified 报告上游是否因邮箱未验证拒绝该账号。
//
// 与普通 403 不同：账号的登录态无效，换不换号都一样，
// 应标记后跳过（而不是当成可重试失败反复换号）。
func (e *HTTPError) EmailUnverified() bool { return e.Code() == CodeEmailUnverified }

// LaneUnsupported 报告车道版本不再被上游接受（426）。
func (e *HTTPError) LaneUnsupported() bool {
	return e.Status == http.StatusUpgradeRequired || e.Code() == CodeLaneUnsupported
}

// BodyRejected 报告上游是否因“请求体不是 DeepAI Code 的形状”而拒绝。
//
// 上游对新车道加了请求体校验，典型响应是 400 + code=invalid_request_error，
// message 固定为 "This request is not one DeepAI Code sends."。
func (e *HTTPError) BodyRejected() bool {
	if e.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(string(e.Body)), CodeNotDeepAICodeMsg)
}

// DailyQuotaExceeded 报告这类 429 是不是「这个账号在这条出口上的当日额度用完了」。
//
// 上游对同一账号的每个出口 IP 各有一份按天额度，耗尽后固定回
// `{"type":"rate_limit_exceeded","message":"Too many requests today. Try again tomorrow."}`。
// 它与普通的瞬时 429 语义完全不同：换账号/重试都没用，**换出口**才有用，
// 而且今天之内换回同一条出口也没用。
func (e *HTTPError) DailyQuotaExceeded() bool {
	if e.Code() != "rate_limit_exceeded" {
		return false
	}
	return strings.Contains(strings.ToLower(e.Error()), "too many requests today")
}

// CheckInRefused 报告签到是否被上游按「当天重启次数」限流。
//
// 它与账号不可用不同：推理照常 200，只是会落在替身模型分支
// （见 docs/recon.md §5.1），因此不能把账号标成 error。
func (e *HTTPError) CheckInRefused() bool { return e.Code() == CodeCheckInRefused }

// IsSocialLoginRequired 报告 err 是否为 social_login_required。
func IsSocialLoginRequired(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.SocialLoginRequired()
}

// IsCheckInRefused 报告 err 是否为签到被限流（check_in_refused）。
func IsCheckInRefused(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.CheckInRefused()
}

// CapacityWaiting 报告是否为“会话忙”的容量拒绝。
func (e *HTTPError) CapacityWaiting() bool {
	return e.Status == http.StatusConflict && e.Code() == "session_busy"
}

// RetryAfter 解析 Retry-After 头（秒），默认 fallback。
//
// 与桌面端 waitForDeepAICapacity 同形：可解析的秒数夹到 [1, 30]，
// 头部缺失 / 非数字（含 HTTP-date）时才用调用方的 fallback。
func RetryAfter(resp *http.Response, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return fallback
	}
	var secs int
	if _, err := fmt.Sscanf(raw, "%d", &secs); err != nil {
		return fallback
	}
	if secs < 1 {
		secs = 1
	}
	if secs > 30 {
		secs = 30
	}
	return time.Duration(secs) * time.Second
}

// transientMarkers 是已知的“出口/网络抖动”错误特征。
//
// 有些代理会在隧道建立阶段返回自定义文案（例如 "No exit node"），
// 它既不是 net.Error 也不包装 io.EOF，只能按文案识别。
var transientMarkers = []string{
	"no exit node",
	"unexpected eof",
	"tls handshake timeout",
	"connection reset",
	"connection refused",
	"broken pipe",
	"i/o timeout",
	"timeout awaiting response",
	"temporary failure in name resolution",
}

// IsTransient 报告错误是否属于“再试一次可能成功”的瞬时故障。
//
// 用于 /theos 验活、入池探测这类关键路径的退避重试判断：
// 网络抖动与 5xx/429 值得重试；4xx 是请求或账号本身的问题，重试无意义。
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// 调用方主动取消不算可重试。
	if errors.Is(err, context.Canceled) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
			return true
		}
		return he.Status >= 500
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range transientMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// Client 是 DeepAI 上游客户端。
//
// 一个 Client 可以服务多个账号（按代理分桶复用），签到状态按 API Key 隔离。
type Client struct {
	Origin    string
	HTTP      *http.Client
	Machine   string
	UserAgent string
	Proxy     string

	// lane 是当前使用的 Theos 车道版本（atomic：探测/换道时会被并发读取）。
	// 零值表示用编译期的 LaneVersion。
	lane atomic.Value

	// Instance 是进程级实例号（16 字节随机 hex），随所有 JSON 请求发出。
	Instance string

	// BrowserFingerprint 决定是否把账号的浏览器指纹头（User-Agent /
	// sec-ch-ua / Accept-Language）带到上游请求。
	//
	// 桌面端走 Node fetch，**从不发这些头**；带上去等于在推理接口上自报
	// “我不是客户端”。注册链路需要它们（和网页注册时保持一致），
	// 因此只有 NewClientWithFingerprint 打开它。
	BrowserFingerprint bool

	// checkIns 缓存每个 API Key 的签到挑战与心跳时间。
	checkIns *checkInStore
}

// NewClient 构造上游客户端（默认读取环境代理）。
//
// DEEPAI_API_ORIGIN 可覆盖上游地址（仅接受 http/https）。
func NewClient() *Client { return inferClient(NewClientWithFingerprint("", "")) }

// NewClientWithProxy 构造走指定代理的上游客户端。
func NewClientWithProxy(proxyRaw string) *Client {
	return inferClient(NewClientWithFingerprint(proxyRaw, ""))
}

// inferClient 把客户端切成「推理形态」：不带浏览器指纹。
//
// 注册链路专用 NewClientWithFingerprint（默认带账号的 Chrome 指纹）；
// 其余调用点都是推理 / 探测，必须与桌面端一致。
func inferClient(c *Client) *Client {
	c.BrowserFingerprint = false
	return c
}

// NewClientWithFingerprint 构造带代理与 User-Agent 的上游客户端。
func NewClientWithFingerprint(proxyRaw, userAgent string) *Client {
	origin := strings.TrimRight(EnvOr("DEEPAI_API_ORIGIN", DefaultOrigin), "/")
	if u, err := url.Parse(origin); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		origin = DefaultOrigin
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	proxyRaw = strings.TrimSpace(proxyRaw)
	if proxyRaw != "" {
		if u, err := url.Parse(proxyRaw); err == nil && u.Host != "" {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &Client{
		Origin:             origin,
		Machine:            MachineID(),
		UserAgent:          strings.TrimSpace(userAgent),
		Proxy:              proxyRaw,
		Instance:           newInstanceID(),
		BrowserFingerprint: true,
		checkIns:           newCheckInStore(),
		HTTP: &http.Client{
			Transport: transport,
			// 上游会 302 跳到登录页，跟随只会掩盖鉴权失败。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// ModelURL 返回 /deepai_code 下的完整地址。
func (c *Client) ModelURL(path string) string { return c.Origin + ModelBasePath + path }

// RootURL 返回根路径完整地址。
func (c *Client) RootURL(path string) string { return c.Origin + path }

// SignInURL 返回桌面端同款浏览器登录地址。
func (c *Client) SignInURL(state string, port int) string {
	u := c.ModelURL(PathSignIn)
	q := url.Values{}
	q.Set("state", state)
	q.Set("port", fmt.Sprintf("%d", port))
	return u + "?" + q.Encode()
}

// desktopInferenceHeaders 复刻桌面端 Node fetch（undici）的默认请求头。
//
// 实测 Node 22 的全局 fetch 发出的是：
//
//	user-agent: node / accept: */* / accept-language: * / sec-fetch-mode: cors
//
// 没有浏览器的 UA，也没有 sec-ch-ua。网关此前把注册用的 Chrome 指纹带到
// 推理请求上，与真实客户端不一致；这里按实测值对齐。
var desktopInferenceHeaders = map[string]string{
	"User-Agent":      "node",
	"Accept":          "*/*",
	"Accept-Language": "*",
	"Sec-Fetch-Mode":  "cors",
}

// commonHeaders 写入所有上游 JSON 请求的公共头。
//
// Content-Type 无条件设置（含无 body 的 GET）：桌面端 requestDeepAIJson 的
// header 字面量对 /models、/theos、/get_usage_summary 同样带 application/json。
func (c *Client) commonHeaders(req *http.Request, acct *Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderLane, c.laneVersion())
	if c.Instance != "" {
		req.Header.Set(HeaderInstance, c.Instance)
	}
	// 机器指纹优先取账号级：上游按它发放每日免费额度，共用会互相抵消。
	machine := c.Machine
	if acct != nil && acct.MachineID != "" {
		if fp := MachineFingerprint(acct.MachineID); fp != "" {
			machine = fp
		}
	}
	if machine != "" {
		req.Header.Set(HeaderMachine, machine)
	}

	// 两种指纹形态二选一，不混用：
	//   - BrowserFingerprint=true：注册链路，带账号的 Chrome 指纹；
	//   - 否则：与桌面端推理一致，只带 Node fetch 的默认头。
	//
	// 注意：这里不能 return —— 后面还有 Api-Key（鉴权头）要写。
	if !c.BrowserFingerprint {
		for k, v := range desktopInferenceHeaders {
			req.Header.Set(k, v)
		}
	} else {
		req.Header.Set("Accept", "application/json")
		// 指纹头也优先取账号级：注册与推理必须用同一套特征。
		ua := c.UserAgent
		if acct != nil && acct.UserAgent != "" {
			ua = acct.UserAgent
		}
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		if acct != nil {
			if acct.AcceptLanguage != "" {
				req.Header.Set("Accept-Language", acct.AcceptLanguage)
			}
			if acct.SecCHUA != "" {
				req.Header.Set("sec-ch-ua", acct.SecCHUA)
				req.Header.Set("sec-ch-ua-mobile", "?0")
			}
			if acct.Platform != "" {
				req.Header.Set("sec-ch-ua-platform", acct.Platform)
			}
		}
	}
	if acct != nil && acct.APIKey != "" {
		req.Header.Set(HeaderKeyName, acct.APIKey)
	}
}

// LaneCandidates 是车道探测的候选序列。
//
// 上游每抬一次车道就把旧值一律回 426（0.3.17 是 2，0.3.19 起是 4）。
// 按「新版优先、旧版兜底」试：命中即用，这样上游升级车道时网关不需要改代码。
var LaneCandidates = []string{"4", "5", "6", "7", "8", "9", "10", "11", "12", "3", "2"}

// SetLane 设置当前车道（空串 = 用编译期默认值）。
func (c *Client) SetLane(lane string) {
	c.lane.Store(strings.TrimSpace(lane))
}

// laneVersion 返回本次请求要发的车道版本。
func (c *Client) laneVersion() string {
	if v, ok := c.lane.Load().(string); ok && v != "" {
		return v
	}
	return LaneVersion
}

// Lane 返回当前生效的车道版本。
func (c *Client) Lane() string { return c.laneVersion() }

// DetectLane 用免鉴权的 /models 探测可用的车道版本。
//
// /models 不需要 API Key 与签到，所以启动时探测几乎零成本；一旦命中就把结果
// 写回 Client，后续请求自动带上新车道。全部候选都失败时返回最后一个错误。
func (c *Client) DetectLane(ctx context.Context) (string, error) {
	var lastErr error
	for _, lane := range LaneCandidates {
		ok, err := c.laneWorks(ctx, lane)
		if ok {
			c.SetLane(lane)
			return lane, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("所有候选车道都被上游拒绝（426）")
	}
	return "", lastErr
}

// laneWorks 用候选车道请求一次 /models。
//
// 只有 2xx 才算通过：5xx 说明上游抖动而不是车道不对，继续试下一个候选。
func (c *Client) laneWorks(ctx context.Context, lane string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ModelURL(PathModels), nil)
	if err != nil {
		return false, err
	}
	c.commonHeaders(req, nil)
	req.Header.Set(HeaderLane, lane) // 覆盖成待测值
	resp, err := c.do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode/100 == 2, nil
}

// do 发送请求并返回原样响应（调用方负责关闭 Body）。
//
// 3xx 一律归一成 401：上游对无效 Key 会 302 跳到登录页。跟随会把鉴权失败
// 伪装成 200（所以 CheckRedirect 返回 ErrUseLastResponse），而原样透传在
// API 客户端那边又是个没有 Location 的裸 3xx。统一成 auth_required 后，
// 调用方会标账号失败并换号，客户端拿到的是可识别的 401。
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 3 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, &HTTPError{
			Op:     "上游鉴权",
			Status: http.StatusUnauthorized,
			Body: []byte(fmt.Sprintf(
				`{"error":{"type":%q,"code":%q,"message":"上游把请求重定向到登录页（API Key 无效或已失效，status=%d）"}}`,
				CodeAuthRequired, CodeAuthRequired, resp.StatusCode)),
		}
	}
	return resp, nil
}

// FetchModels 拉取上游模型目录（无需 API Key，但需要机器指纹）。
func (c *Client) FetchModels(ctx context.Context) ([]Model, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ModelURL(PathModels), nil)
	if err != nil {
		return nil, "", err
	}
	c.commonHeaders(req, nil)

	resp, err := c.do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", &HTTPError{Op: "模型目录", Status: resp.StatusCode, Body: body}
	}

	var payload struct {
		Models  []Model `json:"models"`
		Default string  `json:"default"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", fmt.Errorf("模型目录解析失败: %w", err)
	}
	models := make([]Model, 0, len(payload.Models))
	seen := map[string]bool{}
	for _, m := range payload.Models {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if m.Input == nil {
			m.Input = []string{"text"}
		}
		models = append(models, m)
	}
	if len(models) == 0 {
		return nil, "", errors.New("上游模型目录为空")
	}
	def := payload.Default
	if def == "" {
		def = models[0].ID
	}
	return models, def, nil
}

// OpenModel 发起一次推理请求，返回未经读取的响应。
//
// path 取 PathChat 或 PathResponses；affinity 会作为 X-Session-Affinity
// 透传（上游据此做会话粘性调度）。
func (c *Client) OpenModel(ctx context.Context, acct *Account, path string, body []byte, affinity string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ModelURL(path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req, acct)
	// 桌面端在转发推理前会记一次 lastRelayAt，心跳里的 pi 字段由此得来。
	c.markRelay(acct)
	if affinity != "" {
		req.Header.Set(HeaderAffinity, affinity)
	}
	return c.do(req)
}

// Theos 查询账号的免费额度 / 余额。
//
// 0.3.19 起 /theos 同时是签到通道：响应（包括 403 拒绝）带 check_in，
// 下一次请求把 nonce 回传并附带心跳。
func (c *Client) Theos(ctx context.Context, acct *Account) (*Allowance, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ModelURL(PathTheos), nil)
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req, acct)
	c.applyCheckInHeaders(req, acct)
	sentNonce := req.Header.Get(HeaderCheckIn)

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 请求带上了 nonce 就说明这一次是「回传」：签到完成（403 拒绝也算，
	// 桌面端对拒绝同样记一次 noteCheckIn）。
	c.markAcked(acct, sentNonce)
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	// 无论成功还是拒绝，都先吃掉签到挑战：上游在 403 里也会下发。
	c.captureCheckIn(acct, body)
	if resp.StatusCode/100 != 2 {
		httpErr := &HTTPError{Op: "查询 Theos", Status: resp.StatusCode, Body: body}
		// 签到被当日限流：给账号上静默锁，避免 EnsureCheckIn 每 30s 再撞一次。
		if httpErr.CheckInRefused() {
			c.lockCheckIn(acct, CheckInRefusedLock)
		}
		return nil, httpErr
	}
	var out Allowance
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("Theos 响应解析失败: %w", err)
	}
	out.CheckedAt = time.Now().Unix()
	return &out, nil
}

// UsageSummary 查询账号订阅 / 用量。
//
// 与桌面端一致：这个根路径请求**不覆盖** User-Agent（保持 Node fetch 的
// `node`）。`DeepAI-Code` 这个 UA 只属于 /gravity-display-proxy 赞助位渲染
// （见 DesktopUserAgent），带到用量接口上反而又是一个异常信号。
func (c *Client) UsageSummary(ctx context.Context, acct *Account) (*UsageSummary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.RootURL(PathUsageSummary), nil)
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req, acct)

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Op: "查询用量", Status: resp.StatusCode, Body: body}
	}
	var out UsageSummary
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("用量响应解析失败: %w", err)
	}
	return &out, nil
}

// WebSearch 调用上游联网搜索能力。
func (c *Client) WebSearch(ctx context.Context, acct *Account, payload []byte) ([]byte, error) {
	return c.postJSON(ctx, acct, PathWebSearch, payload, "联网搜索")
}

// FindService 调用上游服务推荐能力。
func (c *Client) FindService(ctx context.Context, acct *Account, payload []byte) ([]byte, error) {
	return c.postJSON(ctx, acct, PathFindService, payload, "服务搜索")
}

func (c *Client) postJSON(ctx context.Context, acct *Account, path string, payload []byte, op string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ModelURL(path), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req, acct)

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Op: op, Status: resp.StatusCode, Body: body}
	}
	return body, nil
}

// Transcribe 把音频转发到上游语音转文字接口。
func (c *Client) Transcribe(ctx context.Context, acct *Account, filename, contentType string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("音频内容为空")
	}
	if len(data) > 8<<20 {
		return "", errors.New("音频超过 8 MB")
	}
	if filename == "" {
		filename = "rec.webm"
	}
	if contentType == "" {
		contentType = "audio/webm"
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="voiceRecording"; filename="%s"`, escapeQuotes(filename)))
	h.Set("Content-Type", contentType)
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.RootURL(PathSpeechToText), &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if acct != nil {
		req.Header.Set("api-key", acct.APIKey)
	}

	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", &HTTPError{Op: "语音转写", Status: resp.StatusCode, Body: body}
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("转写响应解析失败: %w", err)
	}
	if strings.TrimSpace(out.Text) == "" {
		return "", errors.New("上游未返回转写文本")
	}
	return strings.TrimSpace(out.Text), nil
}

// ExchangeCode 用桌面端登录回调里的 code 换取 API Key。
func (c *Client) ExchangeCode(ctx context.Context, code string) (*Credentials, error) {
	payload, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ModelURL(PathExchange), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Op: "登录换取密钥", Status: resp.StatusCode, Body: body}
	}
	var out struct {
		APIKey   string `json:"api_key"`
		Username string `json:"username"`
		IsPro    bool   `json:"is_pro"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("登录响应解析失败: %w", err)
	}
	if out.APIKey == "" || strings.ContainsAny(out.APIKey, "\r\n") {
		return nil, errors.New("登录响应缺少有效 API Key")
	}
	return &Credentials{APIKey: out.APIKey, Username: out.Username, IsPro: out.IsPro}, nil
}

func escapeQuotes(s string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(s)
}
