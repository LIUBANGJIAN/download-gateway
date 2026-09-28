-- ============================================================
-- 04 方案 · v2 基线(13表) + 本次增量(1表) 合并 DDL · 可执行验证
-- 目标: table=14 / 外键自检为空
-- ============================================================
PRAGMA foreign_keys = ON;

-- 0) 迁移版本
CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT    NOT NULL,
  applied_at INTEGER NOT NULL
);

-- 1) 节点（v2：删除 root_download_dir 与容量/磁盘列；保留 max_concurrent）
CREATE TABLE node (
  node_id           INTEGER PRIMARY KEY,
  alias             TEXT    NOT NULL UNIQUE,
  base_url          TEXT    NOT NULL,
  user              TEXT    NOT NULL,
  pass_enc          TEXT    NOT NULL,
  device_name       TEXT    NOT NULL DEFAULT 'dispatch-proxy',
  client_id         TEXT,
  weight            REAL    NOT NULL DEFAULT 1.0 CHECK (weight >= 0),
  max_concurrent    INTEGER NOT NULL DEFAULT 3,
  max_rate_bytes    INTEGER NOT NULL DEFAULT 0,
  role              TEXT    NOT NULL DEFAULT 'generic'
                    CHECK (role IN ('generic','series','netdisk')),
  tags              TEXT    NOT NULL DEFAULT '',
  enabled           INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  state             TEXT    NOT NULL DEFAULT 'unknown'
                    CHECK (state IN ('online','degraded','offline','unknown')),
  state_since       INTEGER NOT NULL DEFAULT 0,
  fail_streak       INTEGER NOT NULL DEFAULT 0,
  ok_streak         INTEGER NOT NULL DEFAULT 0,
  throttle_until    INTEGER NOT NULL DEFAULT 0,
  token_cipher      TEXT,
  token_expire_at   INTEGER NOT NULL DEFAULT 0,
  last_probe_at     INTEGER NOT NULL DEFAULT 0,
  last_seen_at      INTEGER NOT NULL DEFAULT 0,
  app_version       TEXT,
  platform          TEXT,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);

-- 2) 网盘来源档案
CREATE TABLE source_profile (
  source_key        TEXT PRIMARY KEY,
  display_name      TEXT NOT NULL,
  match_host_suffix TEXT NOT NULL,
  match_path_regex  TEXT,
  strategy          TEXT NOT NULL DEFAULT 'round_robin'
                    CHECK (strategy IN ('round_robin','affinity','pinned')),
  pinned_node_id    INTEGER REFERENCES node(node_id) ON DELETE SET NULL,
  max_rate_bytes    INTEGER NOT NULL DEFAULT 0,
  enabled           INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);

-- 3) 任务组（v2：删除 rel_dir/path_template/prefer_same_node/pinned_node_id；
--            split_policy 默认 capacity -> none；新增组级硬绑定列）
CREATE TABLE task_group (
  group_id                INTEGER PRIMARY KEY,
  name                    TEXT NOT NULL,
  kind                    TEXT NOT NULL DEFAULT 'series'
                          CHECK (kind IN ('series','batch','single','source')),
  season                  TEXT,
  source_key              TEXT REFERENCES source_profile(source_key) ON DELETE SET NULL,
  affinity_key            TEXT NOT NULL UNIQUE,
  bound_node_id           INTEGER REFERENCES node(node_id) ON DELETE SET NULL,
  binding_mode            TEXT NOT NULL DEFAULT 'auto'
                          CHECK (binding_mode IN ('auto','manual')),
  bound_at                INTEGER NOT NULL DEFAULT 0,
  binding_epoch           INTEGER NOT NULL DEFAULT 0,
  group_state             TEXT NOT NULL DEFAULT 'unbound'
                          CHECK (group_state IN ('unbound','active','split','draining','closed')),
  binding_policy          TEXT NOT NULL DEFAULT 'strong'
                          CHECK (binding_policy IN ('strong','weak','manual')),
  split_policy            TEXT NOT NULL DEFAULT 'none'
                          CHECK (split_policy IN ('none','capacity')),
  split_at                INTEGER,
  split_reason            TEXT,
  max_concurrent_in_group INTEGER NOT NULL DEFAULT 2,
  wait_deadline           INTEGER,
  size_hint               INTEGER,
  created_at              INTEGER NOT NULL,
  updated_at              INTEGER NOT NULL
);

-- 4) 任务（v2：error_class 重写；删 save_folder_rel/abs；新增 episode_no/dispatch_reason）
CREATE TABLE task (
  task_id         TEXT PRIMARY KEY,
  gid             TEXT UNIQUE,
  group_id        INTEGER REFERENCES task_group(group_id) ON DELETE SET NULL,
  source_key      TEXT REFERENCES source_profile(source_key) ON DELETE SET NULL,
  kind            TEXT NOT NULL
                  CHECK (kind IN ('http','bt','magnet','torrent')),
  url_raw         TEXT NOT NULL,
  url_norm        TEXT,
  dedup_key       TEXT,
  dedup_level     INTEGER NOT NULL DEFAULT 0 CHECK (dedup_level BETWEEN 0 AND 3),
  name            TEXT,
  episode_no      INTEGER,
  size_state      TEXT NOT NULL DEFAULT 'unknown'
                  CHECK (size_state IN ('unknown','probing','resolved','unresolved','estimated')),
  total_size      INTEGER,
  downloaded_size INTEGER NOT NULL DEFAULT 0,
  permillage      INTEGER NOT NULL DEFAULT 0 CHECK (permillage BETWEEN 0 AND 1000),
  aria_status     TEXT NOT NULL DEFAULT 'waiting',
  internal_state  TEXT NOT NULL DEFAULT 'queued',
  error_class     TEXT CHECK (error_class IN
                  ('none','not_started','server_file_not_found','out_of_retry',
                   'node_down','silently_not_created','throttled','agent_timeout','unknown')),
  error_message   TEXT,
  node_id         INTEGER REFERENCES node(node_id) ON DELETE SET NULL,
  node_task_id    TEXT,
  dispatch_reason TEXT CHECK (dispatch_reason IN
                  ('affinity_hit','scored','affinity_wait','split_forced')),
  attempt_no      INTEGER NOT NULL DEFAULT 0,
  max_attempts    INTEGER NOT NULL DEFAULT 3,
  next_retry_at   INTEGER NOT NULL DEFAULT 0,
  possible_duplicate INTEGER NOT NULL DEFAULT 0 CHECK (possible_duplicate IN (0,1)),
  progress_at     INTEGER NOT NULL DEFAULT 0,
  batch_pending   INTEGER NOT NULL DEFAULT 0 CHECK (batch_pending IN (0,1)),
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  completed_at    INTEGER
);

-- 5) 任务尝试
CREATE TABLE task_attempt (
  attempt_id   INTEGER PRIMARY KEY,
  task_id      TEXT NOT NULL REFERENCES task(task_id) ON DELETE CASCADE,
  attempt_no   INTEGER NOT NULL,
  node_id      INTEGER NOT NULL REFERENCES node(node_id),
  node_task_id TEXT,
  state        TEXT NOT NULL
               CHECK (state IN ('dispatched','created','running','failed','orphaned','superseded')),
  reason       TEXT,
  started_at   INTEGER NOT NULL,
  ended_at     INTEGER,
  UNIQUE (task_id, attempt_no)
);

-- 6) 任务文件
CREATE TABLE task_file (
  file_id     INTEGER PRIMARY KEY,
  task_id     TEXT NOT NULL REFERENCES task(task_id) ON DELETE CASCADE,
  idx         INTEGER NOT NULL,
  path        TEXT,
  length      INTEGER,
  completed   INTEGER NOT NULL DEFAULT 0,
  selected    INTEGER NOT NULL DEFAULT 1,
  UNIQUE (task_id, idx)
);

-- 7) 去重索引
CREATE TABLE dedup_index (
  dedup_key   TEXT PRIMARY KEY,
  level       INTEGER NOT NULL CHECK (level IN (1,2,3)),
  confidence  TEXT NOT NULL CHECK (confidence IN ('exact','high','medium')),
  task_id     TEXT REFERENCES task(task_id) ON DELETE SET NULL,
  node_id     INTEGER REFERENCES node(node_id) ON DELETE SET NULL,
  created_at  INTEGER NOT NULL
);

-- 8) 进度历史
CREATE TABLE task_progress (
  id          INTEGER PRIMARY KEY,
  task_id     TEXT NOT NULL REFERENCES task(task_id) ON DELETE CASCADE,
  node_id     INTEGER NOT NULL,
  permillage  INTEGER NOT NULL,
  downloaded  INTEGER NOT NULL,
  total       INTEGER,
  rate        REAL NOT NULL DEFAULT 0,
  observed_at INTEGER NOT NULL,
  UNIQUE (task_id, observed_at)
);

-- 9) 调度决策留痕
CREATE TABLE dispatch_log (
  id          INTEGER PRIMARY KEY,
  task_id     TEXT NOT NULL,
  group_id    INTEGER,
  chosen_node INTEGER,
  candidates  INTEGER NOT NULL,
  score_json  TEXT NOT NULL,
  decision    TEXT NOT NULL
              CHECK (decision IN ('assigned','queued','deduped','rejected')),
  reason      TEXT,
  created_at  INTEGER NOT NULL
);

-- 10) 健康探测历史
CREATE TABLE node_health (
  id          INTEGER PRIMARY KEY,
  node_id     INTEGER NOT NULL REFERENCES node(node_id) ON DELETE CASCADE,
  tcp_ok      INTEGER NOT NULL CHECK (tcp_ok IN (0,1)),
  api_ok      INTEGER NOT NULL CHECK (api_ok IN (0,1)),
  http_code   INTEGER,
  error_code  TEXT,
  latency_ms  INTEGER,
  state_after TEXT NOT NULL CHECK (state_after IN ('online','degraded','offline')),
  observed_at INTEGER NOT NULL
);

-- 11) 事件/审计
CREATE TABLE event_log (
  id         INTEGER PRIMARY KEY,
  level      TEXT NOT NULL CHECK (level IN ('info','warn','error')),
  category   TEXT NOT NULL CHECK (category IN ('node','task','dedup','group','system','security')),
  node_id    INTEGER,
  task_id    TEXT,
  message    TEXT NOT NULL,
  detail     TEXT,
  created_at INTEGER NOT NULL
);

-- 12) 配置 KV
CREATE TABLE app_config (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- 13) ★本次新增★ 组绑定变更留痕
CREATE TABLE group_binding_log (
  id            INTEGER PRIMARY KEY,
  group_id      INTEGER NOT NULL REFERENCES task_group(group_id) ON DELETE CASCADE,
  binding_epoch INTEGER NOT NULL,
  from_node_id  INTEGER REFERENCES node(node_id) ON DELETE SET NULL,
  to_node_id    INTEGER REFERENCES node(node_id) ON DELETE SET NULL,
  action        TEXT NOT NULL
                CHECK (action IN ('bind','rebind','split','rejoin','release')),
  reason        TEXT NOT NULL,
  detail        TEXT,
  at            INTEGER NOT NULL
);

-- ============ 索引 ============
CREATE INDEX idx_task_state        ON task(internal_state, next_retry_at);
CREATE INDEX idx_task_group        ON task(group_id);
CREATE INDEX idx_task_source       ON task(source_key);
CREATE INDEX idx_task_node         ON task(node_id);
CREATE INDEX idx_task_dedup        ON task(dedup_key);
CREATE INDEX idx_task_progress_at  ON task(progress_at);
-- INV-A5：组内集号唯一（★ 部分唯一索引：SQLite 的 UNIQUE 对 NULL 不去重，
-- 故显式限定 episode_no IS NOT NULL；适用范围仅限"已识别集号"的成员）
CREATE UNIQUE INDEX ux_task_group_episode ON task(group_id, episode_no) WHERE episode_no IS NOT NULL;
CREATE INDEX idx_attempt_node      ON task_attempt(node_id, state);
-- INV-A3（05 §10「投递时刻一致性」）的对账查询按 task_id 定位投递行：
--   SELECT a.started_at FROM task_attempt a WHERE a.task_id=? AND a.node_id=? ...
-- 该查找由 UNIQUE(task_id, attempt_no) 隐式建立的索引覆盖（前缀 task_id），**无需新增索引**；
-- 组的绑定时间线由 idx_gbl_group(group_id, at) 覆盖。⇒ 本不变量跑得起、且不增加写放大。
CREATE INDEX idx_progress_task     ON task_progress(task_id, observed_at DESC);
CREATE INDEX idx_health_node       ON node_health(node_id, observed_at DESC);
CREATE INDEX idx_health_time       ON node_health(observed_at);
CREATE INDEX idx_dispatch_task     ON dispatch_log(task_id);
CREATE INDEX idx_dispatch_time     ON dispatch_log(created_at);
CREATE INDEX idx_event_time        ON event_log(created_at DESC);
CREATE INDEX idx_event_cat         ON event_log(category, created_at DESC);
-- 新增表的索引
CREATE INDEX idx_gbl_group         ON group_binding_log(group_id, at);
CREATE INDEX idx_gbl_time          ON group_binding_log(at);

-- ============ 种子配置（沿用 03 §2.3）============
INSERT INTO app_config(key, value, updated_at) VALUES
  ('dispatch.save_folder.mode', 'node_default', 0),
  ('dispatch.space_constraint', 'off', 0),
  ('dispatch.size_probe.head',  'optional_off', 0),
  ('dispatch.group.binding_policy', 'strong', 0),
  ('dispatch.group.split_policy', 'none', 0),
  ('dispatch.group.wait_seconds', '600', 0),
  ('dispatch.group.max_concurrent_in_group', '2', 0),
  ('dispatch.group.key_include_source', 'off', 0),
  ('dispatch.retry.no_progress_seconds', '120', 0),
  ('dispatch.retry.cross_node', 'off', 0);
