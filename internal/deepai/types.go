// Package deepai 实现 DeepAI Code 桌面端上游服务的逆向复刻。
//
// 上游是 OpenAI 兼容的 HTTPS 接口：
//
//	POST https://api.deepai.org/deepai_code/chat/completions
//	POST https://api.deepai.org/deepai_code/responses        （仅 openai-responses 模型）
//	GET  https://api.deepai.org/deepai_code/models
//	GET  https://api.deepai.org/deepai_code/theos
//	POST https://api.deepai.org/deepai_code/web_search
//	POST https://api.deepai.org/deepai_code/find_service
//	GET  https://api.deepai.org/get_usage_summary
//	POST https://api.deepai.org/speech_to_text
//
// 鉴权使用桌面端登录后写入的 API Key（`Api-Key` 头），并附带
// `X-DeepAI-Code-Theos` 车道版本、`X-DeepAI-Code-Instance` 进程实例、
// `X-DeepAI-Code-Machine` 机器指纹；`/theos` 还要带签到 nonce 与心跳。
package deepai

import "strings"

// 上游协议常量。
const (
	// LaneVersion 是本网关对齐的 Theos 计费车道版本。
	//
	// 0.3.17 → 0.3.19 上游把车道从 2 提到 4：其它值一律 426
	// lane_version_unsupported（"This version of DeepAI Code is no longer supported"）。
	LaneVersion = "4"

	// AppVersion 是上游据以识别客户端版本的应用版本号（心跳 v= 字段）。
	//
	// 0.3.20（2026-11-07 发布）与 0.3.19 的 wire protocol 逐项核对后完全一致：
	// 车道仍是 4、实例仍是每进程一次、机器指纹算法未变、签到形态未变。
	// 只有心跳里的版本号需要跟上，否则 /theos 上自报的是旧客户端。
	AppVersion = "0.3.20"

	// DesktopUserAgent 是桌面端访问赞助位渲染代理（/gravity-display-proxy）时
	// 使用的 User-Agent。
	//
	// 注意：它**只**用在这一个路径上；/get_usage_summary 等其它根路径请求仍发
	// Node fetch 的 `node`（见 deepai.Client.UsageSummary）。
	DesktopUserAgent = "DeepAI-Code"

	// 请求头名称。
	HeaderKeyName  = "Api-Key"             // 上游统一使用 Api-Key（大小写不敏感）
	HeaderLane     = "X-DeepAI-Code-Theos" // 车道版本
	HeaderMachine  = "X-DeepAI-Code-Machine"
	HeaderAffinity = "X-Session-Affinity"
	// HeaderInstance 是进程级随机实例号（16 字节随机数的 hex），桌面端每个
	// 进程生成一次，随所有 JSON 请求发出。
	HeaderInstance = "X-DeepAI-Code-Instance"
	// HeaderCheckIn 是 /theos 响应下发的签到 nonce，仅 /theos 请求回传。
	HeaderCheckIn = "X-DeepAI-Code-Check-In"
	// HeaderBeacon 是 /theos 请求上的客户端心跳。
	//
	// 形态 `v=<版本>;up=<秒>;pi=<0|1>;focus=<0|1>`，其中 focus 只在
	// 焦点状态已知时出现（见 deepaiBeacon）：网关没有窗口焦点，因此省略。
	HeaderBeacon = "X-DeepAI-Code-Beacon"

	// 上游错误码。
	CodeSessionBusy     = "session_busy"
	CodeLaneUnsupported = "lane_version_unsupported"
	CodeSocialLogin     = "social_login_required"
	CodeMachineRequired = "machine_required"
	CodeEmailUnverified = "email_unverified"
	CodeAuthRequired    = "auth_required"
	CodeInvalidAPIKey   = "invalid_api_key"
	CodeInvalidReq      = "invalid_request_error"
	// CodeCheckInRefused 是签到被限流：账号本身没坏，推理仍可用，
	// 但当天不能再签到 —— 会一直落在替身模型分支直到明天。
	CodeCheckInRefused   = "check_in_refused"
	CodeNotDeepAICodeMsg = "not one deepai code sends"

	// StatusBlocked 是账号级“上游策略拦截”状态（目前来源：social_login_required）。
	//
	// 与普通错误不同：它不是网络/额度问题，换号重试也救不回来，
	// 调度器应直接跳过，直到一次成功的 /theos 把它重新置回 ok。
	StatusBlocked = "blocked"
	// StatusExhausted 表示该账号在所有已知出口上的当日额度都用光了。
	// 它会在额度刷新（theos.refresh_at）后自动解除。
	StatusExhausted = "exhausted"

	// 路径前缀。
	ModelBasePath = "/deepai_code"

	// 模型目录路径（相对 ModelBasePath）。
	PathModels       = "/models"
	PathChat         = "/chat/completions"
	PathResponses    = "/responses"
	PathTheos        = "/theos"
	PathWebSearch    = "/web_search"
	PathFindService  = "/find_service"
	PathSessionsList = "/sessions/list"
	PathSessionsGet  = "/sessions/get"
	PathSessionsDel  = "/sessions/delete"
	PathSessionsSave = "/sessions/save" // 0.3.20 新增：云端会话 / 偏好上传
	PathSessionTurn  = "/session_turn"
	PathSignIn       = "/desktop_auth/"
	PathExchange     = "/desktop_auth/exchange/"

	// 根路径（不在 /deepai_code 之下）。
	PathUsageSummary      = "/get_usage_summary"
	PathSpeechToText      = "/speech_to_text"
	PathAttachments       = "/chat_attachments/upload"
	PathAttachmentsGet    = "/chat_attachments/get"
	PathGravityDisplay    = "/gravity-display-proxy"
	PathGravityImpression = "/gravity-imp"

	// DefaultOrigin 是生产环境上游地址。
	DefaultOrigin = "https://api.deepai.org"

	// DefaultModel 是目录不可用时的兜底模型。
	DefaultModel = "glm-5.3-flash"
)

// Model 是上游模型目录中的一个条目。
type Model struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Reasoning       bool              `json:"reasoning"`
	Input           []string          `json:"input"`
	ContextWindow   int               `json:"contextWindow"`
	MaxTokens       int               `json:"maxTokens"`
	Premier         bool              `json:"premier,omitempty"`
	API             string            `json:"api,omitempty"` // 空 = chat/completions；openai-responses = /responses
	TheosPerRequest string            `json:"theosPerRequest,omitempty"`
	TheosPerSecond  map[string]string `json:"theosPerSecond,omitempty"`
}

// IsResponses 报告该模型是否走 OpenAI Responses 端点。
func (m Model) IsResponses() bool { return m.API == "openai-responses" }

// Free 报告该模型是否免费（每次请求 0 Theos）。
func (m Model) Free() bool {
	if m.TheosPerRequest != "" {
		return m.TheosPerRequest == "0" || m.TheosPerRequest == "0.0" || m.TheosPerRequest == "0.00"
	}
	return !m.Premier
}

// SupportsImage 报告模型是否接受图片输入。
func (m Model) SupportsImage() bool {
	for _, in := range m.Input {
		if in == "image" {
			return true
		}
	}
	return false
}

// Credentials 是桌面端保存的 DeepAI 账号凭据。
type Credentials struct {
	APIKey   string `json:"apiKey"`
	Username string `json:"username"`
	IsPro    bool   `json:"isPro"`
}

// CheckIn 是 /theos 响应里的签到挑战（成功与拒绝都会下发）。
//
// 桌面端把它缓存起来，下一次 /theos 带 X-DeepAI-Code-Check-In: <nonce>，
// 并按 seconds 的半个周期重新签到。
type CheckIn struct {
	Nonce   string `json:"nonce"`
	Seconds int    `json:"seconds"`
}

// Valid 报告签到挑战是否可回传（nonce 为 32 位小写 hex，seconds 为正）。
func (c *CheckIn) Valid() bool {
	if c == nil || c.Seconds <= 0 || len(c.Nonce) != 32 {
		return false
	}
	for _, r := range c.Nonce {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Allowance 是 /theos 返回的配额信息。
type Allowance struct {
	Enabled     bool     `json:"enabled"`
	Balance     string   `json:"balance,omitempty"`
	Granted     string   `json:"granted,omitempty"`
	RefreshAt   string   `json:"refresh_at,omitempty"`
	LaneVersion int      `json:"lane_version,omitempty"`
	CheckIn     *CheckIn `json:"check_in,omitempty"`
	CheckedAt   int64    `json:"checked_at,omitempty"`
}

// Clone 返回配额信息的副本（nil 安全）。
func (a *Allowance) Clone() *Allowance {
	if a == nil {
		return nil
	}
	cp := *a
	if a.CheckIn != nil {
		ci := *a.CheckIn
		cp.CheckIn = &ci
	}
	return &cp
}

// UsageSummary 是 /get_usage_summary 的返回（字段按需保留）。
type UsageSummary struct {
	IsPro             bool    `json:"is_pro"`
	Plan              string  `json:"plan"`
	IsAuthenticated   bool    `json:"is_authenticated"`
	CreditsRemaining  float64 `json:"credits_remaining"`
	TotalOverageCost  float64 `json:"total_overage_cost"`
	HasOverages       bool    `json:"has_overages"`
	PeriodDescription string  `json:"period_description"`
}

// Account 是一个 DeepAI 上游账号（= 一个 API Key）。
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Source   string `json:"source"` // desktop / manual / oauth / register
	APIKey   string `json:"api_key"`
	Username string `json:"username"`
	IsPro    bool   `json:"is_pro"`

	// 网页账号信息（注册链路写入；用于补跑与审计）。
	Email         string `json:"email,omitempty"`
	Password      string `json:"password,omitempty"`
	WebSessionID  string `json:"session_id,omitempty"`
	CSRFToken     string `json:"csrf_token,omitempty"`
	UserPK        int64  `json:"user_pk,omitempty"`
	Plan          string `json:"plan,omitempty"`
	EmailVerified bool   `json:"email_verified"`
	RegisteredAt  int64  `json:"registered_at,omitempty"`
	LastVerified  int64  `json:"last_verified,omitempty"`

	// Group 是号池分组（空串等价于 default）。
	Group string `json:"group,omitempty"`

	// Proxy 是该账号推理时使用的上游代理（完整 URL，仅本地保存）。
	//
	// 空串表示直连。上游按 **(账号 × 出口 IP)** 计每日额度，所以这个字段
	// 会在「当前出口当日额度耗尽」时被自动漂移（见 server/egress.go）。
	Proxy       string `json:"proxy,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`

	// EgressCooldown 记录每个出口对该账号的「当日额度耗尽」到期时间
	// （出口可读名 → unix 秒）。键是 direct 或代理 URL。
	EgressCooldown map[string]int64 `json:"egress_cooldown,omitempty"`

	// 浏览器指纹快照（注册时写入）。推理侧带上同一套头，否则“用 Chrome 注册、
	// 用空 UA 推理”本身就是一个可被识别的异常信号。
	UserAgent      string `json:"user_agent,omitempty"`
	AcceptLanguage string `json:"accept_language,omitempty"`
	SecCHUA        string `json:"sec_ch_ua,omitempty"`
	Platform       string `json:"platform,omitempty"`

	// MachineID 是该账号专用的机器指纹原始值（UUID）。
	//
	// 上游按 X-DeepAI-Code-Machine 每天发放一次免费额度；多个账号共用
	// 一个指纹时只有第一个能拿到额度。注册时每号生成一个。
	// 空串表示回退到进程级 MachineID()（历史账号 + 桌面端导入）。
	MachineID string `json:"machine_id,omitempty"`

	Enabled  bool   `json:"enabled"`
	Status   string `json:"status"` // ok / error / unknown
	Note     string `json:"note"`
	LastUsed int64  `json:"last_used"`
	LastErr  string `json:"last_err"`
	Failures int    `json:"failures"`

	// ProbeAfter 是额度探测的下次允许时间（Unix 秒，0 = 无冷却）。
	//
	// 探测失败后按次退避：没有这个字段时，后台额度守护每 5 分钟会把同一批
	// 失败号全部重探一遍（docs/performance.md §14 之外的另一条写放大），
	// 而失败往往是成片的（出口故障 / 上游抖动）。
	ProbeAfter int64 `json:"probe_after,omitempty"`
	// ProbeFailures 是连续探测失败次数（探测成功即清零）。
	ProbeFailures int `json:"probe_failures,omitempty"`

	// Theos 是最近一次 /theos 探测结果（只做展示，不做调度）。
	Theos *Allowance `json:"theos,omitempty"`
}

// Clone 返回账号的深拷贝（含 Theos 指针）。
//
// 号池持有的是唯一可变实例；对外一律交付 Clone，避免调用方在锁外
// 改字段与调度 / 后台探测裸竞争。
func (a *Account) Clone() *Account {
	if a == nil {
		return nil
	}
	cp := *a
	if a.Theos != nil {
		theos := *a.Theos
		cp.Theos = &theos
	}
	if a.EgressCooldown != nil {
		cd := make(map[string]int64, len(a.EgressCooldown))
		for k, v := range a.EgressCooldown {
			cd[k] = v
		}
		cp.EgressCooldown = cd
	}
	return &cp
}

// MergeFrom 把已有账号 ex 的运行态与缺失身份字段并入 a。
//
// 用于「同一个账号被重新导入 / 重新注册」的场景，策略有二：
//   - 运行期状态（Failures / Status / LastErr / LastUsed / Theos）以 ex 为准：
//     一次导入不应该抹掉故障计数与配额缓存；
//   - 身份字段以 a 的非空值为准，空值才回落到 ex：重新导入不应清空
//     机器指纹、代理、指纹快照这类“已赚到”的信息（清掉会断掉
//     上游的额度归属，或让注册/推理特征不一致）。
func (a *Account) MergeFrom(ex *Account) {
	if a == nil || ex == nil {
		return
	}
	a.ID = ex.ID
	a.Failures = ex.Failures
	a.LastUsed = ex.LastUsed
	a.Status = ex.Status
	a.LastErr = ex.LastErr
	a.Theos = ex.Theos
	a.ProbeAfter = ex.ProbeAfter
	a.ProbeFailures = ex.ProbeFailures

	fill := []struct {
		dst *string
		src string
	}{
		{&a.Name, ex.Name},
		{&a.Email, ex.Email},
		{&a.Password, ex.Password},
		{&a.Group, ex.Group},
		{&a.Proxy, ex.Proxy},
		{&a.Fingerprint, ex.Fingerprint},
		{&a.MachineID, ex.MachineID},
		{&a.UserAgent, ex.UserAgent},
		{&a.AcceptLanguage, ex.AcceptLanguage},
		{&a.SecCHUA, ex.SecCHUA},
		{&a.Platform, ex.Platform},
	}
	for _, f := range fill {
		if *f.dst == "" {
			*f.dst = f.src
		}
	}
	if a.RegisteredAt == 0 {
		a.RegisteredAt = ex.RegisteredAt
	}
}

// GroupName 返回规范化后的分组名。
func (a *Account) GroupName() string {
	if a == nil {
		return "default"
	}
	g := strings.TrimSpace(a.Group)
	if g == "" {
		return "default"
	}
	return g
}

// Registration 是网页注册链路产出的账号信息。
type Registration struct {
	Email          string
	Password       string
	Username       string
	APIKey         string
	SessionID      string
	CSRFToken      string
	UserPK         int64
	Plan           string
	EmailVerified  bool
	Group          string
	Proxy          string
	Fingerprint    string
	MachineID      string
	UserAgent      string
	AcceptLanguage string
	SecCHUA        string
	Platform       string
	Source         string
}

// KeyID 返回 API Key 的稳定短标识（用于去重与日志，不泄露明文）。
func (a *Account) KeyID() string { return KeyFingerprint(a.APIKey) }

// SessionID 返回用于展示/排障的会话标识。
func (a *Account) SessionID() string { return ShortHash(a.APIKey) }

// DefaultModelEntry 是目录不可用时的内置兜底条目。
func DefaultModelEntry() Model {
	return Model{
		ID:            DefaultModel,
		Name:          "GLM 5.3 Flash",
		Reasoning:     true,
		Input:         []string{"text", "image"},
		ContextWindow: 1048576,
		MaxTokens:     131072,
	}
}
