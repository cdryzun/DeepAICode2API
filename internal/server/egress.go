package server

import (
	"log"
	"os"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

// 出口（egress）自动漂移。
//
// 背景：上游的「当日额度」是按 **(账号 × 出口 IP)** 计数的。同一个账号换一条
// 出口就是一份新额度，而每条出口的延迟差得很远（机房直连 ~0.55s，绕办公家宽
// ~1.8s）。所以正确的策略不是「固定用哪条出口」，而是：
//
//  1. 首选最快的那条（直连）；
//  2. 它报 `Too many requests today` 就只把这**一个账号**漂到下一出口，
//     并且立刻重试，而不是当成账号故障重试三次；
//  3. 上游额度刷新（theos.refresh_at）之后自动回切到首选出口。
//
// 这里是第 2、3 步的实现。第 1 步由 EgressCandidates 的顺序决定。

// EgressDirect 是「直连」出口的标识：账号的 Proxy 为空字符串。
const EgressDirect = ""

// egressDirectLabel 是直连在日志/冷却表里的可读名。
const egressDirectLabel = "direct"

// EgressPoolEnv 是显式指定出口优先级的变量；留空时用「直连 + PROXY_POOL」。
const EgressPoolEnv = "DEEPAI2API_EGRESS_POOL"

// egressCooldownFallback 是拿不到 refresh_at 时的冷却时长。
//
// 上游的按天额度通常次日 UTC 刷新；拿不到确切时间时宁可保守一些，
// 也不要每 30 秒去撞一次已经耗尽的出口。
const egressCooldownFallback = 6 * time.Hour

// EgressLabel 返回出口的可读名（用于日志与冷却表键）。
func EgressLabel(proxy string) string {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		return egressDirectLabel
	}
	return proxy
}

// EgressCandidates 返回按优先级排序的出口列表（首项最快）。
//
// 默认顺序：直连 → PROXY_POOL 里的每个代理（保留配置顺序）。
// DEEPAI2API_EGRESS_POOL 非空时以它为准，支持 `direct` 作为直连的写法。
func EgressCandidates() []string {
	if raw := strings.TrimSpace(os.Getenv(EgressPoolEnv)); raw != "" {
		return normalizeEgressList(raw)
	}
	out := []string{EgressDirect}
	for _, p := range normalizeEgressList(os.Getenv("PROXY_POOL")) {
		if p != EgressDirect {
			out = append(out, p)
		}
	}
	return out
}

// normalizeEgressList 把逗号/换行/空白分隔的出口清单编号成去重列表。
func normalizeEgressList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';' || r == ' ' || r == '\t'
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.EqualFold(f, "direct") || strings.EqualFold(f, "none") {
			f = EgressDirect
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// egressCooled 返回该出口对这个账号的冷却到期时间（零值 = 没凉）。
func egressCooled(a *deepai.Account, proxy string) time.Time {
	if a == nil || len(a.EgressCooldown) == 0 {
		return time.Time{}
	}
	if ts, ok := a.EgressCooldown[EgressLabel(proxy)]; ok && ts > 0 {
		return time.Unix(ts, 0)
	}
	return time.Time{}
}

// egressExhausted 报告账号当前出口是否还在冷却期内。
func egressExhausted(a *deepai.Account) bool {
	if a == nil {
		return false
	}
	until := egressCooled(a, a.Proxy)
	return !until.IsZero() && time.Now().Before(until)
}

// cooldownUntil 决定某个出口要凉到什么时候：优先用账号的额度刷新时间。
func cooldownUntil(a *deepai.Account, now time.Time) time.Time {
	if a != nil && a.Theos != nil {
		if ts := parseRefreshAt(a.Theos.RefreshAt); ts > 0 {
			if t := time.Unix(int64(ts), 0); t.After(now) {
				return t.Add(time.Minute) // 留 1 分钟余量，避免刚好卡在刷新的那一秒
			}
		}
	}
	return now.Add(egressCooldownFallback)
}

// parseRefreshAt 解析 /theos 返回的 refresh_at（RFC3339）。
func parseRefreshAt(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return float64(t.Unix())
	}
	return 0
}

// rotateEgressOnExhausted 在「当前出口当日额度耗尽」时把账号漂到下一条出口。
//
// 返回 true 表示已经切换到一条可用出口，调用方应当立刻重试；
// 返回 false 表示没有可用出口了（全部在冷却期内）。
func (s *Server) rotateEgressOnExhausted(a *deepai.Account, note string) bool {
	if a == nil {
		return false
	}
	from := a.Proxy
	until := cooldownUntil(a, time.Now())

	next := ""
	found := false
	s.Store.UpdateAccount(a.ID, func(acc *deepai.Account) {
		if acc.EgressCooldown == nil {
			acc.EgressCooldown = map[string]int64{}
		}
		acc.EgressCooldown[EgressLabel(from)] = until.Unix()
		for _, cand := range EgressCandidates() {
			if cand == from {
				continue
			}
			if t, ok := acc.EgressCooldown[EgressLabel(cand)]; ok && time.Now().Before(time.Unix(t, 0)) {
				continue
			}
			next = cand
			found = true
			break
		}
		if found {
			acc.Proxy = next
			acc.LastErr = note
			// 出口切换不等于账号故障：失败计数归零，避免被「失败最少优先」饿死。
			acc.Failures = 0
			if acc.Status == "error" || acc.Status == "exhausted" {
				acc.Status = "unknown"
			}
		}
	})
	_ = s.Store.Save()
	if !found {
		log.Printf("[egress] %s 的所有出口当日额度均已耗尽（%s），冷静到 %s",
			acctLabel(a), EgressLabel(from), until.Format(time.RFC3339))
		return false
	}
	log.Printf("[egress] %s 出口额度耗尽：%s → %s（原出口冷却到 %s）",
		acctLabel(a), EgressLabel(from), EgressLabel(next), until.Format(time.RFC3339))
	return true
}

// RestoreEgress 把冷却到期的账号切回首选出口（通常是直连，最快）。
//
// 由后台守护周期调用。返回恢复的账号数。
func (s *Server) RestoreEgress() int {
	preferred := EgressDirect
	if list := EgressCandidates(); len(list) > 0 {
		preferred = list[0]
	}
	restored := 0
	s.Store.UpdateAccounts(func(a *deepai.Account) bool {
		changed := false
		now := time.Now()
		// 先清掉已过期的冷却记录（保留未到期的）。
		for label, ts := range a.EgressCooldown {
			if !now.Before(time.Unix(ts, 0)) {
				delete(a.EgressCooldown, label)
				changed = true
			}
		}
		// 首选出口不再冷却、且当前不在首选上 → 回切。
		if a.Proxy != preferred && egressCooled(a, preferred).IsZero() {
			log.Printf("[egress] %s 回切到首选出口：%s → %s",
				acctLabel(a), EgressLabel(a.Proxy), EgressLabel(preferred))
			a.Proxy = preferred
			a.LastErr = ""
			if a.Status == "exhausted" {
				a.Status = "unknown"
			}
			changed = true
		}
		if changed {
			restored++
		}
		return changed
	})
	if restored > 0 {
		_ = s.Store.Save()
	}
	return restored
}
