package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/registry"
	"deepaicode2api/internal/store"
)

// --- 控制台读接口（overview / 分页账号 / 日志查询） ---
//
// 拆分的动机：/admin/api/state 一次下发全部账号（603 个约 429KB），
// 而控制台一屏只渲染 20 行。这里按「不常变的元信息」与「可翻页的账号列表」
// 分成两个端点，并都带 ETag，空闲刷新退化为 304。

// accountView 是账号下发给控制台的投影（完整 Key 与机器指纹不外泄）。
func accountView(a *deepai.Account) map[string]any {
	// 额度时效：控制台不必自己算 checked_at 与当前时间的差，
	// 否则每个前端都得复制一份“多久算陈旧”的规则。
	age := int64(0)
	if a.Theos != nil && a.Theos.CheckedAt > 0 {
		if d := time.Now().Unix() - a.Theos.CheckedAt; d > 0 {
			age = d
		}
	}
	return map[string]any{
		"id": a.ID, "name": a.Name, "source": a.Source,
		"key": deepai.MaskSecret(a.APIKey), "key_id": a.KeyID(),
		"username": a.Username, "email": a.Email, "is_pro": a.IsPro,
		"plan": a.Plan, "email_verified": a.EmailVerified,
		"user_pk": a.UserPK, "group": a.GroupName(),
		"proxy": deepai.MaskProxy(a.Proxy), "fingerprint": a.Fingerprint,
		// 出口冷却表：哪条出口对这个号的当日额度已用光、什么时候回切。
		"egress_cooldown": a.EgressCooldown,
		// 机器指纹只下发摘要：上游按它发额度，完整值没必要出网。
		"machine": maskMachine(a.MachineID), "has_machine": a.MachineID != "",
		"registered_at": a.RegisteredAt, "last_verified": a.LastVerified,
		"enabled": a.Enabled, "status": a.Status, "note": a.Note,
		"last_err": a.LastErr, "failures": a.Failures, "last_used": a.LastUsed,
		"theos": a.Theos,
		// quota_age 是额度快照的年龄（秒）；quota_due 表示按需复探会去刷新它。
		"quota_age": age,
		"quota_due": quotaNeedsRefresh(a, time.Now(), quotaStaleAfter),
	}
}

// handleAdminOverview 返回不随账号数增长的元信息。
//
// 含：统计、密钥、模型目录、设置、机器指纹、上游地址。
// 账号列表请走 /admin/api/accounts（支持分页与过滤）。
func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	settings := s.Store.Settings()
	total, ready := s.Store.AccountCounts()
	writeJSONCached(w, r, http.StatusOK, map[string]any{
		"accounts":       total,
		"ready":          ready,
		"keys":           safeKeyList(s.Store.Keys()),
		"models":         registry.List(s.catalog(), settings.ModelAliases),
		"default_model":  s.Store.DefaultModel(),
		"models_updated": s.Store.ModelsUpdated(),
		"machine_id":     shortMachine(s.Store.MachineID()),
		"settings":       sanitizedSettings(settings),
		"stats":          s.Store.Stats(),
		"upstream":       s.Up.Origin,
	})
}

// handleAdminAccountsPage 分页返回账号列表。
//
// 查询参数：offset / limit / group / status / q。
// 响应：{items, total, offset, limit}，并带 ETag。
func (s *Server) handleAdminAccountsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))

	sortKey := strings.TrimSpace(q.Get("sort"))
	desc := strings.EqualFold(q.Get("order"), "desc")

	page := s.Store.AccountPage(store.AccountQuery{
		Offset: offset, Limit: limit,
		Group: q.Get("group"), Status: q.Get("status"), Query: q.Get("q"),
		Sort: sortKey, Desc: desc,
	})
	items := make([]map[string]any, 0, len(page.Items))
	for _, a := range page.Items {
		items = append(items, accountView(a))
	}
	writeJSONCached(w, r, http.StatusOK, map[string]any{
		"items":  items,
		"total":  page.Total,
		"offset": offset,
		"limit":  limit,
		"sort":   sortKey,
		"order":  map[bool]string{true: "desc", false: "asc"}[desc],
	})
}

// logFilterFromQuery 把查询参数解析成 store.QueryFilter。
//
// 支持：limit / since / endpoint / account / min_status / q。
func logFilterFromQuery(r *http.Request) store.QueryFilter {
	q := r.URL.Query()
	f := store.QueryFilter{
		Endpoint: strings.TrimSpace(q.Get("endpoint")),
		Account:  strings.TrimSpace(q.Get("account")),
		Keyword:  strings.TrimSpace(q.Get("q")),
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.MinStatus, _ = strconv.Atoi(q.Get("min_status"))
	if v, _ := strconv.ParseInt(q.Get("since"), 10, 64); v > 0 {
		f.Since = v
	}
	if f.Limit <= 0 {
		f.Limit = 200
	}
	return f
}

// --- ETag 支持 ---

// writeJSONCached 写出 JSON 并带 ETag；命中 If-None-Match 时回 304。
func writeJSONCached(w http.ResponseWriter, r *http.Request, code int, v any) {
	raw := jsonEvent(v)
	if raw == nil {
		writeErr(w, http.StatusInternalServerError, "响应序列化失败", "server_error")
		return
	}
	sum := sha256.Sum256(raw)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "no-cache")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeRawJSON(w, code, raw)
}

// etagMatches 判断 If-None-Match 里是否包含目标 ETag（支持逗号分隔与弱校验前缀）。
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, part := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(part), "W/") == want {
			return true
		}
	}
	return false
}
