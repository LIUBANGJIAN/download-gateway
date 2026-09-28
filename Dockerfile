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
ENTRYPOINT ["download-gateway"]
