// Command download-gateway 是入口：**双端口 + SQLite 单写者** 的纯装配。
//
// # 启动顺序（与 Rust `src/main.rs` 逐字对齐，顺序不可换，见 design/12 §1.6）
//
//	读 config(环境变量)
//	 → 安全告警（无 token / 口令随机 / 密钥生成 / Cookie 非 Secure）
//	 → Store.Open(dbPath)         // 内部：建父目录 + PRAGMA → migrate.Apply → 单写者写池
//	 → SecretBox.Resolve(dbPath)  // DISPATCH_SECRET_KEY → node-secret.key → 生成落盘
//	 → 构造 PublicState / AdminState（含握手表、会话表、守卫）
//	 → 起 :6800 http.Server / :8080 http.Server
//	 → 等中断信号 → Shutdown ×2 → db.Close()
//
// 【为何先迁移再构造写池】迁移要开事务、需独占一条连接；一旦写池被限制为单连接，
// 迁移持有那条连接时任何二次取连接都会死锁。故顺序被**封装在 `store.Open` 内部**
// （`open_connection → migrate::apply → from_connection`），调用者无从搞反。
//
// # 本轮接线范围
//
// 本文件不再只是骨架：`internal/ingress`（对外 aria2 / BitComet 协议面）与
// `internal/admin`（管理 REST + 壳页面）已经装配进来，`internal/tasks` 由此**真正接入**
// —— 任务可以从 :6800 入库、在 :8080 看到与干预。仍未落地的部分（调度内核、节点写操作、
// 完整三段式握手）已在启动日志与相应包注释里显式标注，不静默假装可用。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/admin"
	"github.com/LIUBANGJIAN/download-gateway/internal/adminpass"
	"github.com/LIUBANGJIAN/download-gateway/internal/config"
	"github.com/LIUBANGJIAN/download-gateway/internal/engine"
	"github.com/LIUBANGJIAN/download-gateway/internal/handshake"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/ingress"
	"github.com/LIUBANGJIAN/download-gateway/internal/secret"
	"github.com/LIUBANGJIAN/download-gateway/internal/state"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// version 是本版本号。CI 构建时由 `-ldflags "-X main.version=$APP_VERSION"` **在编译期**
// 注入；本地开发 / 未注入时退回默认值。对齐 Rust 侧 `dispatch-core::VERSION` 的注入链
// （`option_env!("APP_VERSION")`，见 `crates/dispatch-core/src/lib.rs`）。
//
// ⚠️ **此处无法取得金样（如实声明，勿在后续据此判"弱"）**：单测**只能断言值的形态**
// （三段 X.Y.Z），**无法**在单测层证伪注入链本身 —— 它真正的守卫在端到端
// （容器 `/healthz` 自报版本 == 本次应有版本）。Rust 侧 `lib.rs` 明确记录了这一点，
// 并**刻意删掉**了曾存在的恒真用例，Go 版照此，不重复那个错误。
var version = "0.1.0"

// EnvRustLog 是 Rust 侧日志过滤器的标准环境变量名（仅支持**纯级别**形式，见 [effectiveLogLevel]）。
const EnvRustLog = "RUST_LOG"

// shutdownTimeout 是两端口优雅退出的等待上限。
const shutdownTimeout = 10 * time.Second

func main() {
	// 子命令优先于「启动服务」：`reset-password` 是忘记口令时的人工补救路径，
	// 它只碰数据库、不绑端口，所以必须在 run() 读配置、起监听之前分流。
	//
	// 未知子命令直接报错而不是忽略：`download-gateway restar` 这种手误会静默启动一个
	// 抢端口的服务，排查起来比一句报错贵得多。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case ResetPasswordCmd:
			if err := runResetPassword(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "download-gateway %s: %v\n", ResetPasswordCmd, err)
				os.Exit(1)
			}
			return
		case "-h", "--help", "help":
			fmt.Fprint(os.Stdout, mainUsage)
			return
		default:
			fmt.Fprintf(os.Stderr, "download-gateway: 未知子命令 %q\n\n%s", os.Args[1], mainUsage)
			os.Exit(2)
		}
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "download-gateway: %v\n", err)
		os.Exit(1)
	}
}

// mainUsage 是无参数子命令时的总用法（`download-gateway help` / `--help`）。
const mainUsage = `用法：
  download-gateway                    启动服务（读环境变量，监听对外口与管理口）
  download-gateway reset-password     重置管理口令（忘记口令时的补救路径，详见 -h）
  download-gateway help               显示本说明
`

// run 是真正的启动流程（便于测试与 `main` 分离）。
func run() error {
	// ── 配置 ─────────────────────────────────────────────────────────────
	cfg := config.FromEnv()
	initLogging(effectiveLogLevel(cfg.LogLevel))

	slog.Info("download-gateway 启动中",
		"version", version,
		"public", cfg.PublicAddr,
		"admin", cfg.AdminAddr,
		"db", cfg.DBPath,
		"migrations", cfg.MigrationsDir,
	)

	// ── 安全告警（启动即显式告知风险，不静默）──────────────────────────────
	// 调度内核已落地（B1）。这里不再告警"不会下载"，改为如实报告开关状态 ——
	// 用户最需要的不是"做完了"，而是"现在会不会真的下载"。
	if !engine.SchedulerEnabledByEnv() {
		slog.Warn("调度内核已被环境变量 DISPATCH_SCHEDULER 关闭：任务只会入库排队，不会下发")
	} else {
		slog.Info("调度内核已启用：排队任务会按亲和/评分选点并投递到 BitComet 节点；" +
			"可用 DISPATCH_SCHEDULER=off 或 app_config.scheduler.enabled='off' 关闭")
	}
	if cfg.PublicToken == nil {
		slog.Warn("对外口未配置 DISPATCH_PUBLIC_TOKEN：6800 无鉴权，任何可达者都能投递任务",
			"addr", cfg.PublicAddr)
	}
	if cfg.PublicCORS && cfg.PublicToken == nil {
		slog.Warn("已开启 CORS 但 DISPATCH_PUBLIC_TOKEN 未配置：任意网页可向内网投任务（drive-by）")
	}

	// ── 管理口令 ─────────────────────────────────────────────────────────
	adminPolicy := config.ResolveAdminPolicy(cfg)
	if adminPolicy.Generated {
		slog.Warn("DISPATCH_ADMIN_PASSWORD 未设置：已生成随机管理口令（仅本次启动有效，重启会变）",
			"password", adminPolicy.Password)
	}
	if !isLoopback(cfg.AdminAddr) && adminPolicy.CookieSecure != config.CookieSecureAlways {
		slog.Warn("会话 Cookie 未强制 Secure，明文传输；请置于 TLS/反代之后，或勿将管理口暴露到不可信网络",
			"addr", cfg.AdminAddr)
	}

	// ── 数据库（open → migrate → 写池，顺序封装在 store.Open 内）───────────
	st, err := store.Open(cfg.DBPath, cfg.MigrationsDir)
	if err != nil {
		return fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	// 先关 listener（下方 Shutdown）再关 store —— defer 保证在 return 前最后执行。
	defer func() { _ = st.Close() }()

	now := ids.NowSecs()

	// ── 节点密码主密钥 ───────────────────────────────────────────────────
	// 取值顺序：DISPATCH_SECRET_KEY → <库目录>/node-secret.key → 生成并落盘。
	// 生成的密钥**必须**落在持久化卷里，否则重启后已存节点的密码再也回看不了。
	secrets, keyGenerated, err := secret.Resolve(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("初始化节点密码主密钥失败: %w", err)
	}
	if keyGenerated {
		slog.Warn("DISPATCH_SECRET_KEY 未设置：已生成节点密码主密钥并落盘。"+
			"该文件与数据库同等重要 —— 丢了就再也回看不了已保存的节点密码（需重新填写）。"+
			"容器部署请确认它位于持久化卷内。",
			"file", secret.KeyFilePath(cfg.DBPath))
	}

	// ── 两端口状态 ───────────────────────────────────────────────────────
	// 握手令牌表用叶子包 `internal/handshake` 的真实类型；代理自身 client_id 仍待
	// T02/T04 的 bitcomet clientid 文件机制接入。会话表/守卫由 NewAdminState 内部构造。
	hs := state.HandshakeState{
		ProxyClientID: "",                   // T04: bitcomet clientid 文件机制
		Tokens:        handshake.NewStore(), // 叶子包 internal/handshake
	}
	publicState := state.NewPublicState(st, now, cfg.PublicToken, cfg.PublicCORS, hs)

	env := config.EnvSnapshot{
		PublicAddr:              cfg.PublicAddr,
		AdminAddr:               cfg.AdminAddr,
		DBPath:                  cfg.DBPath,
		MigrationsDir:           cfg.MigrationsDir,
		LogLevel:                cfg.LogLevel,
		StateDir:                stateDirFromEnv(),
		WebDir:                  cfg.WebDir,
		AdminPasswordConfigured: !adminPolicy.Generated,
		PublicTokenConfigured:   cfg.PublicToken != nil,
		AllowFileDelete:         cfg.AllowFileDelete,
		PublicCORS:              cfg.PublicCORS,
		AdminCookieSecure:       adminPolicy.CookieSecure,
	}
	adminState := state.NewAdminState(st, now, &adminPolicy, env, secrets, cfg.WebDir)

	// 管理口令的**实际来源**：库里用户自设的 argon2id 哈希 > 环境变量 > 启动时随机生成。
	// 只在这里读一次；之后「在管理台改口令」走 `Credential.SetHash` 热替换，无需重启。
	//
	// 顺序不能反：若先信任环境变量，用户改过的口令会被部署参数悄悄覆盖回去 ——
	// 症状是「改成功了，重启后又变回旧口令」，而日志里一个字都不会提。
	if phc, found := adminpass.LoadHash(context.Background(), st); found {
		slog.Info("检测到已在管理台设置的口令，环境变量中的初始口令不再参与校验")
		adminState.Credential.SetHash(phc)
	}
	// 一句兜底提示：口令一旦改过就无法从库反推。两条补救路径都写在这里，
	// 因为「忘记口令的人根本进不了界面」—— 提示只能出现在日志和文档里。
	slog.Info("忘记管理口令时：登录页「忘记口令？」可生成一次性临时口令（只写在本日志里，" +
		"用 `docker logs <容器名> | grep temp_password` 取）；" +
		"日志拿不到或在裸机上，用 `download-gateway reset-password`（改完需重启）")

	// ── 路由装配 ─────────────────────────────────────────────────────────
	// 对外口：aria2 JSON-RPC / BitComet 8 条复刻路径 / 三段式握手第一段 / `/healthz`。
	// 管理口：登录会话 / 任务只读与干预 / 节点只读 / 策略配置 / 管理台壳页面。
	// 两条链路的 handler 都直接使用上面构造的 state，**没有**第二份状态。
	publicHandler := ingress.NewRouter(publicState, version)
	adminHandler := admin.NewRouter(adminState, version)

	// ── 调度内核（B1：选点 + 投递 + 回读）──────────────────────────────────
	// 它必须在两端口起来**之后**启动：任务来源是对外口，先有入口再谈消费。
	// ctx 在下方与信号绑定，这里先用一个可取消的父 ctx，取消点与 HTTP 一致。
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	eng := engine.New(st, secrets, engine.DefaultConfig())
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		eng.Run(runCtx)
	}()

	publicSrv := &http.Server{Addr: cfg.PublicAddr, Handler: publicHandler}
	adminSrv := &http.Server{Addr: cfg.AdminAddr, Handler: adminHandler}

	// ── 起两端口（各一 goroutine）────────────────────────────────────────
	listenerErr := make(chan error, 2)
	go func() {
		if err := publicSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenerErr <- fmt.Errorf("对外端口异常退出: %w", err)
		}
	}()
	go func() {
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenerErr <- fmt.Errorf("管理端口异常退出: %w", err)
		}
	}()

	slog.Info("对外端口监听中；管理端口监听中", "public", cfg.PublicAddr, "admin", cfg.AdminAddr)

	// ── 等信号 / 等 listener 异常 ────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 信号 ctx 与调度 ctx 同步：先停业务，再停调度，避免"任务正在投递时库已关"。
	go func() {
		<-ctx.Done()
		runCancel()
	}()

	select {
	case <-ctx.Done():
		slog.Info("收到中断信号，开始优雅退出")
	case e := <-listenerErr:
		slog.Error("listener 异常退出，进程准备退出", "err", e)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := publicSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("对外端口关停异常", "err", err)
	}
	if err := adminSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("管理端口关停异常", "err", err)
	}

	// 等调度循环真正退出（它可能在一次投递中，网络超时上限 30s 内必然返回）。
	select {
	case <-engineDone:
	case <-time.After(shutdownTimeout):
		slog.Warn("调度内核未在超时内退出，继续关闭")
	}

	slog.Info("已退出")
	return nil
}

// effectiveLogLevel 解析**生效**日志级别，优先级：DISPATCH_LOG（显式设置）→ RUST_LOG → 配置默认值。
//
// # 与 Rust 的差异（已声明；按 team-lead 裁定，非等价移植）
//
// Rust `init_tracing` 是 `EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new(level))`，
// 其中 `try_from_default_env` 读 `RUST_LOG` ⇒ **Rust 里 RUST_LOG 优先**。本 Go 版按 team-lead
// 裁定 **DISPATCH_LOG 优先**，`RUST_LOG` 仅作回退（项目刚从 Rust 切来，启动脚本里可能留着 RUST_LOG）。
//
// 且只移植**纯级别**形式（`debug`/`info`/`warn`/`error`，大小写不敏感）；Rust 的 EnvFilter
// target 语法（如 `debug,hyper=off`）**不支持** —— 未识别取值由 [parseLevel] 回落 Info。
//
// 之所以不能直接用 `cfg.LogLevel`：`config.envOr(DISPATCH_LOG,"info")` 已把「未设置」塌缩为
// "info"，无法区分「显式设为 info」与「未设置」，故此处需重新 `os.LookupEnv(DISPATCH_LOG)`。
func effectiveLogLevel(cfgLevel string) string {
	if v, ok := os.LookupEnv(config.EnvLog); ok && strings.TrimSpace(v) != "" {
		return v
	}
	if v, ok := os.LookupEnv(EnvRustLog); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return cfgLevel
}

// initLogging 按生效级别初始化标准库 slog 默认 logger。
func initLogging(level string) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(level),
	})))
}

// parseLevel 把日志级别串映射为 slog.Level；未知取值回落 Info。
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// stateDirFromEnv 解析 `DISPATCH_STATE_DIR`。对照 Rust `std::env::var("DISPATCH_STATE_DIR").ok()`：
// **已设置但为空串**也返回非 nil（与 `config.env_opt` 的「空白视为未设置」不同，此处刻意照搬 Rust 行为）。
func stateDirFromEnv() *string {
	if v, ok := os.LookupEnv("DISPATCH_STATE_DIR"); ok {
		return &v
	}
	return nil
}

// isLoopback 判定监听地址是否指向回环。对照 Rust `is_loopback`。
func isLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
