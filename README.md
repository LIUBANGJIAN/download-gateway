# download-gateway

BitComet 下载任务代理分发网关：软件把下载任务推送到代理服务，由代理按负载调度算法
分发到多个 BitComet 服务器执行。

对外同时兼容 **Aria2 JSON-RPC** 与 **BitComet WebUI** 两套协议，
现有下载客户端无需修改即可接入。

## 状态

| 项 | 状态 |
|---|---|
| 设计 | 已闭合（14 表 / 17 显式索引 / 双端口 / 五级去重 / C1–C6 硬约束） |
| 实现 | **T01 工程骨架** ✅ · **T02 BitComet 客户端** ✅（三台真节点已验收） · **T08 Aria2 兼容面** ✅ · **T09 BitComet 兼容面 + 自签三段式握手** ✅ · **T05（入库侧）任务建立 / read-after-write / 状态机** ✅ · **T10（后端 + 内嵌台）管理 API / 会话 / Web 管理台** ✅ |
| 未实现 | **T03–T07**：节点健康检查、调度器选点、真正下发、轮询同步、去重 ⇒ **任务只会入库排队，不会真正下载**（启动日志有 `WARN` 明示） |
| CI/CD | ✅ 双架构镜像已在 Docker Hub：`liubangjian/download-gateway` 的 `latest` 与 `sha-<短SHA>` 均含 linux/amd64 + linux/arm64 |
| 部署 | ✅ `docker-compose.yml`（本仓库） |

> ⚠️ **「入库」是当前链路真正的终点。** 两个端口都已按 `02 §4` 的契约对外服务
> （Aria2 JSON-RPC / BitComet WebUI / Web 管理台都能打开、能鉴权、能拿回形状正确的响应），
> 但**没有任何组件会去连 BitComet 节点**：所有任务停在库里的排队态。
> 所以现在可以验证的是**协议面 + 鉴权 + 入库 + 查询**，**不是**完整下载链路。

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

容器（本地构建，仅用于验证 Dockerfile；正式部署见下一节）：

```bash
docker build -t download-gateway .
docker run --rm -p 6800:6800 -p 8080:8080 -v "$PWD/data:/data" download-gateway
```

## 怎么验证「推送接入口」是正常的

下面的命令**不需要任何下载客户端**，直接对着网关打即可。「我推上去了但没反应」这类问题，
先跑这一组，把范围收敛到网关侧还是客户端侧。每行都标了预期响应 —— 对不上就是真有问题，
不是"大概好了"。（示例用默认端口；若按 `docker-compose.yml` 映射成 `"6900:6800"` / `"8989:8080"`，
把命令里的端口换掉。远端部署则把 `127.0.0.1` 换成宿主 IP。）

### 1) 先确认服务活着、库通

```bash
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:6800/healthz
# → {"db":true,"port":"admin"|"public","status":"ok","uptime_seconds":N,"version":"0.1.0"}
# db:false 且 HTTP 503 = 库不通。这时先查数据卷属主，别往下查协议。
```

### 2) 对外口 6800 · Aria2 协议

```bash
# 2a. 投一个任务：立即返回 GID
curl -fsS http://127.0.0.1:6800/jsonrpc -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":"1","method":"aria2.addUri","params":[["magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"]]}'
# → {"id":"1","jsonrpc":"2.0","result":"39347268fb1e6252"}      16 位小写 hex

# 2b. 用上一步的 GID 回查
curl -fsS http://127.0.0.1:6800/jsonrpc -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":"2","method":"aria2.tellStatus","params":["<GID>"]}'
# → status="waiting"、totalLength="0"、totalLengthKnown=false、
#   dir=""、dirKnown=false、dispatchState="queued_no_node"、dispatched=false
#   ↑ 两个"未知"是有意为之：当前没有调度器，编造一个保存路径/大小比返回空更危险。

# 2c. 全局汇总（自聚合，会把你刚投的任务算进去）
curl -fsS http://127.0.0.1:6800/jsonrpc -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":"3","method":"aria2.getGlobalStat","params":[]}'
# → {"numWaiting":"1","numActive":"0","numStopped":"0",...}

# 2d. 批量调用（AriaNg 这类客户端常用）
curl -fsS http://127.0.0.1:6800/jsonrpc -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":"4","method":"system.multicall","params":[[{"methodName":"aria2.getVersion","params":[]},{"methodName":"aria2.getGlobalStat","params":[]}]]}'
# → {"id":"4","jsonrpc":"2.0","result":[[{...getVersion...}],[{...getGlobalStat...}]]}
#   结果是**逐条一元素数组**；某条失败则该位换成 {"code":C,"message":M}，整体仍是 HTTP 200。
#   ⚠️ token **不放外层**：按 aria2 官方手册，system.multicall 的 token 由**每个子调用**
#      自己的 params[0] 携带（外层若也放了，会被剥离后忽略，不会报错）。
```

> 设了 `DISPATCH_PUBLIC_TOKEN` 时，**每条**调用的 `params[0]` 要放 `"token:<值>"`：
> `"params":["token:你的令牌"]`（addUri 则是 `"params":["token:你的令牌",["magnet:..."]]`）。
> 缺失/错误 → `{"error":{"code":-1,"message":"Unauthorized"},...}`。

### 3) 对外口 6800 · BitComet 协议

```bash
# 3a. 没握手就打业务接口 → 必须 401（这是门禁在工作的证据，不是故障）
curl -sS -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:6800/api/task/http/add
# → 401  响应体 {"error_code":"INVALID_TOKEN",...,"platform":"proxy"}

# 3b. 握手第一步不需要凭据
curl -fsS -X POST http://127.0.0.1:6800/api/webui/ip_verify
# → {"bypass_eligible":false,"error_code":"OK","error_message":"",...}
```
后续 `/api/webui/login` → `/api/device_token/get` 的请求体是 **RNCryptor v3 密文**
（AES-256-CBC + PKCS#7 + PBKDF2-HMAC-SHA1×10000 + HMAC-SHA256，口令 = 客户端自己生成的 `client_id`），
**没法用 curl 手搓**。要验证完整握手，请用真实 BitComet 客户端 / 推送插件指向 6800。

### 4) 管理口 8080

```bash
curl -fsS http://127.0.0.1:8080/healthz
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/
# → 200（Web 管理台首页；未登录会跳登录页）
```

> 🔒 **跨端口隔离**：对外口打管理路由、管理口打对外路由，都必须是 **404**。
> 两个 app 的路由表在源码里是分别构造的，不存在"从 6800 摸到管理后台"。
> ```bash
> curl -sS -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:6800/api/admin/tasks   # → 404
> curl -sS -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8080/jsonrpc           # → 404
> ```

## 部署（Docker Compose）

镜像已在 Docker Hub（多架构 manifest，amd64 / arm64 都能直接拉）：

```bash
# 1) 起服务
docker compose up -d

# 2) 等健康
docker compose ps            # STATUS 应为 Up ... (healthy)

# 3) 冒烟：两个端口都应返回 {"status":"ok",...}
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:6800/healthz
```

固定到某个具体构建（可复现）而不是跟着 `latest` 走：

```bash
IMAGE_TAG=sha-eb6a12c docker compose up -d       # 或写进同目录的 .env
```

### 四个必须知道的点

1. **管理口只绑回环。** `docker-compose.yml` 里管理口写的是 `127.0.0.1:8080:8080`，
   而不是 `8080:8080`。镜像内部把 8080 绑在 `0.0.0.0`（Dockerfile 的 `ENV`），
   容器自身收不了口 —— **能不能从外部访问管理面，唯一就由 compose 这一行决定**。
   要在远端看管理台，走 SSH 隧道，不要把 8080 发布到公网。
2. **用了 `stop_signal: SIGINT`。** `main.rs` 的优雅退出只等
   `tokio::signal::ctrl_c()`，而 Unix 下它等价于 `signal(SignalKind::interrupt())`，
   **只监听 SIGINT**。容器里应用就是 PID 1，内核不对 PID 1 套用信号的默认处置，
   于是 `docker stop` 默认发的 SIGTERM 会被**静静忽略**，只能干等满
   `stop_grace_period`（10s）再 SIGKILL，`store.shutdown()` 永远执行不到。
3. **数据都在命名卷 `gateway-data` 里**（`/data`：SQLite 三件套 + client_id）。
   改用宿主目录挂载时要先 `mkdir -p ./data && chown -R 10001:10001 ./data`，
   否则报 `unable to open database file` / `readonly database`（运行用户是 uid 10001）。
4. **把管理口暴露到局域网时，必须先设 `DISPATCH_ADMIN_PASSWORD`。** 镜像里 8080 绑的是
   `0.0.0.0`，所以只要你把 compose 那行写成 `"8989:8080"` 之类，管理台就直接对局域网（乃至公网）开放。
   此时仍不设口令的话，口令每次启动都会重新随机生成、只出现在 `docker logs` 里 —— 表现是
   "登录页反复失败"，并在**连续 5 次失败后被锁定 5 分钟**（设计 `07 §5.1` 的防爆破退避）。
   与之配套的是 `DISPATCH_ADMIN_COOKIE_SECURE`：明文 HTTP 下 `Secure` cookie 不会被浏览器回传，
   默认 `auto` 已经替你绕开了这个坑（见上面配置表里的说明）。**正式做法仍是配 TLS 反代 + 走 `always`。**

## 配置

全部经环境变量，无需配置文件即可启动：

**基础**

| 变量 | 默认值 | 说明 |
|---|---|---|
| `DISPATCH_PUBLIC_ADDR` | `0.0.0.0:6800` | 对外推送端口 |
| `DISPATCH_ADMIN_ADDR` | `127.0.0.1:8080` | 管理后台端口（默认仅内网；**镜像里改成 `0.0.0.0:8080`**） |
| `DISPATCH_DB` | `data/dispatch.db` | SQLite 路径（容器内为 `/data/dispatch.db`） |
| `DISPATCH_MIGRATIONS` | `migrations` | 迁移脚本目录（容器内为 `/opt/download-gateway/migrations`） |
| `DISPATCH_LOG` | `info` | 日志级别（**`RUST_LOG` 存在时优先于它**） |
| `DISPATCH_STATE_DIR` | 未设 | client_id 落盘目录（`clientid.rs` 解析链的最优先项；容器里设 `/data`） |

**对外口（6800）**

| 变量 | 默认值 | 说明 |
|---|---|---|
| `DISPATCH_PUBLIC_TOKEN` | 未设＝**不校验** | 共享令牌。Aria2 面走 `params[0]` 的 `"token:<值>"`；BitComet 面走 `Authorization: Bearer <值>`。**它同时是自签握手解出 `password` 的判据**——设了它，客户端必须拿同一个值当 `password` 才能从 `/api/webui/login` 换到 `invite_token`，否则握手一律失败（这是堵住"自造 client_id 换 device_token 绕过门禁"的承重墙） |
| `DISPATCH_PUBLIC_CORS` | `false` | 是否对 6800 回 `Access-Control-Allow-Origin: *` 并处理 OPTIONS 预检。默认关：开了之后**任何你访问过的网页**都能跨源往这台机器投递任务（drive-by）。开它必须同时设 `DISPATCH_PUBLIC_TOKEN`，否则启动时打 `warn` |

**管理口（8080）**

| 变量 | 默认值 | 说明 |
|---|---|---|
| `DISPATCH_ADMIN_PASSWORD` | 未设＝每次启动**随机生成**并打印到启动日志 | 管理口令。只要管理口离开了回环就必须显式设，否则容器一重启口令就换，你会当成"密码错了"反复重试，然后撞上连续 5 次失败锁 5 分钟 |
| `DISPATCH_ADMIN_COOKIE_SECURE` | `auto` | 会话 Cookie 的 `Secure` 属性，取值 `auto` / `always` / `never`。`auto` ＝ **仅**在请求带 `X-Forwarded-Proto: https` 时加 `Secure`。这是对设计 `07 §5.1`「非 127.0.0.1 即加 Secure」的**刻意偏离**：按原文，在 `http://<局域网IP>:端口` 这种明文形态下浏览器**不会**回传该 cookie ⇒ 登录成功即丢会话 ⇒ 死循环。真正的解法是给管理口配 TLS 反代，然后设 `always` |
| `DISPATCH_WEB_DIR` | 未设＝用**编进二进制的内嵌页面** | 指向磁盘目录以覆盖内嵌页面（改完刷新即生效，不用重编译）。需配合把宿主目录挂进容器 |
| `DISPATCH_ALLOW_FILE_DELETE` | `false` | 是否允许"连节点上的文件一起删"。默认关：删磁盘文件**不可逆**，且节点侧删除能力（T05）尚未落地，现在放开等于让一个没接通的开关可被误触 |

缺失或**全空白**的变量一律回落默认值（`env_or` 把空白也当未设置）；布尔量接受 `1/true/yes/on` 与 `0/false/no/off`（大小写不敏感，无法识别时回落默认）。

> 设计文档 `02 §5.8` 里的 `DISPATCH_MASTER_KEY` / `DISPATCH_NODES_FILE` 属于后续任务，
> **当前源码没有任何读取点**，现在设上不会有任何效果。

## 工程结构

```
Cargo.toml              workspace 根（同时是根 package）
src/
  main.rs               双端口入口（6800 对外 / 8080 管理）
  config.rs             环境变量配置
crates/
  dispatch-core/        核心库：
    src/state.rs          PublicState / AdminState（共享句柄）
    src/tasks/            任务建立 / 查询 / 状态映射（唯一入库入口）
    src/ingress/          对外口 6800：Aria2 兼容面、BitComet 兼容面、自签三段式握手
    src/admin/            管理口 8080：REST + 会话 + 内嵌 Web 管理台
  bitcomet-api/         BitComet WebUI API 客户端（RNCryptor v3 三段式）
migrations/
  V1__init.sql          14 表 / 17 索引
.github/workflows/
  ci.yml                脱敏闸门 → 质量门 → 原生矩阵 → 多架构 manifest
Dockerfile              多阶段（Rust 构建 + 瘦运行镜像，非 root uid 10001）
docker-compose.yml      部署用 Compose（命名卷 / 管理口只绑回环 / stop_signal: SIGINT）
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

## 已知偏差与取舍（留痕）

下面每一条都是**与设计文档字面不同、但经过权衡后刻意如此**的地方 —— 记在这里是为了让后来人
不会把它当成 bug 又"修"回去。设计文档本身含真实内网信息、不随本仓库发布，所以留痕放在这里。

1. **会话 Cookie 的 `Secure` 默认 `auto`**，而设计 `07 §5.1` 原文是「来源非 `127.0.0.1` 即追加 `Secure`」。
   按原文，在 `http://<局域网IP>:端口` 这种明文形态下浏览器**不会回传**该 cookie ⇒ 登录成功即丢会话 ⇒
   死循环。**「能登录」优先于「cookie 属性齐全」**；正式做法是给管理口配 TLS 反代并设 `DISPATCH_ADMIN_COOKIE_SECURE=always`。
2. **CSRF：`Origin` 与 `Sec-Fetch-Site` 两者皆缺时，视为同源放行**（设计 `07 §5.1` 字面要求「校验」）。
   理由是会话 Cookie 已带 `SameSite=Strict` ⇒ 浏览器**跨站**请求根本不会携带 `sid`，CSRF 在结构上已被阻断；
   而强制要求 `Origin` 必然存在会连带封死 `curl` / 脚本运维的正当用法。跨站写请求（必带 `Origin` 或
   `Sec-Fetch-Site: cross-site`）现在**已被 403 拦截**。
3. **`system.multicall` 的外层不校验 token。** 按 aria2 官方手册，multicall 的 token 由**每个子调用**
   自己的 `params[0]` 携带，外层若放了会被剥离后忽略（不报错）。强制点集中在 `handle_method` 一处。
4. **`dir` 与 `files[].path` 在未知时返回空串，绝不编造。** 代理不掌握节点真实落盘路径，返回一个
   看起来合理的假路径会被用户当成事实（"我文件在哪"），危害大于空值。
5. **Aria2 `status` 取 `task.aria_status` 列**（设计 `02 §4.1.5`），不从 `internal_state` 重新派生 ——
   否则 `start_later:true` 建出的任务（`internal_state='queued'` + `aria_status='paused'`）会错误地报成 `waiting`。
6. **`tellStopped` 的终态集合目前是 `{completed, failed, removed}`。**
   将来 T03/T06 引入 `orphaned` / `missing_on_node` / `deduplicated`（分别映射到 Aria2 的 `error` / `removed`）时，
   **必须同步补进终态集合**，否则 `tellStopped` 会静默漏掉这些任务。

### 已知缺口（尚未实现）

- **T03–T07**：节点健康检查 / 调度器选点 / 真正下发 / 轮询同步 / 去重。**任务只会入库排队，不会真正下载**
  （启动日志有 `WARN` 明示）。
- **`aria2.remove` 的语义**：当前是直接删除任务行（`DELETE FROM task`），而 aria2 官方语义是删除后进入
  `removed` 终态、仍可被 `tellStopped` 查到，真正清除由 `removeDownloadResult` 完成；后者目前返回 `-32601`。
  这是初版即存在的缺口，非回归，待下一个增量处理。

## 开发

```bash
cargo fmt --all --check
cargo clippy --all-targets --all-features -- -D warnings
cargo test --all --locked
```

**这三行要与 CI 逐字一致**，尤其是 `--all-features` 和 `--locked`：
少了前者会出现"本地绿、CI 红"；少了后者则本地会静默更新 `Cargo.lock`，
而 CI 用的是 `--locked`（改了 `Cargo.toml` 必须同步 `Cargo.lock`）。

## 许可证

MIT
