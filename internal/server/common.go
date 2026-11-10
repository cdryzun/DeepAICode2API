package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/registry"
	"deepaicode2api/internal/store"
)

// maxRequestBody 限制入站请求体大小。
const maxRequestBody = 32 << 20

// Usage 是归一化后的 token / 成本用量。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ReasoningTokens  int
	// CachedTokens 是命中上游前缀缓存的输入 token（属于 PromptTokens 的子集）。
	//
	// 这条信息以前在网关里被丢掉：只有 chat 路径因为「上游 JSON 原样透传」
	// 碰巧正确，responses 被写死成 0、anthropic 根本没有字段。于是调用方
	// 看到的缓存命中率恒为 0，反过来误以为是网关把缓存打散了。
	CachedTokens int
	// CacheWriteTokens 是本次写入上游缓存的输入 token（按写入价计费的量）。
	CacheWriteTokens int
	Cost             float64
}

// Add 累加用量。
func (u *Usage) Add(v Usage) {
	u.PromptTokens += v.PromptTokens
	u.CompletionTokens += v.CompletionTokens
	u.TotalTokens += v.TotalTokens
	u.ReasoningTokens += v.ReasoningTokens
	u.CachedTokens += v.CachedTokens
	u.CacheWriteTokens += v.CacheWriteTokens
	u.Cost += v.Cost
}

// cacheReadDetails 返回 OpenAI 口径的输入明细：cached_tokens 是 input_tokens 的子集。
func (u Usage) cacheReadDetails() map[string]any {
	return map[string]any{"cached_tokens": u.CachedTokens}
}

// anthropicUsage 按 Anthropic 口径组装用量。
//
// Anthropic 的 input_tokens **不含**缓存读写：客户端把
// input + cache_read + cache_creation 当上下文总量。直接把上游的
// prompt_tokens（含缓存）填进 input_tokens 会让 Claude Code 这类客户端
// 把同一批 token 数两遍，上下文预算与费用估算都会翻倍。
func (u Usage) anthropicUsage(outputTokens int) map[string]any {
	input := u.PromptTokens - u.CachedTokens - u.CacheWriteTokens
	if input < 0 {
		input = 0
	}
	return map[string]any{
		"input_tokens":                input,
		"output_tokens":               outputTokens,
		"cache_read_input_tokens":     u.CachedTokens,
		"cache_creation_input_tokens": u.CacheWriteTokens,
	}
}

// affinityPattern 与桌面端一致：只透传安全字符集的会话标识。
var affinityPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)

// affinityFor 解析会话粘性标识。
//
// 优先级：显式头 > prompt_cache_key > 上下文指纹。前两个由调用方声明，
// 第三个是兜底：多数客户端（magpie、各种 SDK 封装）两个都不送，若就此放弃
// 亲和，多号池就会把同一条对话的前缀缓存摊薄到 N 个号上。
func affinityFor(r *http.Request, payload map[string]any) string {
	if r != nil {
		if v := strings.TrimSpace(r.Header.Get("X-Session-Affinity")); affinityPattern.MatchString(v) {
			return v
		}
	}
	if payload != nil {
		if v, ok := payload["prompt_cache_key"].(string); ok && affinityPattern.MatchString(strings.TrimSpace(v)) {
			return strings.TrimSpace(v)
		}
	}
	return conversationFingerprint(payload)
}

// conversationFingerprint 用请求的「前导内容」算一个稳定的会话指纹。
//
// 取 system/developer 指令 + 第一条对话消息：同一条对话逐轮追加的后文不影响它，
// 不同对话几乎不会撞。只用 sha256 前 12 位十六进制（48 bit），既短又够用。
func conversationFingerprint(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	var b strings.Builder

	// system / instructions / developer
	b.WriteString(strOf(payload["instructions"]))
	switch sys := payload["system"].(type) {
	case string:
		b.WriteString(sys)
	case []any:
		for _, block := range sys {
			if m, ok := block.(map[string]any); ok {
				b.WriteString(strOf(m["text"]))
			}
		}
	}

	first := ""
	appendMsg := func(v any) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		role := strings.ToLower(strOf(m["role"]))
		if role == "system" || role == "developer" {
			b.WriteString(strOf(m["content"]))
			return
		}
		if first == "" {
			if raw, err := json.Marshal(m); err == nil {
				first = string(raw)
			}
		}
	}
	switch msgs := payload["messages"].(type) {
	case []any:
		for _, m := range msgs {
			if first != "" {
				break
			}
			appendMsg(m)
		}
	case nil:
		// Responses 形状：input 可能是字符串或条目数组。
		switch in := payload["input"].(type) {
		case string:
			first = in
		case []any:
			for _, m := range in {
				if first != "" {
					break
				}
				appendMsg(m)
			}
		}
	}
	b.WriteString(first)

	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(b.String())))
	return "conv-" + sum[:12]
}

// catalog 返回当前模型目录；目录为空时回落到内置兜底模型，
// 便于首次启动（尚未拉到上游目录）时仍能应答。
func (s *Server) catalog() []deepai.Model {
	catalog := s.Store.Models()
	if len(catalog) == 0 {
		return []deepai.Model{deepai.DefaultModelEntry()}
	}
	return catalog
}

// resolveModel 用当前目录 + 别名解析请求模型（宽松：解析不到时回落，永不失败）。
func (s *Server) resolveModel(name string) deepai.Model {
	m, _ := s.resolveModelOK(name)
	return m
}

// resolveModelOK 解析模型名，并报告它是否“有依据”地命中目录。
//
// 顺序：精确 ID → 自定义别名 → 内置别名 → 模型名称 → （非严格模式）子串
// → 默认模型 / 目录首项。ok=false 只会在严格模式下出现。
//
// 严格模式（Settings.StrictModel）的存在意义：宽松回落会把拼写错误
// 静默换成一个完全不同的模型（例如 gpt-6-sol → glm-5.3-flash），
// 调用方会以为自己测的是 A，实际跑的是 B。
func (s *Server) resolveModelOK(name string) (deepai.Model, bool) {
	catalog := s.catalog()
	settings := s.Store.Settings()
	def := s.defaultModelID()
	if m, ok := registry.ResolveStrict(catalog, settings.ModelAliases, name, def); ok {
		return m, true
	}
	if settings.StrictModel {
		return deepai.Model{}, false
	}
	return registry.Resolve(catalog, settings.ModelAliases, name, def), true
}

// defaultModelID 返回回落目标：设置里的兜底模型优先，否则目录默认。
func (s *Server) defaultModelID() string {
	catalog := s.catalog()
	settings := s.Store.Settings()
	def := s.Store.DefaultModel()
	if settings.FallbackModel == "" {
		return def
	}
	if m, ok := registry.Find(catalog, settings.FallbackModel); ok {
		return m.ID
	}
	return settings.FallbackModel
}

// unknownModelMessage 生成严格模式下的可执行错误信息。
//
// 只靠一句“模型不存在”会让调用方无从下手；带上近似候选（gpt-6-sol 附近
// 就是 gpt-6.1-sol）才能一眼看出是拼写还是渠道不支持。
func (s *Server) unknownModelMessage(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "(空)"
	}
	msg := fmt.Sprintf("模型 %q 不在上游目录中（严格模式已开启，不做静默回落）", name)
	if picks := registry.Suggest(s.catalog(), name, 6); len(picks) > 0 {
		msg += "；相近的模型：" + strings.Join(picks, "、")
	}
	msg += "。可用模型见 GET /v1/models；也可在控制台「模型别名」里把该名称映射到目录模型。"
	return msg
}

// admissionWait / admissionPoll 是「池里暂时没有空槽」时的排队上限与轮询间隔。
//
// 以前槽位不够就立刻 429（见 docs/performance.md §14：14 号 / 28 并发时
// 成功率只有 48~70%）。短暂排队能把突发流量摊平，又不至于把调用方挂死。
const (
	admissionWait = 2 * time.Second
	admissionPoll = 50 * time.Millisecond
)

// 会话亲和（affinity → 账号）的参数。
const (
	// pinTTL 是一条会话保持在同一账号上的时长。
	//
	// 比上游的前缀缓存寿命长：上游缓存按分钟级 TTL 滑窗，只要会话还在
	// 滚动，落回原账号就能继续命中；太长又会把新会话锁在旧号上。
	pinTTL = 30 * time.Minute
	// pinMax 是亲和表上限：不设上限时长时间运行会被短命会话撑大内存。
	pinMax = 4096
)

// pinEntry 是一条会话绑定的账号及其过期时间。
type pinEntry struct {
	acctID string
	exp    time.Time
}

// pinnedAccount 返回该会话当前绑定的账号 ID；过期/未绑定返回空串。
func (s *Server) pinnedAccount(affinity string) string {
	if affinity == "" {
		return ""
	}
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	e, ok := s.pins[affinity]
	if !ok {
		return ""
	}
	if time.Now().After(e.exp) {
		delete(s.pins, affinity)
		return ""
	}
	return e.acctID
}

// pinAccount 把会话绑到刚应答的账号上（affinity 为空时什么也不做）。
func (s *Server) pinAccount(affinity, acctID string) {
	if affinity == "" || acctID == "" {
		return
	}
	now := time.Now()
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	if len(s.pins) >= pinMax {
		// 先清过期项；仍满就整表清空。亲和只是最优化提示，丢掉不影响正确性。
		for k, v := range s.pins {
			if now.After(v.exp) {
				delete(s.pins, k)
			}
		}
		if len(s.pins) >= pinMax {
			s.pins = map[string]pinEntry{}
		}
	}
	s.pins[affinity] = pinEntry{acctID: acctID, exp: now.Add(pinTTL)}
}

// unpinAccount 解除会话绑定：换号成功前必须先松绑，否则下一次请求
// 会被又钉回那个刚出错的号。
func (s *Server) unpinAccount(affinity string) {
	if affinity == "" {
		return
	}
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	delete(s.pins, affinity)
}

// dispatchable 判断账号是否属于可调度集合（与 pool.Scheduler 的准入一致）。
func dispatchable(a *deepai.Account, group string) bool {
	if a == nil || !a.Enabled || a.Status == deepai.StatusBlocked {
		return false
	}
	return group == "" || strings.EqualFold(a.GroupName(), group)
}

// maxInFlightPerAccount 是单账号同时进行的上游请求上限。
//
// 调度只决定「这一轮选到谁」，不保证并发不会集中到少数账号；
// 实测 12 并发时可能全落在 1~2 个号上，一碰上上游限流就是长尾。
const maxInFlightPerAccount = 2

// nonStreamBudget 是非流式请求的整体预算（选号 + 重试 + 读体）。
//
// 没有它时，「MaxRetries × Retry-After≤30s」理论上可挂 90s 以上。
const nonStreamBudget = 90 * time.Second

// releaseBody 在响应体关闭时归还账号的在途配额。
//
// 所有上游调用点都以 defer resp.Body.Close() 收尾，所以槽位会随 Body 释放：
// 流式响应可能持续数分钟，不能在建连成功时就归还。
type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseBody) Close() error {
	b.once.Do(b.release)
	return b.ReadCloser.Close()
}

// pickFreeAccount 扫一圈号池，找一个还有在途槽位的账号。
//
// 不能用 max_retries 次贪心选号：饱和时几次尝试大概率全撞在忙号上，
// 于是对外 429 而池里其实还有空槽（docs/performance.md §14）。
// 返回值第三个标记池里是否有可选账号：区分「全忙」与「没有可调度账号」，
// 后者要交给 noAccountError 给出可行动的原因（social_login / 未验证…）。
func (s *Server) pickFreeAccount(group, affinity string) (*deepai.Account, bool, bool) {
	// 会话亲和优先：同一会话回到上一次应答的账号，缓存才不会被轮询摊薄。
	//
	// 只在「钉住的号还在、还能调度、还有空槽」时走这条路；号满了就退回
	// 常规选择而不是排队——亲和是最优化，不该把请求拖慢。
	if id := s.pinnedAccount(affinity); id != "" {
		for _, a := range s.Store.Accounts() {
			if a.ID != id {
				continue
			}
			if dispatchable(a, group) && s.acquireAccount(a.ID) {
				return a, true, true
			}
			break
		}
	}

	candidates := false
	for i, n := 0, len(s.Store.Accounts()); i < n; i++ {
		a := s.Store.NextAccountGroup(group)
		if a == nil {
			return nil, false, false
		}
		candidates = true
		if s.acquireAccount(a.ID) {
			return a, true, true
		}
	}
	return nil, false, candidates
}

// acquireAccount 尝试占用一个账号的在途槽位；已满时返回 false。
func (s *Server) acquireAccount(id string) bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflight == nil {
		s.inflight = map[string]int{}
	}
	if s.inflight[id] >= maxInFlightPerAccount {
		return false
	}
	s.inflight[id]++
	return true
}

// releaseAccount 归还一个在途槽位。
func (s *Server) releaseAccount(id string) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflight[id] <= 0 {
		delete(s.inflight, id)
		return
	}
	s.inflight[id]--
	if s.inflight[id] == 0 {
		delete(s.inflight, id)
	}
}

// hasFreeAccount 报告池里是否还有账号有空在途槽位。
//
// 409 之后用它区分「立即换号」与「整池都满，等一等」：以前一律先
// sleep(Retry-After) 再换号，最坏白等 30s，还把非流式预算（90s）吃掉。
func (s *Server) hasFreeAccount() bool {
	for _, a := range s.Store.Accounts() {
		if !a.Enabled || a.Status == deepai.StatusBlocked {
			continue
		}
		s.inflightMu.Lock()
		busy := s.inflight[a.ID] >= maxInFlightPerAccount
		s.inflightMu.Unlock()
		if !busy {
			return true
		}
	}
	return false
}

// errAllAccountsBusy 表示所有候选账号都达到了并发上限。
//
// 用 429 而不是 502：这是可重试的背压，客户端应稍后重试。
func errAllAccountsBusy() error {
	return &deepai.HTTPError{
		Op:     "账号调度",
		Status: http.StatusTooManyRequests,
		Body:   []byte(`{"error":{"message":"所有账号并发已满，请稍后重试","type":"rate_limit_error"}}`),
	}
}

// openModel 在账号池上打开一次上游推理请求。
//
// 语义：
//   - 2xx：立即返回响应，调用方负责关闭 Body；
//   - 400/404/422 等请求错误：不重试，直接返回 *deepai.HTTPError；
//   - 401/403/429/5xx：记录账号失败并换号重试；
//   - 409 session_busy：池里有空号就立刻换号，整池都满才按 Retry-After 等一次；
//   - 槽位不够：短暂排队（admissionWait），超时才回 429。
func (s *Server) openModel(ctx context.Context, path string, body []byte, affinity, group string) (*http.Response, *deepai.Account, error) {
	accts := s.Store.Accounts()
	if len(accts) == 0 {
		return nil, nil, errors.New("没有可用账号，请先在控制台导入桌面端凭据或添加 API Key")
	}
	settings := s.Store.Settings()
	maxTries := settings.MaxRetries
	if maxTries <= 0 {
		maxTries = 3
	}
	if maxTries > len(accts)+2 {
		maxTries = len(accts) + 2
	}

	var (
		lastErr error
		used    *deepai.Account
	)
	deadline := time.Now().Add(s.admissionWait)
	for i := 0; i < maxTries; i++ {
		acct, ok, candidates := s.pickFreeAccount(group, affinity)
		if !candidates {
			// 池里没有可调度账号（全被拦 / 分组为空）：交给 noAccountError 说明原因。
			break
		}
		if !ok {
			// 全忙：短暂排队而不是立刻 429；等槽位不消耗重试预算。
			lastErr = errAllAccountsBusy()
			if !time.Now().Before(deadline) {
				break
			}
			select {
			case <-time.After(admissionPoll):
			case <-ctx.Done():
				return nil, used, ctx.Err()
			}
			i--
			continue
		}
		// 额度快照过旧时顺手异步刷新：只刷新「已经探过、但结果过期」的账号，
		// 不在热点路径上给从未探测的号新增上游请求（那些交给入池复探 / 后台守护）。
		if acct.Theos != nil && quotaNeedsRefresh(acct, time.Now(), quotaStaleAfter) {
			s.probeAsync(acct)
		}
		used = acct

		client := s.clientFor(acct)
		// 与桌面端一致：推理前先补签到（仅在挑战过期时，失败不阻塞）。
		if path == deepai.PathChat || path == deepai.PathResponses {
			client.EnsureCheckIn(ctx, acct)
		}
		resp, err := client.OpenModel(ctx, acct, path, body, affinity)
		if err != nil {
			s.releaseAccount(acct.ID)
			s.unpinAccount(affinity)
			lastErr = err
			s.Store.MarkAccountResult(acct.ID, false, err.Error())
			if ctx.Err() != nil {
				break
			}
			continue
		}

		if resp.StatusCode/100 == 2 {
			s.Store.MarkAccountResult(acct.ID, true, "")
			// 应答成功才把会话钉到这个号：失败换号时钉回去只会重复踩坑。
			s.pinAccount(affinity, acct.ID)
			// 槽位随响应体一起释放：流式响应可能持续数分钟。
			resp.Body = &releaseBody{ReadCloser: resp.Body, release: func() { s.releaseAccount(acct.ID) }}
			return resp, acct, nil
		}

		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			s.releaseAccount(acct.ID)
			lastErr = readErr
			s.Store.MarkAccountResult(acct.ID, false, readErr.Error())
			continue
		}
		// 非 2xx：先松绑会话亲和，再决定「同号重试」还是「换号」。
		s.unpinAccount(affinity)
		httpErr := &deepai.HTTPError{Op: "上游推理", Status: resp.StatusCode, Body: payload}

		// 上游 0.3.19 对请求体做了形状校验（400 + "This request is not one
		// DeepAI Code sends."）。用桌面端字段白名单 + 补 system 消息同号重试一次。
		if httpErr.BodyRejected() && path == deepai.PathChat {
			if alt, ok := deepai.ChatBodyFallback(body, ""); ok {
				log.Printf("[upstream] 请求体被拒，按桌面端形状重试：account=%s", acctLabel(acct))
				if retry, err2 := client.OpenModel(ctx, acct, path, alt, affinity); err2 == nil {
					if retry.StatusCode/100 == 2 {
						s.Store.MarkAccountResult(acct.ID, true, "")
						// 回退重试换了一套请求体（前缀变了），但号没变：
						// 仍然把它当作这条会话的宿主，不必重新轮询。
						s.pinAccount(affinity, acct.ID)
						retry.Body = &releaseBody{ReadCloser: retry.Body, release: func() { s.releaseAccount(acct.ID) }}
						return retry, acct, nil
					}
					payload2, _ := io.ReadAll(io.LimitReader(retry.Body, 4<<20))
					_ = retry.Body.Close()
					if len(payload2) > 0 {
						payload = payload2
						httpErr = &deepai.HTTPError{Op: httpErr.Op, Status: retry.StatusCode, Body: payload2}
					}
				} else {
					log.Printf("[upstream] 形状回退重试失败：account=%s err=%v", acctLabel(acct), err2)
				}
			}
		}
		s.releaseAccount(acct.ID)

		// 邮箱未验证：账号登录态无效，换号也救不了它。
		// 标记后继续试下一个号（下次 /theos 成功会自动解封）；
		// 不要把它当普通 403 反复重试。
		if httpErr.EmailUnverified() {
			s.Store.BlockAccount(acct.ID, blockedReason(httpErr))
			lastErr = httpErr
			continue
		}
		// 账号级策略拦截：记 blocked 后直接回传（下一次请求调度会跳过它）。
		if httpErr.SocialLoginRequired() {
			s.Store.BlockAccount(acct.ID, blockedReason(httpErr))
			return nil, acct, httpErr
		}
		// 缺机器指纹：换号也没用（所有账号共用同一个探测结果）。
		if httpErr.MachineRequired() {
			return nil, acct, httpErr
		}
		// 车道不对：先用免鉴权的 /models 探一次，命中新车道就换道重试；
		// 仍在冷却期 / 探测失败时把 426 原样交给调用方（提示升级网关）。
		if httpErr.LaneUnsupported() {
			before := s.CurrentLane()
			if lane, lerr := s.RefreshLane(ctx); lerr == nil && lane != "" && lane != before {
				lastErr = httpErr
				continue
			}
			return nil, acct, httpErr
		}

		// 容量拒绝：池里还有空槽就立刻换号（粘性由 X-Session-Affinity 保证，
		// 不需要在同一个号上干等）；整池都满了才按 Retry-After 等一次。
		if httpErr.CapacityWaiting() {
			lastErr = httpErr
			if s.hasFreeAccount() {
				continue
			}
			wait := deepai.RetryAfter(resp, 2*time.Second)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, acct, ctx.Err()
			}
			continue
		}

		// 请求本身的问题：直接回传，不浪费其他账号。
		if !httpErr.Retryable() {
			return nil, acct, httpErr
		}

		lastErr = httpErr
		s.Store.MarkAccountResult(acct.ID, false, httpErr.Error())
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = s.noAccountError()
	}
	return nil, used, lastErr
}

// blockedReason 把上游错误码拼进账号的 last_err。
//
// 光存 message 会丢掉 code：全池被拦时要靠它区分 social_login_required /
// email_unverified，才能给调用方一个能行动的错误。
func blockedReason(he *deepai.HTTPError) string {
	if code := he.Code(); code != "" {
		return code + ": " + he.Error()
	}
	return he.Error()
}

// noAccountError 在没有可选账号时给出可执行的错误。
//
// 池里全部账号都被上游要求改用 Google/Apple 登录时，不能只回一句
// 「全部账号均不可用」——那是运营层面的策略变更，必须让调用方看见原因。
func (s *Server) noAccountError() error {
	accts := s.Store.Accounts()
	if len(accts) == 0 {
		return errors.New("没有可用账号，请先在控制台导入桌面端凭据或添加 API Key")
	}
	blocked := 0
	for _, a := range accts {
		if a.Status == deepai.StatusBlocked {
			blocked++
		}
	}
	if blocked > 0 && blocked == len(accts) {
		// 全部被拦时要把原因说清楚：只报“全部不可用”会让人以为网关坏了。
		social, unverified := 0, 0
		for _, a := range accts {
			switch {
			case strings.Contains(a.LastErr, deepai.CodeSocialLogin):
				social++
			case strings.Contains(a.LastErr, deepai.CodeEmailUnverified):
				unverified++
			}
		}
		if unverified > 0 && social == 0 {
			return &deepai.HTTPError{
				Op:     "账号调度",
				Status: http.StatusServiceUnavailable,
				Body:   []byte(`{"error":{"message":"全部账号都被上游判定为邮箱未验证（email_unverified），请先验证邮箱后重试","type":"email_unverified","code":"email_unverified"}}`),
			}
		}
		if social > 0 {
			return &deepai.HTTPError{
				Op:     "账号调度",
				Status: http.StatusServiceUnavailable,
				Body:   []byte(`{"error":{"message":"全部账号已被上游要求改用 Google/Apple 登录（social_login_required）；邮箱密码注册的账号无法再推理","type":"social_login_required","code":"social_login_required"}}`),
			}
		}
		return &deepai.HTTPError{
			Op:     "账号调度",
			Status: http.StatusServiceUnavailable,
			Body:   []byte(`{"error":{"message":"全部账号都被上游拦截（策略 / 登录态），请到控制台查看各账号的 last_err","type":"account_blocked","code":"account_blocked"}}`),
		}
	}
	return errors.New("全部账号均不可用")
}

// callJSON 发起一次非流式上游请求并解析 JSON。
func (s *Server) callJSON(ctx context.Context, path string, payload map[string]any, affinity, group string) (map[string]any, *deepai.Account, error) {
	ctx, cancel := context.WithTimeout(ctx, nonStreamBudget)
	defer cancel()

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("请求序列化失败: %w", err)
	}
	resp, acct, err := s.openModel(ctx, path, body, affinity, group)
	if err != nil {
		return nil, acct, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, acct, fmt.Errorf("读取上游响应失败: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, acct, fmt.Errorf("上游响应解析失败: %w", err)
	}
	return out, acct, nil
}

// writeUpstreamErr 把上游错误原样（或包装后）返回给客户端。
func writeUpstreamErr(w http.ResponseWriter, err error) {
	var he *deepai.HTTPError
	if errors.As(err, &he) {
		if json.Valid(he.Body) && len(he.Body) > 0 {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(he.Status)
			_, _ = w.Write(he.Body)
			return
		}
		writeErr(w, he.Status, he.Error(), "upstream_error")
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeErr(w, http.StatusGatewayTimeout, "上游请求超时", "upstream_error")
		return
	}
	writeErr(w, http.StatusBadGateway, err.Error(), "upstream_error")
}

// writeAnthropicUpstreamErr 按 Anthropic 错误结构回传上游错误。
func writeAnthropicUpstreamErr(w http.ResponseWriter, err error) {
	var he *deepai.HTTPError
	if errors.As(err, &he) {
		typ := "api_error"
		switch he.Status {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
			typ = "invalid_request_error"
		case http.StatusUnauthorized, http.StatusForbidden:
			typ = "authentication_error"
		case http.StatusTooManyRequests:
			typ = "rate_limit_error"
		}
		writeAnthropicErr(w, he.Status, typ, he.Error())
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	writeAnthropicErr(w, http.StatusBadGateway, "api_error", err.Error())
}

// parseUsage 兼容 chat/completions 与 responses 两种用量字段。
func parseUsage(v any) Usage {
	m, ok := v.(map[string]any)
	if !ok {
		return Usage{}
	}
	u := Usage{
		PromptTokens:     intOf(m["prompt_tokens"]),
		CompletionTokens: intOf(m["completion_tokens"]),
		TotalTokens:      intOf(m["total_tokens"]),
		Cost:             floatOf(m["cost"]),
	}
	if u.PromptTokens == 0 {
		u.PromptTokens = intOf(m["input_tokens"])
	}
	if u.CompletionTokens == 0 {
		u.CompletionTokens = intOf(m["output_tokens"])
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if d, ok := m["completion_tokens_details"].(map[string]any); ok {
		u.ReasoningTokens = intOf(d["reasoning_tokens"])
	}
	if d, ok := m["output_tokens_details"].(map[string]any); ok && u.ReasoningTokens == 0 {
		u.ReasoningTokens = intOf(d["reasoning_tokens"])
	}
	if c, ok := m["cost_details"].(map[string]any); ok && u.Cost == 0 {
		u.Cost = floatOf(c["upstream_inference_cost"])
	}
	// 缓存用量：上游（OpenRouter 风格）把命中量放在 prompt_tokens_details，
	// Responses 形状放在 input_tokens_details，Anthropic 形状是独立字段。
	if d, ok := m["prompt_tokens_details"].(map[string]any); ok {
		u.CachedTokens = intOf(d["cached_tokens"])
		u.CacheWriteTokens = intOf(d["cache_write_tokens"])
	}
	if d, ok := m["input_tokens_details"].(map[string]any); ok && u.CachedTokens == 0 {
		u.CachedTokens = intOf(d["cached_tokens"])
	}
	if u.CachedTokens == 0 {
		u.CachedTokens = intOf(m["cache_read_input_tokens"])
	}
	if u.CacheWriteTokens == 0 {
		u.CacheWriteTokens = intOf(m["cache_creation_input_tokens"])
	}
	return u
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func floatOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// record 记录一次请求元信息。
// reqLog 是一次请求的日志上下文。
//
// 旧版 record() 收 11 个位置参数（key/acct/model/endpoint/stream/status/
// start/inLen/outLen/usage/err），二十余处调用完全靠记忆参数顺序，
// 写错编译器也不报错。现在把不变量在构造时固化，调用点只补变化的部分。
type reqLog struct {
	s        *Server
	key      string
	acct     *deepai.Account
	model    string
	endpoint string
	stream   bool
	start    time.Time
	inLen    int
}

// newReqLog 固化一次请求的公共字段。acct 可以为 nil（尚未选到账号）。
func (s *Server) newReqLog(key string, acct *deepai.Account, model, endpoint string, stream bool, start time.Time, inLen int) reqLog {
	return reqLog{
		s: s, key: key, acct: acct, model: model,
		endpoint: endpoint, stream: stream, start: start, inLen: inLen,
	}
}

// done 落一条请求日志；err 为 nil 视为成功（HTTP 200），否则记为 502。
func (l reqLog) done(outLen int, usage Usage, err error) {
	e := store.LogEntry{
		Time:         time.Now().Unix(),
		Key:          mask(l.key),
		Model:        l.model,
		Endpoint:     l.endpoint,
		Stream:       l.stream,
		Status:       statusOf(err),
		LatencyMS:    time.Since(l.start).Milliseconds(),
		PromptLen:    l.inLen,
		OutputLen:    outLen,
		TokensIn:     usage.PromptTokens,
		TokensOut:    usage.CompletionTokens,
		TokensCached: usage.CachedTokens,
		Cost:         usage.Cost,
	}
	if l.acct != nil {
		e.Account = l.acct.ID
	}
	if err != nil {
		e.Err = err.Error()
	}
	l.s.Store.Record(e, outLen)
}

// readBodyWithLimit 读取入站请求体。
func readBodyWithLimit(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
}

// requestModelName 提取请求里的 model 字段。
func requestModelName(payload map[string]any) string {
	v, _ := payload["model"].(string)
	return strings.TrimSpace(v)
}

// estimatePromptLen 粗略统计 messages / input 的字符数。
func estimatePromptLen(payload map[string]any) int {
	if v, ok := payload["messages"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			return len(raw)
		}
	}
	if v, ok := payload["input"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			return len(raw)
		}
	}
	return 0
}

// chatTextLen 统计 chat 响应中的正文 + 思维链字符数。
func chatTextLen(m map[string]any) int {
	choices, _ := m["choices"].([]any)
	total := 0
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if msg, ok := cm["message"].(map[string]any); ok {
			total += lenOfString(msg["content"])
			total += lenOfString(msg["reasoning"])
		}
		if d, ok := cm["delta"].(map[string]any); ok {
			total += lenOfString(d["content"])
			total += lenOfString(d["reasoning"])
		}
	}
	return total
}

func lenOfString(v any) int {
	s, _ := v.(string)
	return len([]rune(s))
}

// writeRawJSON 直接写出已生成的 JSON 字节。
func writeRawJSON(w http.ResponseWriter, code int, raw []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(raw)
}

// sseWriter 统一 SSE 响应头与 flush。
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	flusher, _ := w.(http.Flusher)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	// 在这里提交 200：调用方随后再写 header 会被 net/http 判为
	// superfluous（旧实现正是如此，只在日志里制造噪音）。
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	return &sseWriter{w: w, flusher: flusher}
}

func (s *sseWriter) data(raw []byte) {
	_ = deepai.WriteSSE(s.w, raw)
	s.flush()
}

func (s *sseWriter) event(name string, raw []byte) {
	_ = deepai.WriteSSEEvent(s.w, name, raw)
	s.flush()
}

func (s *sseWriter) done() {
	_, _ = s.w.Write([]byte("data: [DONE]\n\n"))
	s.flush()
}

func (s *sseWriter) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// jsonEvent 序列化事件载荷，失败时返回 nil。
func jsonEvent(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

// requestGroup 返回当前请求密钥绑定的号池分组（无密钥时为空=全部分组）。
func (s *Server) requestGroup(r *http.Request) string {
	if r == nil {
		return ""
	}
	key := extractKey(r)
	if key == "" {
		return ""
	}
	return s.Store.KeyGroup(key)
}

// nextAccount 按请求所属分组挑一个轮询账号。
func (s *Server) nextAccount(r *http.Request) *deepai.Account {
	return s.Store.NextAccountGroup(s.requestGroup(r))
}

// clientFor 返回账号绑定的上游客户端（按代理缓存）。
func (s *Server) clientFor(acct *deepai.Account) *deepai.Client {
	if acct == nil || strings.TrimSpace(acct.Proxy) == "" {
		return s.Up
	}
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	if s.proxyClients == nil {
		s.proxyClients = map[string]*deepai.Client{}
	}
	if c, ok := s.proxyClients[acct.Proxy]; ok {
		return c
	}
	c := deepai.NewClientWithProxy(acct.Proxy)
	s.proxyClients[acct.Proxy] = c
	return c
}
