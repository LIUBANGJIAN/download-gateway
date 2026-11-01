#!/usr/bin/env python3
"""伪装 BitComet 节点 —— 供端到端验收用。

为什么需要它：真实 BitComet 节点要登录凭据，验收环境里拿不到时，
"投递 → 回读" 这条链路就完全无法验证。本脚本按代理侧真实调用的接口与
信封格式应答，并且**会随时间推进进度**，因此能看到 permillage 从 0 走到 1000、
`state` 从 running 变 completed，而不是一个永远 0 的死任务。

它**不是** BitComet 的替代品：不做真实下载、不校验 torrent 合法性。
用途只有一个 —— 证明代理侧的调度、投递、回读、状态机是通的。

接口（与 internal/protocol 一致）：
  POST /api/webui/login              三段握手第 1 段，返回 32 位 invite_token
  POST /api/device_token/get         第 2 段，返回 device_token
  POST /api/config/about/get         节点健康探测（L2）
  POST /api/task/http/add            HTTP 任务投递
  POST /api/task/torrent_links/add   磁力/种子投递
  POST /api_v2/task_list/get         任务列表（扁平信封，tasks 在顶层）
  GET  /_state                       调试：当前全部任务与调用计数

用法：
  python3 fake-bitcomet-node.py [--port 19085] [--step-ms 2000] [--step-permille 200]
"""

import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOCK = threading.Lock()
TASKS = []          # 按投递顺序
NEXT_ID = 1000
CALLS = {}          # 路径 -> 调用次数

STEP_MS = 2000      # 每 2 秒推进一步
STEP_PERMILLE = 200  # 每步 +200‰（约 10 秒下完）
TOTAL_SIZE = 64 * 1024 * 1024


def now_ms() -> int:
    return int(time.time() * 1000)


def advance(t: dict) -> None:
    """按经过时间推进一个任务的进度。无状态：每次都从 started_at 重算。"""
    if t["state"] in ("complete", "error"):
        return
    elapsed = now_ms() - t["started_at"]
    steps = int(elapsed // STEP_MS)
    permille = min(1000, steps * STEP_PERMILLE)
    t["permillage"] = permille
    t["download_size"] = TOTAL_SIZE * permille // 1000
    if permille >= 1000:
        t["state"] = "complete"
        t["download_rate"] = 0
    else:
        t["state"] = "running"
        t["download_rate"] = TOTAL_SIZE * STEP_PERMILLE // 1000 * 1000 // STEP_MS


def name_of(body: dict) -> str:
    for key in ("url", "torrent_links", "torrent_url", "link"):
        v = body.get(key)
        if isinstance(v, str) and v:
            return v
    # 有的客户端把 links 放在数组里
    links = body.get("torrent_links")
    if isinstance(links, list) and links:
        return str(links[0])
    return ""


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # 静音，靠 /_state 看计数
        pass

    def _send(self, obj: dict, code: int = 200) -> None:
        raw = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _body(self) -> dict:
        n = int(self.headers.get("Content-Length") or 0)
        if not n:
            return {}
        try:
            return json.loads(self.rfile.read(n).decode("utf-8") or "{}")
        except Exception:
            return {}

    def do_GET(self):
        if self.path == "/_state":
            with LOCK:
                for t in TASKS:
                    advance(t)
                self._send({"calls": CALLS, "tasks": TASKS})
            return
        self._send({"code": 404, "message": "not found"}, 404)

    def do_POST(self):
        path = self.path.split("?")[0]
        body = self._body()
        with LOCK:
            CALLS[path] = CALLS.get(path, 0) + 1

        if path == "/api/webui/login":
            self._send({"error_code": "OK",
                        "invite_token": "0123456789abcdef0123456789abcdef"})
        elif path == "/api/device_token/get":
            self._send({"error_code": "OK", "device_token": "fake-device-token"})
        elif path == "/api/config/about/get":
            self._send({"error_code": "OK", "host_name": "fake-bitcomet",
                        "version": "2.12.0", "client_type": "BitComet"})
        elif path in ("/api/task/http/add", "/api/task/torrent_links/add"):
            global NEXT_ID
            with LOCK:
                NEXT_ID += 1
                task = {
                    "task_id": str(NEXT_ID),
                    "name": name_of(body),
                    "state": "running",
                    "permillage": 0,
                    "download_size": 0,
                    "total_size": TOTAL_SIZE,
                    "download_rate": 0,
                    "started_at": now_ms(),
                }
                TASKS.append(task)
                tid = task["task_id"]
            self._send({"error_code": "OK", "task_id": tid})
        elif path == "/api_v2/task_list/get":
            with LOCK:
                out = []
                for t in TASKS:
                    advance(t)
                    out.append({k: v for k, v in t.items() if k != "started_at"})
            self._send({"error_code": "OK", "tasks": out,
                        "filtered_task_count": len(out)})
        else:
            self._send({"error_code": "NOT_FOUND", "message": path}, 404)


def main() -> None:
    # global 必须出现在函数内**任何引用之前**，否则 SyntaxError：
    #   name 'STEP_MS' is used prior to global declaration
    # 上一版把默认值写成 default=STEP_MS（引用）再 global，直接起不来，
    # 而 nohup 把它藏进了日志 —— 表面现象是「进程秒退、日志空、端口不通」。
    global STEP_MS, STEP_PERMILLE

    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=19085)
    ap.add_argument("--step-ms", type=int, default=STEP_MS)
    ap.add_argument("--step-permille", type=int, default=STEP_PERMILLE)
    args = ap.parse_args()

    STEP_MS, STEP_PERMILLE = args.step_ms, args.step_permille

    srv = ThreadingHTTPServer(("0.0.0.0", args.port), Handler)
    print(f"fake BitComet node listening on :{args.port} "
          f"(step={STEP_MS}ms +{STEP_PERMILLE}‰)", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
