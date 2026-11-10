// Package pool 实现号池的选号策略（分组过滤 + 失败度加权轮询 + 额度感知）。
//
// 这些是业务策略而不是持久化细节，原先住在 store 包里导致：
//   - 单测必须构造完整 Store 才能验证调度；
//   - 调整「0 额度何时算陈旧」这类规则要动持久化层。
//
// 抽出来后 Scheduler 不持有账号列表，调用方在持锁状态下把快照传进来。
package pool

import (
	"strconv"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

// QuotaStaleAfter 是「0 额度」被判为陈旧、需要重探的时限。
//
// 新注册的账号在入池那一刻常常还是 0，几分钟后才会下发当日额度
// （见 README §6.10 实测）。把陈旧的 0 当作「未知」，避免账号被永久
// 排除在调度之外。
const QuotaStaleAfter = 15 * time.Minute

// Scheduler 保存轮询游标，实现号池选号。
//
// 非并发安全：调用方必须在自己的锁内使用（store 用写锁保护它）。
type Scheduler struct {
	rotate int
}

// New 构造一个调度器。
func New() *Scheduler { return &Scheduler{} }

// Pick 在 accounts 里按分组与健康度选一个账号；无可用账号时返回 nil。
//
// group 为空表示全部分组。调度分两轮：
//  1. 只考虑「额度未知或额度 > 0」的账号——上游按机器发每日免费额度，
//     池里常有 balance=0 的账号，先打过去只会被上游拒一次再换号；
//  2. 再在全集里选，保证有账号时绝不硬阻塞。
//
// 每轮内部优先选失败次数最少的账号，同失败度时按轮询游标推进。
func (s *Scheduler) Pick(accounts []*deepai.Account, group string) *deepai.Account {
	group = strings.TrimSpace(group)
	eligible := func(a *deepai.Account) bool {
		if !a.Enabled {
			return false
		}
		// 上游策略拦截（social_login_required）是账号级终态：
		// 继续轮询只会把每个请求都撞一次 403。
		if a.Status == deepai.StatusBlocked {
			return false
		}
		// 当日额度在所有出口上都用光：刷新前打它只会拿 429。
		// 这不是终态——后台 RestoreEgress 会在 refresh_at 之后改回 unknown。
		if a.Status == deepai.StatusExhausted {
			return false
		}
		return group == "" || strings.EqualFold(a.GroupName(), group)
	}
	if a := s.pick(accounts, eligible, true); a != nil {
		return a
	}
	return s.pick(accounts, eligible, false)
}

// pick 在 eligible 里按「失败最少 + 轮询」选一个账号。
//
// needQuota 为 true 时跳过已知额度为 0 的账号。
func (s *Scheduler) pick(accounts []*deepai.Account, eligible func(*deepai.Account) bool, needQuota bool) *deepai.Account {
	n := len(accounts)
	var best *deepai.Account
	bestIdx := -1
	for i := 0; i < n; i++ {
		idx := (s.rotate + i) % n
		a := accounts[idx]
		if !eligible(a) {
			continue
		}
		if needQuota && !HasQuota(a) {
			continue
		}
		if best == nil || a.Failures < best.Failures {
			best = a
			bestIdx = idx
		}
		if a.Failures == 0 {
			s.rotate = (idx + 1) % n
			return a
		}
	}
	if bestIdx >= 0 {
		s.rotate = (bestIdx + 1) % n
	}
	return best
}

// HasQuota 报告账号是否可能还有可用额度。
//
// 保守为准：只有「探测过、启用、且余额能解析为 0」的账号才算耗尽，
// 其余（未探测 / 解析不了 / 已付费）都当作可能可用。
func HasQuota(a *deepai.Account) bool {
	if a == nil || a.Theos == nil || !a.Theos.Enabled {
		return true
	}
	bal := strings.TrimSpace(a.Theos.Balance)
	if bal == "" {
		return true
	}
	v, err := strconv.ParseFloat(bal, 64)
	if err != nil {
		return true
	}
	if v > 0 {
		return true
	}
	// 0 额度只在「刚探过」时才算数。
	if a.Theos.CheckedAt <= 0 {
		return true
	}
	return time.Since(time.Unix(a.Theos.CheckedAt, 0)) > QuotaStaleAfter
}
