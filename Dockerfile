# ============================================================
# download-gateway · 多阶段构建
# ------------------------------------------------------------
# T01 阶段为**两阶段**（Rust 构建 + 瘦运行镜像）。
# T10 落地前端后，在最前面插入「阶段 0：前端构建」，并在 Rust 阶段
# `COPY --from=web /web/dist ./web/dist`（见 `02 §5.3` 的完整形态）。
#
# 关键点（对应 `02 §5.3` 的六条要点）：
#   ① rustls ⇒ 运行镜像**无需 libssl**；② 非 root（uid 10001）；
#   ③ `VOLUME /data` 持久化 SQLite；④ `HEALTHCHECK` 打管理端口 `/healthz`；
#   ⑤ 两端口 `EXPOSE`；⑥ 迁移脚本随镜像分发。
# ============================================================

# ---------- 阶段 1：Rust 构建 ----------
FROM rust:1.98-bookworm AS build
WORKDIR /src

RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential pkg-config ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# 清单与源码一起拷入。
# 注：`rusqlite` 用 `bundled` 特性自带 SQLite 源码，
# 因此**不需要** apt 装 libsqlite3-dev（这也是选 bundled 的原因）。
COPY Cargo.toml Cargo.lock ./
COPY crates ./crates
COPY src ./src
COPY migrations ./migrations

# 版本号在**编译期**注入（`option_env!("APP_VERSION")`）。
# 必须放在这里而不是文件顶部：改动它会令此后所有层缓存失效，
# 放顶部会连 apt-get 那一层也一起失效，白白多花时间。
# 默认值刻意写成 `0.0.0-unknown` 而不是 `0.1.0` —— 让"忘了传 build-arg"
# 这件事**可见**（冒烟断言会因此红），而不是悄悄退化成看起来正常的 0.1.0。
ARG APP_VERSION=0.0.0-unknown
ENV APP_VERSION=${APP_VERSION}

RUN cargo build --release --locked

# ---------- 阶段 2：瘦运行镜像 ----------
FROM debian:bookworm-slim AS runtime

# curl 供 HEALTHCHECK 使用；ca-certificates 供 rustls 读取系统根证书
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -r -u 10001 -m -d /data app

COPY --from=build /src/target/release/download-gateway /usr/local/bin/download-gateway
# 迁移脚本随镜像分发：容器启动时按 DISPATCH_MIGRATIONS 读取并应用
COPY --from=build /src/migrations /opt/download-gateway/migrations

VOLUME ["/data"]
ENV DISPATCH_DB=/data/dispatch.db \
    DISPATCH_MIGRATIONS=/opt/download-gateway/migrations \
    DISPATCH_PUBLIC_ADDR=0.0.0.0:6800 \
    DISPATCH_ADMIN_ADDR=0.0.0.0:8080

EXPOSE 6800 8080

# 打管理端口（容器内一定监听在 0.0.0.0，故 127.0.0.1 可达）
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8080/healthz || exit 1

USER app

# 运行阶段可见的 OCI 版本标签，便于 `docker inspect` 直接看到（不进二进制也能读）。
# 需在本阶段重新声明 ARG：`--build-arg` 只对该阶段已声明的 ARG 生效。
ARG APP_VERSION=0.0.0-unknown
LABEL org.opencontainers.image.version="${APP_VERSION}"

ENTRYPOINT ["download-gateway"]
