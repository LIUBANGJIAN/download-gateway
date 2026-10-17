//! `download-gateway` 入口：**双端口 + SQLite 单写者**。
//!
//! 启动顺序（顺序不可调换）：
//!
//! ```text
//! 读配置 → open_connection() → migrate::apply() → Store::from_connection()
//!        → 起两个 axum listener（6800 对外 / 8080 管理）→ 等信号 → 关停 actor
//! ```
//!
//! 之所以**先迁移再起 actor**：迁移要用 `&mut Connection` 开事务，
//! 而连接一旦交给 actor 线程就不再可变。
//!
//! 端口隔离见 `02 §1.3` / `02 §4.4`：对外口可能公网可达，管理口**默认仅内网**。
//!
//! # 本轮增量（T08α/T09α/T10α）
//!
//! 本文件退化为**纯装配**：业务路由由 `dispatch-core` 的 `ingress`/`admin` 提供，
//! `/healthz` 仍由本文件拥有（契约逐字节不变）。两个 listener 都需 `ConnectInfo`
//! （管理口按来源 IP 防爆破登录；对外口 `/api/webui/login` 按来源 IP 限速）。

mod config;

use std::io::IsTerminal;
use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{Context, Result};
use axum::extract::State;
use axum::response::Response;
use axum::routing::get;
use dispatch_core::state::{AdminState, EnvSnapshot, HandshakeState, PublicState};
use dispatch_core::store::{self, Store, migrate};
use tokio::net::TcpListener;
use tokio::sync::mpsc;
use tracing_subscriber::EnvFilter;

use crate::config::Config;

/// 对外口 `/healthz` 薄包装（契约与重构前逐字节一致）。
async fn healthz_public(State(s): State<Arc<PublicState>>) -> Response {
    dispatch_core::health::health_body(&s.store, s.started_at, "public").await
}

/// 管理口 `/healthz` 薄包装。
async fn healthz_admin(State(s): State<Arc<AdminState>>) -> Response {
    dispatch_core::health::health_body(&s.store, s.started_at, "admin").await
}

fn init_tracing(level: &str) {
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new(level));
    // ⚠️ ANSI 只给**终端**。
    //
    // `tracing_subscriber::fmt()` 的 `ansi` 默认是 `true`，它不看输出端是不是终端 ——
    // 于是重定向到文件/管道时也会写转义码。后果不是「难看」而是**取不到东西**：
    // 一行日志实际是 `temp_password` + `ESC[0mESC[2m` + `=` + `ESC[0m` + 口令，
    // 用户按 README 敲 `grep temp_password=` 会**一条都匹配不到**（`docker logs` 同理）。
    // 这也是所有「靠 grep 日志做运维」的工具都会踩的坑，统一在入口关掉。
    tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_ansi(std::io::stdout().is_terminal())
        .init();
}

/// 解析代理自身的稳定 client_id（走 `bitcomet_api::clientid` 文件机制，不落库）。
fn resolve_proxy_client_id() -> String {
    let path = bitcomet_api::clientid::default_client_id_path();
    match bitcomet_api::clientid::load_or_create_client_id(&path) {
        Ok(id) => id,
        Err(e) => {
            let id = bitcomet_api::clientid::new_uuid();
            tracing::warn!(error = %e, path = %path.display(),
                "client_id 文件读写失败，本次使用临时 client_id（重启会变）");
            id
        }
    }
}

/// 监听地址是否指向回环。
fn is_loopback(addr: &str) -> bool {
    let host = addr.rsplit_once(':').map(|(h, _)| h).unwrap_or(addr);
    let host = host.trim_matches(['[', ']']);
    matches!(host, "127.0.0.1" | "localhost" | "::1")
}

/// 重置管理口令的子命令名（与 Go 侧 `ResetPasswordCmd` 逐字一致）。
const RESET_PASSWORD_CMD: &str = "reset-password";

/// 无子命令时的总用法（`download-gateway help` / `--help`）。
const MAIN_USAGE: &str = "\
用法：
  download-gateway                    启动服务（读环境变量，监听对外口与管理口）
  download-gateway reset-password     重置管理口令（忘记口令时的补救路径，详见 --help）
  download-gateway help               显示本说明
";

/// `reset-password --help` 的用法说明。
const RESET_USAGE: &str = "\
用法：
  download-gateway reset-password                    生成随机口令并写入数据库（推荐）
  download-gateway reset-password --password <新口令>  指定新口令（10–128 个字符）
  download-gateway reset-password --clear             清除口令，回退到 DISPATCH_ADMIN_PASSWORD / 启动时随机口令

说明：
  口令只写入 DISPATCH_DB 指向的数据库（默认 data/dispatch.db），不打印旧口令、也无法反推旧口令。
  命令本身不会重启服务；**改完必须重启**，运行中的进程才会重新读取口令。
";

/// `reset-password` 子命令：**忘记管理口令时的唯一合法补救路径**。
///
/// # 为什么必须有它
///
/// 口令一旦在管理台改过，库里留下的是 argon2id 哈希，启动时 [`main`] 读到它就会
/// 明确不再校验 `DISPATCH_ADMIN_PASSWORD`。哈希不可逆 —— 忘了就是死锁：
/// 登不进去、改不了、连界面上的「改口令」卡片都够不着。
///
/// 在此之前唯一的土办法是拿外部工具改 `dispatch.db`：运行镜像基于 alpine、
/// 以 uid 10001 运行、**不带 sqlite3**，于是「拷出来 → 找台有 sqlite 的机器 →
/// 删两行 → 拷回去」成了唯一出路，或者删库重来（任务、节点、审计全丢）。
///
/// # 为什么是子命令而不是界面上的「忘记密码」
///
/// 自托管网关没有邮箱、没有短信、没有第二个管理员 —— 能证明「你是主人」的东西
/// 只剩下**宿主机控制权**。能 `docker exec` / 能改 compose 的人本来就能改库、换镜像、
/// 删卷，所以把恢复能力放在这里不降低安全性；反过来，任何不依赖宿主机控制权的
/// 自助恢复（密保问题之类）都会额外长出一个可被攻击的面。
///
/// # ⚠️ 生效需要重启
///
/// 本命令直接改库，**不通知**正在运行的服务进程 —— 那个进程的凭据在内存里，
/// 不会察觉库变了。所以输出末尾必须明确要求重启；不写这句，用户会以为命令没生效，
/// 然后开始怀疑口令、怀疑大小写、怀疑输入法。
///
/// 行为、参数、输出结构、审计文案与 Go 侧 `resetcmd.go` 保持一致：
/// 两个实现对同一个 `dispatch.db` 必须能互相接管。
async fn reset_password_cmd(cfg: &Config, args: &[String]) -> Result<()> {
    let mut new_password: Option<String> = None;
    let mut clear_only = false;
    let mut i = 0;
    while i < args.len() {
        let arg = args[i].as_str();
        match arg {
            "-h" | "--help" => {
                print!("{}", RESET_USAGE);
                return Ok(());
            }
            "-c" | "--clear" => {
                clear_only = true;
                i += 1;
            }
            "-password" | "--password" => {
                let value = args
                    .get(i + 1)
                    .with_context(|| format!("{arg} 需要一个值"))?;
                new_password = Some(value.clone());
                i += 2;
            }
            _ if arg.starts_with("--password=") => {
                new_password = Some(arg["--password=".len()..].to_string());
                i += 1;
            }
            _ if arg.starts_with("-password=") => {
                new_password = Some(arg["-password=".len()..].to_string());
                i += 1;
            }
            other => anyhow::bail!("无法识别的参数 {other:?}（本命令只接受 --password / --clear）"),
        }
    }

    if clear_only && new_password.is_some() {
        anyhow::bail!("--clear 与 --password 不能同时使用：一个要清空、一个要写入");
    }
    // 格式先校验，**在打开数据库之前** —— 口令太短这种纯输入错误不该产生任何副作用。
    if let Some(pw) = &new_password {
        dispatch_core::admin::password::validate_new(pw, "").map_err(|e| anyhow::anyhow!("{e}"))?;
    }
    // 先看库在不在：`Store::open` 会顺手建库，在「路径打错」的情况下静默造出一个空库，
    // 然后用户以为重置成功了，其实改的是另一个文件。
    if !cfg.db_path.exists() {
        anyhow::bail!(
            "数据库 {} 不存在：请先启动一次服务完成初始化，或检查 DISPATCH_DB",
            cfg.db_path.display()
        );
    }
    let store = Store::open(&cfg.db_path)
        .with_context(|| format!("打开数据库 {} 失败", cfg.db_path.display()))?;
    let now = dispatch_core::ids::now_secs();

    if clear_only {
        dispatch_core::admin::password::clear_hash(&store)
            .await
            .context("清除口令哈希失败")?;
        audit_password_reset(
            &store,
            now,
            "管理口令已通过命令行清除（reset-password --clear）",
        )
        .await;
        println!("已清除数据库 {} 中的管理口令。\n", cfg.db_path.display());
        println!("重启后生效：");
        println!("  · 若设置了 DISPATCH_ADMIN_PASSWORD → 用该口令登录；");
        println!("  · 若未设置 → 启动时随机生成，并打印在启动日志里（每次重启都会变）。");
        println!("\n重启命令：docker restart <容器名>   或   systemctl restart download-gateway");
        return Ok(());
    }

    let (password, generated) = match new_password {
        Some(p) => (p, false),
        // 与启动时的随机口令同源（`ids::new_hex_128` = 16 字节随机 → 32 位小写 hex），
        // 不另造一套字符集：同一个产品里出现两种「随机口令长相」只会让用户更迷惑。
        None => (dispatch_core::ids::new_hex_128(), true),
    };
    let phc = dispatch_core::admin::password::hash_password(&password)
        .map_err(|e| anyhow::anyhow!("{e}"))?;
    dispatch_core::admin::password::save_hash(&store, &phc, now)
        .await
        .context("保存口令哈希失败")?;
    audit_password_reset(&store, now, "管理口令已通过命令行重置（reset-password）").await;

    println!("管理口令已重置。");
    if generated {
        println!("  新口令（随机生成）：{password}");
        println!("  ⚠️ 这串口令只在这里显示一次，请立刻保存到密码管理器。");
    } else {
        println!("  新口令：使用你通过 --password 指定的那一串（本命令不回显）。");
    }
    println!(
        "  写入位置：{}（app_config.{}）\n",
        cfg.db_path.display(),
        dispatch_core::admin::password::KEY_HASH
    );
    println!("⚠️ 正在运行的服务仍持有旧口令：必须重启才生效。");
    println!("   容器部署：docker restart <容器名>");
    println!("   裸机部署：systemctl restart download-gateway");
    println!("   重启后环境变量 DISPATCH_ADMIN_PASSWORD 不再参与校验（库里已有新口令）。");
    Ok(())
}

/// 写一条「口令被离线改过」的安全审计。
///
/// 具体 SQL 放在 `dispatch-core` 里（[`dispatch_core::admin::password::log_security_audit`]），
/// 根 crate 不为一条 INSERT 去依赖 `rusqlite`。
async fn audit_password_reset(store: &Store, now: i64, message: &str) {
    dispatch_core::admin::password::log_security_audit(store, now, message, "warn").await;
}

#[tokio::main]
async fn main() -> Result<()> {
    let cfg = Config::from_env()?;
    init_tracing(&cfg.log_level);

    // ── 子命令分流 ────────────────────────────────────────────────────────
    // `reset-password` 只碰数据库、不绑端口，必须在起监听之前处理。
    // 未知子命令**直接报错**而不是忽略：手误（如 `restar`）会静默起一个抢端口的服务，
    // 排查成本远高于一句报错。
    let argv: Vec<String> = std::env::args().skip(1).collect();
    if let Some(first) = argv.first() {
        match first.as_str() {
            RESET_PASSWORD_CMD => return reset_password_cmd(&cfg, &argv[1..]).await,
            "-h" | "--help" | "help" => {
                print!("{}", MAIN_USAGE);
                return Ok(());
            }
            other => {
                eprintln!("download-gateway: 未知子命令 {other:?}\n\n{MAIN_USAGE}");
                std::process::exit(2);
            }
        }
    }

    tracing::info!(
        version = dispatch_core::VERSION,
        public = %cfg.public_addr,
        admin = %cfg.admin_addr,
        db = %cfg.db_path.display(),
        migrations = %cfg.migrations_dir.display(),
        "download-gateway 启动中"
    );

    // ── 安全告警（启动即显式告知风险，不静默）──────────────────────────────
    tracing::warn!("调度器与节点下发未实现（T03–T07 未落地）：任务只会入库排队，不会真正下载");
    if cfg.public_token.is_none() {
        tracing::warn!(
            addr = %cfg.public_addr,
            "对外口未配置 DISPATCH_PUBLIC_TOKEN：6800 无鉴权，任何可达者都能投递任务"
        );
    }
    if cfg.public_cors && cfg.public_token.is_none() {
        tracing::warn!(
            "已开启 CORS 但 DISPATCH_PUBLIC_TOKEN 未配置：任意网页可向内网投任务（drive-by）"
        );
    }

    // ── 管理口令 ─────────────────────────────────────────────────────────
    let admin_policy = Arc::new(config::resolve_admin_policy(&cfg));
    if admin_policy.generated {
        tracing::warn!(
            password = %admin_policy.password,
            "DISPATCH_ADMIN_PASSWORD 未设置：已生成随机管理口令（仅本次启动有效，重启会变）"
        );
    }
    if !is_loopback(&cfg.admin_addr)
        && admin_policy.cookie_secure != dispatch_core::state::CookieSecureMode::Always
    {
        tracing::warn!(
            addr = %cfg.admin_addr,
            "会话 Cookie 未强制 Secure，明文传输；请置于 TLS/反代之后，或勿将管理口暴露到不可信网络"
        );
    }

    // ── 数据库 ───────────────────────────────────────────────────────────
    let mut conn = store::open_connection(&cfg.db_path).context("打开 SQLite 失败")?;
    let applied = migrate::apply(&mut conn, &cfg.migrations_dir).context("执行迁移失败")?;
    let versions = migrate::applied_versions(&conn).context("读取迁移版本失败")?;
    tracing::info!(applied, versions = ?versions, "迁移完成");

    let store = Store::from_connection(conn);
    let now = dispatch_core::ids::now_secs();

    // ── 节点密码主密钥 ───────────────────────────────────────────────────
    // 取值顺序：DISPATCH_SECRET_KEY → <库目录>/node-secret.key → 生成并落盘。
    // 生成的密钥**必须**落在持久化卷里，否则重启后已存节点的密码再也回看不了。
    let (secrets, key_generated) = dispatch_core::secret::SecretBox::resolve(&cfg.db_path)
        .context("初始化节点密码主密钥失败")?;
    if key_generated {
        tracing::warn!(
            file = %dispatch_core::secret::key_file_path(&cfg.db_path).display(),
            "DISPATCH_SECRET_KEY 未设置：已生成节点密码主密钥并落盘。\
             该文件与数据库同等重要 —— 丢了就再也回看不了已保存的节点密码（需重新填写）。\
             容器部署请确认它位于持久化卷内。"
        );
    }
    let secrets = Arc::new(secrets);

    // ── 两端口状态 ───────────────────────────────────────────────────────
    let handshake = HandshakeState {
        proxy_client_id: resolve_proxy_client_id(),
        tokens: dispatch_core::ingress::handshake::HandshakeStore::new(),
    };
    let public_state = PublicState::new(
        store.clone(),
        now,
        cfg.public_token.clone(),
        cfg.public_cors,
        handshake,
    );

    let env = EnvSnapshot {
        public_addr: cfg.public_addr.clone(),
        admin_addr: cfg.admin_addr.clone(),
        db_path: cfg.db_path.display().to_string(),
        migrations_dir: cfg.migrations_dir.display().to_string(),
        log_level: cfg.log_level.clone(),
        state_dir: std::env::var("DISPATCH_STATE_DIR").ok(),
        web_dir: cfg.web_dir.as_ref().map(|p| p.display().to_string()),
        admin_password_configured: !admin_policy.generated,
        public_token_configured: cfg.public_token.is_some(),
        allow_file_delete: cfg.allow_file_delete,
        public_cors: cfg.public_cors,
        admin_cookie_secure: admin_policy.cookie_secure.as_str(),
    };
    let admin_state = AdminState::new(
        store.clone(),
        now,
        admin_policy,
        env,
        secrets,
        cfg.web_dir.clone(),
    );

    // 管理口令的**实际来源**：库里用户自设的 argon2id 哈希 > 环境变量 > 启动时随机生成。
    // 只在这里读一次；之后「在管理台改口令」走 `set_credential` 热替换，无需重启。
    // 注意顺序不能反：若先信任环境变量，用户改过的口令会被部署参数悄悄覆盖回去。
    if let Some(phc) = dispatch_core::admin::password::load_hash(&store).await {
        tracing::info!("检测到已在管理台设置的口令，环境变量中的初始口令不再参与校验");
        // `store` 与 admin_state 里的那份是同一条连接池，读到的就是同一个库。
        admin_state.set_credential(dispatch_core::state::AdminCredential::Hash(phc));
    }
    // 一句兜底提示：口令一旦改过就无法从库反推，忘了只能走这两条路。
    // 放在启动日志里（而不是界面里），是因为「忘记口令的人根本进不了界面」。
    // 首选轻的那条（登录页按钮 + 本日志取码），`reset-password` 是拿不到日志时的兜底。
    tracing::info!(
        "忘记管理口令时：登录页「忘记口令？」可生成一次性临时口令（只写在本日志里，\
         用 `docker logs <容器名> | grep temp_password` 取）；\
         日志拿不到或在裸机上，用 `download-gateway reset-password`（改完需重启）"
    );

    // ── 组装（业务路由来自 dispatch-core；/healthz 仍由 main 拥有）─────────
    let public_app = dispatch_core::ingress::router()
        .route("/healthz", get(healthz_public))
        .with_state(public_state.clone())
        // CORS 需 `from_fn_with_state`（`from_fn` 不支持 State 提取器）。
        .layer(axum::middleware::from_fn_with_state(
            public_state.clone(),
            dispatch_core::ingress::cors_layer,
        ));
    let admin_app = dispatch_core::admin::router()
        .route("/healthz", get(healthz_admin))
        .with_state(admin_state);

    let public_listener = TcpListener::bind(&cfg.public_addr)
        .await
        .with_context(|| format!("绑定对外端口 {} 失败", cfg.public_addr))?;
    let admin_listener = TcpListener::bind(&cfg.admin_addr)
        .await
        .with_context(|| format!("绑定管理端口 {} 失败", cfg.admin_addr))?;

    tracing::info!(
        "对外端口监听 {}；管理端口监听 {}",
        cfg.public_addr,
        cfg.admin_addr
    );

    let (tx, mut rx) = mpsc::channel::<String>(4);

    let tx_public = tx.clone();
    let h_public = tokio::spawn(async move {
        // 对外口也需 ConnectInfo：`/api/webui/login` 按来源 IP 限速依赖它。
        if let Err(e) = axum::serve(
            public_listener,
            public_app.into_make_service_with_connect_info::<SocketAddr>(),
        )
        .await
        {
            let _ = tx_public.send(format!("对外端口异常退出: {e}")).await;
        }
    });
    let tx_admin = tx.clone();
    let h_admin = tokio::spawn(async move {
        // 管理口需 ConnectInfo：登录防爆破按来源 IP。
        if let Err(e) = axum::serve(
            admin_listener,
            admin_app.into_make_service_with_connect_info::<SocketAddr>(),
        )
        .await
        {
            let _ = tx_admin.send(format!("管理端口异常退出: {e}")).await;
        }
    });

    tokio::select! {
        r = tokio::signal::ctrl_c() => {
            match r {
                Ok(()) => tracing::info!("收到中断信号，开始优雅退出"),
                Err(e) => tracing::warn!(error = %e, "信号监听异常，转入退出流程"),
            }
        }
        Some(msg) = rx.recv() => {
            tracing::error!(%msg, "listener 异常退出，进程准备退出");
        }
    }

    store.shutdown().await;
    h_public.abort();
    h_admin.abort();
    tracing::info!("已退出");
    Ok(())
}
