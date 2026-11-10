// DeepAICode2API 把本机 DeepAI Code 桌面端的上游模型服务，
// 逆向复刻成一个 OpenAI / Anthropic 兼容的 Go 网关。
//
// 复用桌面端已登录的 API Key，无需另申请密钥；纯 Go 标准库实现。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/envfile"
	"deepaicode2api/internal/server"
	"deepaicode2api/internal/store"
)

// version 由构建时注入：-ldflags "-X main.version=1.0.0"
var version = "dev"

func main() {
	// 必须在任何环境变量读取之前执行：flag 默认值（envOr）与 store.Open 的
	// 主密钥都直读进程环境。此前 .env 只在注册链路里惰性加载，导致
	// DEEPAI2API_MASTER_KEY 对网关启动永远不生效（状态文件只能保持明文）。
	envfile.Load()

	var (
		host          = flag.String("host", envOr("DEEPAI2API_HOST", "127.0.0.1"), "监听地址")
		port          = flag.Int("port", envInt("DEEPAI2API_PORT", 10087), "监听端口")
		dataPath      = flag.String("data", envOr("DEEPAI2API_DATA_PATH", ""), "状态文件路径（默认在可执行文件同目录）")
		deepaiDir     = flag.String("deepai-dir", envOr("DEEPAI2API_DATA_DIR", ""), "DeepAI Code 数据目录（留空自动探测）")
		importOnly    = flag.Bool("import", false, "仅执行一次桌面端凭据导入后退出")
		noImport      = flag.Bool("no-import", false, "启动时不自动导入桌面端凭据")
		adminPw       = flag.String("admin-password", envOr("DEEPAI2API_ADMIN_PASSWORD", ""), "管理密码（非空时覆盖状态文件中的设置）")
		allowOpen     = flag.Bool("allow-insecure", envBool("DEEPAI2API_ALLOW_INSECURE", false), "允许非回环监听但不设管理密码（不安全）")
		maxInflight   = flag.Int("max-inflight", envInt("DEEPAI2API_MAX_INFLIGHT", server.DefaultMaxInflight), "单账号同时在途的上游请求上限（号池总槽位 = 账号数 × 该值）")
		admissionWait = flag.Duration("admission-wait", envDuration("DEEPAI2API_ADMISSION_WAIT", server.DefaultAdmissionWait), "槽位全满时的排队上限，超时才回 429（0 表示立即拒绝）")
		seal          = flag.Bool("seal", false, "把状态/统计/日志中的存量明文封存为密文后退出（需 "+store.EnvMasterKey+"）")
		sealYes       = flag.Bool("yes", false, "配合 -seal：确认执行不可逆的封存")
		showVer       = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("deepaicode2api", version)
		return
	}

	if *dataPath == "" {
		*dataPath = defaultDataPath()
	}

	st, err := store.Open(*dataPath)
	if err != nil {
		log.Fatalf("打开状态文件失败: %v", err)
	}
	log.Printf("状态文件: %s", st.Path())
	if st.MasterKeySet() {
		log.Printf("状态文件加密: 已启用（AES-256-GCM，密钥指纹 %s，来自 %s）", st.MasterKeyFingerprint(), store.EnvMasterKey)
		if !st.Encrypted() {
			log.Printf("  磁盘上仍是明文：首次保存会自动重写；历史日志可用 -seal 立即封存。")
		}
	} else {
		log.Printf("⚠ 状态文件未加密：上游 API Key / 账号密码以明文落盘。")
		log.Printf("  把 %s=$(openssl rand -hex 32) 写进 .env 后重启即可（.env 现在会在启动时加载），"+
			"再执行 -seal 把历史明文一并封存。", store.EnvMasterKey)
	}

	if *seal {
		runSeal(st, *sealYes)
		return
	}

	machine := deepai.MachineID()
	if machine != "" {
		st.SetMachineID(machine)
		log.Printf("机器指纹: %s…（用于上游按天发放免费 Theos）", machine[:12])
	} else {
		log.Printf("⚠ 未能读取机器指纹：上游会拒绝所有模型请求（426 lane_version_unsupported）")
		log.Printf("  请设置 DEEPAI_MACHINE_ID=<原始机器UUID>；Windows 可执行：")
		log.Printf("  powershell -Command \"(Get-CimInstance Win32_ComputerSystemProduct).UUID\"")
	}

	if *importOnly {
		runImport(st, *deepaiDir)
		return
	}

	// 命令行 / 环境变量优先级高于状态文件；设置后立即落盘。
	if *adminPw != "" {
		st.UpdateSettings(func(s *store.Settings) { s.AdminPassword = *adminPw })
		if err := st.Save(); err != nil {
			log.Printf("保存管理密码失败: %v", err)
		} else {
			log.Printf("已启用管理密码（来自命令行 / DEEPAI2API_ADMIN_PASSWORD）")
		}
	}

	if !*noImport {
		autoImport(st, *deepaiDir)
	}
	if envAcct := deepai.EnvAccount(); envAcct != nil {
		if isNew := st.UpsertAccount(envAcct); isNew {
			log.Printf("已从 DEEPAI_API_KEY 添加账号 %s", envAcct.Name)
			_ = st.Save()
		}
	}

	// 非回环监听意味着网关已接入局域网 / 公网，此时两项底线不能靠“默认值”撑着：
	// 管理面必须有密码，数据面必须已有密钥。否则从启动到人工创建密钥之间，
	// 上游账号池就是一个无需鉴权的开放中继。
	if !isLoopbackListen(*host) {
		if st.Settings().AdminPassword == "" && !*allowOpen {
			log.Fatalf("监听地址 %q 不是回环地址，但未设置管理密码：控制台会对局域网开放。\n"+
				"  请用 -admin-password 或 DEEPAI2API_ADMIN_PASSWORD 设置密码；\n"+
				"  确实需要无密码运行请显式加 -allow-insecure（或 DEEPAI2API_ALLOW_INSECURE=1）。", *host)
		}
		if !st.HasKeys() {
			k := st.AddKey("bootstrap", "default")
			if err := st.Save(); err != nil {
				log.Fatalf("生成引导密钥失败: %v", err)
			}
			log.Printf("⚠ 非回环监听且尚未签发密钥，已自动生成引导密钥（仅本次启动显示，请立即保存）：")
			log.Printf("    %s", k.Key)
			log.Printf("  之后可在控制台改名或吊销。")
		}
	}

	srv := server.New(st)
	srv.SetAdmission(*maxInflight, *admissionWait)

	addr := net.JoinHostPort(*host, fmt.Sprintf("%d", *port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		// 流式响应可能持续数分钟，不设 WriteTimeout
	}

	// 后台任务：首个目录拉取 + 定期落盘 + 定期刷新模型目录。
	// 有意放在监听之前启动：首个目录拉取可能与上游耗上 20s 超时，
	// 先起它、再起监听，两者并行，互不阻塞。
	stopBG := make(chan struct{})
	// 车道探测：只打免鉴权的 /models，命中即落盘；失败沿用旧值，不阻塞启动。
	go func() {
		lane, err := srv.RefreshLane(context.Background())
		if err != nil {
			log.Printf("车道探测失败（沿用 %s）: %v", srv.CurrentLane(), err)
			return
		}
		log.Printf("车道版本: %s（免鉴权 /models 探测）", lane)
	}()

	go background(st, srv, stopBG)
	go quotaGuard(srv, stopBG)

	go func() {
		// 一行结构化摘要：加密状态、数据路径、号池规模这些排障第一眼看的东西
		// 之前散在多行中文日志里，机器采集只能靠正则。JSON 行可直接 grep/jq。
		total, ready := st.AccountCounts()
		summary, _ := json.Marshal(map[string]any{
			"event":         "startup",
			"version":       version,
			"listen":        addr,
			"data":          st.Path(),
			"encrypted":     st.Encrypted(),
			"accounts":      total,
			"ready":         ready,
			"keys":          len(st.Keys()),
			"models":        len(st.Models()),
			"default_model": st.DefaultModel(),
			"machine_ready": st.MachineID() != "",
		})
		log.Printf("startup %s", summary)

		log.Printf("DeepAICode2API %s 已启动: http://%s/", version, addr)
		if st.HasKeys() {
			log.Printf("已签发 API 密钥，调用需携带 Authorization: Bearer sk-...")
		} else {
			log.Printf("尚未签发 API 密钥：仅本机回环可无密钥调用，非回环来源返回 503（请在控制台创建）")
		}
		if st.Settings().AdminPassword != "" {
			log.Printf("管理接口已启用密码校验（X-Admin-Password / Bearer）")
		} else {
			log.Printf("未设置管理密码，管理接口仅允许本机访问（可用 -admin-password 设置）")
		}
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("监听失败: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	close(stopBG)
	log.Printf("正在关闭…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("关闭异常: %v", err)
	}
	if err := st.Save(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}
	log.Printf("已退出")
}

// background 负责首个模型目录拉取、定期持久化与周期性目录刷新。
//
// 首个拉取放在这里而不是 main() 里同步执行：上游不通时 RefreshModels 要等满
// 20s 超时（实测日志里 "机器指纹" 到 "已启动" 正好差 20 秒），同步做会把
// 「开始监听」整体拖后。目录为空时 /v1/models 与模型解析都回落到内置兜底
// 条目（server.catalog / registry.Resolve），所以先监听、后补目录是安全的。
func background(st *store.Store, srv *server.Server, stop <-chan struct{}) {
	// 首个目录拉取：与监听并行，失败只影响模型清单，不影响服务可用。
	// 用独立 ctx：即使 stop 关闭也让它自己超时结束，不阻塞退出流程。
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		models, err := srv.RefreshModels(ctx)
		cancel()
		if err != nil {
			log.Printf("拉取模型目录失败（先用缓存/兜底）: %v", err)
		} else {
			log.Printf("模型目录已更新：%d 个模型，默认 %s", len(models), st.DefaultModel())
		}
	}

	saveTick := time.NewTicker(30 * time.Second)
	defer saveTick.Stop()
	modelTick := time.NewTicker(6 * time.Hour)
	defer modelTick.Stop()

	for {
		select {
		case <-stop:
			return
		case <-saveTick.C:
			if err := st.Save(); err != nil {
				log.Printf("定期保存状态失败: %v", err)
			}
		case <-modelTick.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			models, err := srv.RefreshModels(ctx)
			cancel()
			if err != nil {
				log.Printf("定期刷新模型目录失败: %v", err)
			} else {
				log.Printf("模型目录已刷新：%d 个模型", len(models))
			}
		}
	}
}

// quotaGuard 是后台额度守护：按 refresh_at / 陈旧度分批复探，
// 不再需要人工点「刷新全部配额」（653 号全量探测要 11 分钟）。
//
// 单独一个 goroutine 而不是塞进 background：上游挂住时一批要几分钟
// （24 个 / 4 并发 × 20s 超时），会拖住 30s 的周期保存。
func quotaGuard(srv *server.Server, stop <-chan struct{}) {
	tick := time.NewTicker(server.QuotaGuardInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			// 出口回切要先做：额度刷新后把账号切回最快的直连，
			// 之后的复探才会走新出口（否则快照还是旧出口的）。
			if n := srv.RestoreEgress(); n > 0 {
				log.Printf("[egress] 后台已回切 %d 个账号的出口", n)
			}
			if n := srv.ReprobeStale(server.QuotaGuardBatch); n > 0 {
				log.Printf("[quota] 后台已复探 %d 个账号的额度", n)
			}
		}
	}
}

// autoImport 在启动时尝试导入本机桌面端凭据。
func autoImport(st *store.Store, dir string) {
	results, err := deepai.ImportAllFromDesktop(dir)
	if err != nil {
		log.Printf("未自动导入桌面端凭据（可在控制台手动导入）: %v", err)
		return
	}
	added, updated := 0, 0
	for _, res := range results {
		if st.UpsertAccount(res.Account) {
			added++
		} else {
			updated++
		}
	}
	if err := st.Save(); err != nil {
		log.Printf("保存账号失败: %v", err)
		return
	}
	for _, res := range results {
		log.Printf("已导入账号 %s（%s，username=%s）", res.Account.Name, res.Path, res.Account.Username)
	}
	log.Printf("桌面端凭据导入完成：新增 %d，更新 %d", added, updated)
}

func runImport(st *store.Store, dir string) {
	results, err := deepai.ImportAllFromDesktop(dir)
	if err != nil {
		log.Fatalf("导入失败: %v", err)
	}
	added, updated := 0, 0
	for _, res := range results {
		isNew := st.UpsertAccount(res.Account)
		state := "更新"
		if isNew {
			state = "新增"
			added++
		} else {
			updated++
		}
		fmt.Printf("%s账号 %s\n凭据文件: %s\nusername: %s\nKey: %s\n\n",
			state, res.Account.Name, res.Path, res.Account.Username, deepai.MaskSecret(res.Account.APIKey))
	}
	if err := st.Save(); err != nil {
		log.Fatalf("保存失败: %v", err)
	}
	fmt.Printf("共导入 %d 个账号（新增 %d，更新 %d）→ %s\n", len(results), added, updated, st.Path())
}

// runSeal 执行一次性存量封存：把主文件、统计与日志里的明文全部改写为密文。
//
// 与普通 Save 的区别：Save 只重写「之后变化」的内容，append-only 的日志
// 不会被回填，所以需要单独的 -seal 入口。
func runSeal(st *store.Store, yes bool) {
	if !st.MasterKeySet() {
		log.Fatalf("未设置 %s：先写入 .env 或导出环境变量（openssl rand -hex 32）\n"+
			"  注意：封存后密钥丢失即号池不可恢复。", store.EnvMasterKey)
	}
	if !yes {
		log.Fatalf("将把 %s 及其统计/日志旁路文件改写为密文（不可逆）。\n"+
			"  请先停止正在运行的网关，再执行：-seal -yes\n  密钥指纹: %s",
			st.Path(), st.MasterKeyFingerprint())
	}
	rep, err := st.Reseal()
	if err != nil {
		log.Fatalf("封存失败: %v", err)
	}
	fmt.Printf("已封存 %d 个文件（账号 %d，历史明文日志行 %d，原样保留 %d）\n",
		len(rep.Files), rep.Accounts, rep.SealedLines, rep.KeptLines)
	for _, f := range rep.Files {
		fmt.Printf("  - %s\n", f)
	}
	fmt.Printf("密钥指纹: %s\n", st.MasterKeyFingerprint())
	fmt.Printf("⚠ 务必备份 %s：丢失后号池无法解密。\n", store.EnvMasterKey)
}

// defaultDataPath 返回默认状态文件路径。
//
// 优先使用用户私有配置目录（Windows 上是 %AppData%，Unix 上是 ~/.config）：
// 状态文件含上游 Key 与账号密码，放在可执行文件同目录时往往会落在
// 共享目录（Program Files、共享盘）里，同机任意用户都能读到。
//
// 为不打断既有部署：如果旧位置（可执行文件同目录）已经有状态文件，继续用它，
// 否则号池看起来会“凭空消失”。
func defaultDataPath() string {
	const name = "deepaicode2api-data.json"

	if exe, err := os.Executable(); err == nil {
		legacy := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "deepaicode2api", name)
	}
	return name
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// envDuration 读取时长型环境变量（time.ParseDuration 语法，如 15s / 2m）。
func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// isLoopbackListen 判断监听地址是否只接受本机连接。
//
// 空串 / 0.0.0.0 / :: 都表示监听全部网卡，属于对外暴露，不算回环。
func isLoopbackListen(host string) bool {
	host = strings.TrimSpace(host)
	switch strings.ToLower(host) {
	case "", "0.0.0.0", "::", "[::]":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
