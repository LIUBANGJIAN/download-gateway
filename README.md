# download-gateway

BitComet 下载任务代理分发网关：软件把下载任务推送到代理服务，由代理按负载调度算法
分发到多个 BitComet 服务器执行。

对外同时兼容 **Aria2 JSON-RPC** 与 **BitComet WebUI** 两套协议，
现有下载客户端无需修改即可接入。

## 状态

| 项 | 状态 |
|---|---|
| 设计 | 已闭合（14 表 / 17 显式索引 / 双端口 / 五级去重 / C1–C6 硬约束） |
| 实现 | **T01 工程骨架** ✅ · T02 起待做 |
| CI/CD | 隐私脱敏闸门 → 质量门 → 原生 amd64/arm64 矩阵 → 多架构 manifest → Docker Hub |

## 快速开始

```bash
# 本地运行
cargo run

# 健康检查（两个端口都提供 /healthz）
curl -fsS http://127.0.0.1:8080/healthz
```

默认监听：

- 对外推送端口 `0.0.0.0:6800`（Aria2 / BitComet 客户端接入）
- 管理后台端口 `127.0.0.1:8080`（**默认仅内网**）

容器：

```bash
docker build -t download-gateway .
docker run --rm -p 6800:6800 -p 8080:8080 -v "$PWD/data:/data" download-gateway
```

## 配置

全部经环境变量，无需配置文件即可启动：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `DISPATCH_PUBLIC_ADDR` | `0.0.0.0:6800` | 对外推送端口 |
| `DISPATCH_ADMIN_ADDR` | `127.0.0.1:8080` | 管理后台端口（默认仅内网） |
| `DISPATCH_DB` | `data/dispatch.db` | SQLite 路径（容器内为 `/data/dispatch.db`） |
| `DISPATCH_MIGRATIONS` | `migrations` | 迁移脚本目录 |
| `DISPATCH_LOG` | `info` | 日志级别（可被 `RUST_LOG` 覆盖） |

## 工程结构

```
Cargo.toml              workspace 根（同时是根 package）
src/
  main.rs               双端口入口（6800 对外 / 8080 管理）
  config.rs             环境变量配置
crates/
  dispatch-core/        调度核心：SQLite 单写者 actor + 迁移框架
  bitcomet-api/         BitComet WebUI API 客户端（RNCryptor v3 三段式）
migrations/
  V1__init.sql          14 表 / 17 索引
.github/workflows/
  ci.yml                脱敏闸门 → 质量门 → 原生矩阵 → 多架构 manifest
Dockerfile              多阶段（Rust 构建 + 瘦运行镜像，非 root uid 10001）
```

启动顺序（不可调换）：

```
读配置 → open_connection() → migrate::apply() → Store::from_connection()
       → 起两个 axum listener → 等信号 → 关停 actor
```

先迁移再起 actor，是因为迁移要用 `&mut Connection` 开事务，
而连接一旦交给 actor 线程就不再可变。

## 设计要点

### 为什么是「SQLite 单写者 actor」而不是连接池

写路径全部经一条 `mpsc` 排队，由**唯一一个 OS 线程**持有 `Connection` 顺序执行。
这不是性能优化，而是**正确性前提**：`task_attempt` 与投递必须**同事务**写入，
全局不变量要在权威库上用 SQL 断言验证。从这个架构出发 `SQLITE_BUSY` 不会出现
（有 128 并发写的单测守着），而不是靠 `busy_timeout` 重试去掩盖它。

### 为什么不用 Redis

1. 正确性建立在 SQLite 事务 + 唯一约束 + 单写者上，不变量需要在权威库跑 SQL 断言；
   绑定等待队列是**带状态、要参与事务**的队列，外置即失效。
2. Redis 默认 `appendonly no`，重启会丢任务（静默丢单）；开启持久化则性能优势尽失，
   且引入双套持久化的一致性问题。
3. 性能动机不成立：写入量约 10 行/秒，而 SQLite WAL 可达 1 万行/秒，余量约 1000×；
   真正的瓶颈在对端 BitComet 的限流。
4. 唯一需要外置状态的场景是多实例 HA，但多实例会先摧毁「单写者」这一基石，
   且正确路径是 **SQLite → PostgreSQL**，而不是 +Redis。

### 为什么 TLS 必须用 rustls

`native-tls` 在 Windows 走 schannel、在 Linux 走 OpenSSL。参考实现原本用 schannel，
**在 Docker 里直接不可用**。因此本项目锁定 `reqwest` 的 `rustls-tls`（ring provider），
并在 CI 里加了一道闸门：`Cargo.lock` 中不得出现 `native-tls`。

另注：`reqwest` 0.13 已把该 feature 改名为 `rustls`，本项目刻意锁 0.12 以避免
文档与代码出现两套 feature 名。

## 开发

```bash
cargo fmt --all --check
cargo clippy --all-targets --all-features -- -D warnings
cargo test --all
```

## 许可证

MIT
