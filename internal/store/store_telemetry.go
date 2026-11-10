package store

import (
	"time"
)

// --- 设置 / 统计 / 日志 ---

// Settings 返回设置快照。
func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := s.st.Settings
	cp.ModelAliases = map[string]string{}
	for k, v := range s.st.Settings.ModelAliases {
		cp.ModelAliases[k] = v
	}
	return cp
}

// UpdateSettings 局部更新设置。
func (s *Store) UpdateSettings(fn func(*Settings)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st.Settings)
}

// Stats 返回统计快照。
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.Stats
}

// Logs 返回最近的日志。
func (s *Store) Logs(limit int) []LogEntry {
	s.mu.RLock()
	logs := s.logs
	s.mu.RUnlock()
	if logs == nil {
		return nil
	}
	return logs.tail(limit)
}

// Record 记录一次请求结果并更新统计。
func (s *Store) Record(e LogEntry, outChars int) {
	s.mu.Lock()
	s.st.Stats.TotalRequests++
	if e.Status >= 400 || e.Err != "" {
		s.st.Stats.FailedRequests++
	}
	s.st.Stats.TotalCharsOut += int64(outChars)
	s.st.Stats.TotalTokensIn += int64(e.TokensIn)
	s.st.Stats.TotalTokensOut += int64(e.TokensOut)
	s.st.Stats.TotalTokensCached += int64(e.TokensCached)
	s.st.Stats.TotalCost += e.Cost
	s.statsDirty = true
	keep := s.st.Settings.LogKeep
	logs := s.logs
	s.mu.Unlock()

	// 日志写入（含文件 IO）放在锁外：它不应该阻塞号池调度。
	if logs != nil {
		_ = logs.append(e, keep)
	}
}

// QueryLogs 按条件查询日志。
//
// 走文件扫描（最多读尾部 4MB），所以能回溯超过内存环形缓冲的历史；
// 文件不可读时退回内存环形缓冲。limit<=0 时默认 200。
func (s *Store) QueryLogs(f QueryFilter) []LogEntry {
	if f.Limit <= 0 {
		f.Limit = 200
	}
	s.mu.RLock()
	logs := s.logs
	s.mu.RUnlock()
	if logs == nil {
		return nil
	}
	out, err := logs.scan(f)
	if err != nil {
		return logs.query(f)
	}
	return out
}

// ClearLogs 清空日志（内存环形缓冲与文件）。
func (s *Store) ClearLogs() {
	s.mu.RLock()
	logs := s.logs
	s.mu.RUnlock()
	if logs != nil {
		_ = logs.clear()
	}
}

// ClearStats 重置统计。
func (s *Store) ClearStats() {
	s.mu.Lock()
	started := s.st.Stats.StartedAt
	if started == 0 {
		started = time.Now().Unix()
	}
	// 保留 StartedAt（进程起点），单独记 ResetAt：日志不清空，
	// 控制台需要能解释「统计数小于日志条数」。
	s.st.Stats = Stats{StartedAt: started, ResetAt: time.Now().Unix()}
	s.statsDirty = true
	s.mu.Unlock()
}
