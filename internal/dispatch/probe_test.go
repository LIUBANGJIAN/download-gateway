package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/secret"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// testKey 是测试用主密钥（与 engine 测试同值，便于跨包一致）。
const testKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"), filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func insertNode(t *testing.T, st *store.Store, baseURL, state string) int64 {
	t.Helper()
	box := secret.WithKey(testKey)
	enc, err := box.Wrap("node-pass")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	now := time.Now().Unix()
	if _, err := st.Execute(context.Background(),
		`INSERT INTO node (alias, base_url, user, pass_enc, weight, max_concurrent, role,
		                   enabled, state, created_at, updated_at)
		 VALUES ('n1', ?1, 'admin', ?2, 1.0, 3, 'generic', 1, ?3, ?4, ?4)`,
		baseURL, enc, state, now,
	); err != nil {
		t.Fatalf("插节点失败: %v", err)
	}
	rows, _ := st.Query(context.Background(), `SELECT node_id FROM node WHERE alias = 'n1'`)
	return rows[0][0].(int64)
}

func aboutNode(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/webui/login":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error_code": "OK", "invite_token": "0123456789abcdef0123456789abcdef",
			})
		case "/api/device_token/get":
			_ = json.NewEncoder(w).Encode(map[string]any{"error_code": "OK", "device_token": "t"})
		case "/api/config/about/get":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error_code": "OK", "about_info": map[string]any{"version": "2.21"},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"error_code": "OK"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeTurnsUnknownIntoOnline 守「新节点不会永远卡在 unknown」。
//
// 这条很实际：节点 `state` 默认 `unknown`，而 C2 只允许 `online`。
// 若探测不落地，用户配好节点、推了任务，看到的是**永远排队**，
// 而日志里没有任何一行说"节点不可用" —— 因为根本没人去探。
func TestProbeTurnsUnknownIntoOnline(t *testing.T) {
	st := openStore(t)
	srv := aboutNode(t)
	id := insertNode(t, st, srv.URL, "unknown")

	box := secret.WithKey(testKey)
	res, err := ProbeAndPersist(context.Background(), st, box, id)
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if res.State != "online" {
		t.Fatalf("state = %q，期望 online（detail=%s）", res.State, res.Detail)
	}

	rows, _ := st.Query(context.Background(), `SELECT state, last_probe_at, ok_streak FROM node WHERE node_id = ?1`, id)
	if rows[0][0].(string) != "online" {
		t.Fatalf("库里 state = %v，期望 online", rows[0][0])
	}
	if rows[0][1].(int64) == 0 {
		t.Fatal("last_probe_at 未更新")
	}
	if rows[0][2].(int64) < 1 {
		t.Fatalf("ok_streak = %v，期望 ≥1", rows[0][2])
	}

	// 历史表必须有行（否则"这台机器最近稳不稳"无从回答）
	hrows, _ := st.Query(context.Background(),
		`SELECT state_after, tcp_ok, api_ok FROM node_health WHERE node_id = ?1`, id)
	if len(hrows) != 1 {
		t.Fatalf("node_health 行数 = %d，期望 1", len(hrows))
	}
	if hrows[0][0].(string) != "online" || hrows[0][1].(int64) != 1 || hrows[0][2].(int64) != 1 {
		t.Fatalf("node_health = %v，期望 online/tcp=1/api=1", hrows[0])
	}
}

// TestProbeUnreachableIsOffline 守"连不上就是 offline"，而不是留在 unknown。
func TestProbeUnreachableIsOffline(t *testing.T) {
	st := openStore(t)
	// 保留端口但没人监听：用一个刚关掉的端口
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	id := insertNode(t, st, addr, "unknown")
	box := secret.WithKey(testKey)
	res, err := ProbeAndPersist(context.Background(), st, box, id)
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if res.State != "offline" {
		t.Fatalf("state = %q，期望 offline", res.State)
	}

	rows, _ := st.Query(context.Background(), `SELECT state, fail_streak FROM node WHERE node_id = ?1`, id)
	if rows[0][0].(string) != "offline" {
		t.Fatalf("库里 state = %v，期望 offline", rows[0][0])
	}
	if rows[0][1].(int64) != 1 {
		t.Fatalf("fail_streak = %v，期望 1", rows[0][1])
	}
}

// TestProbeAppAccessDisabledIsDegraded 守「端口通但 App 访问关闭」必须判 degraded。
//
// 只做 TCP 探测会把它判成 online，于是调度器往里投任务、每个 add 都被拒，
// 而"这台机器其实是坏的"这件事在界面上永远看不出来。
func TestProbeAppAccessDisabledIsDegraded(t *testing.T) {
	st := openStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error_code": "APP_ACCESS_DISABLED", "error_message": "App access is disabled",
		})
	}))
	defer srv.Close()

	id := insertNode(t, st, srv.URL, "unknown")
	box := secret.WithKey(testKey)
	res, err := ProbeAndPersist(context.Background(), st, box, id)
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if res.State != "degraded" {
		t.Fatalf("state = %q，期望 degraded（端口通不等于能用）", res.State)
	}
	if !res.TCPOK {
		t.Fatal("TCP 应为通")
	}
	if res.APIOK {
		t.Fatal("API 应为不通")
	}
}

// TestDelayBackoffIsBounded 守退避公式的两端：指数增长 + 封顶。
func TestDelayBackoffIsBounded(t *testing.T) {
	fixed := func() float64 { return 1.0 }
	if got := Delay(1, fixed); got != 30*time.Second {
		t.Fatalf("attempt=1 delay = %v，期望 30s", got)
	}
	if got := Delay(2, fixed); got != 60*time.Second {
		t.Fatalf("attempt=2 delay = %v，期望 60s", got)
	}
	if got := Delay(10, fixed); got != 1800*time.Second {
		t.Fatalf("attempt=10 delay = %v，期望封顶 1800s", got)
	}
	// 抖动窗口 [0.8,1.2)
	lo, hi := Delay(1, func() float64 { return 0.8 }), Delay(1, func() float64 { return 1.19 })
	if lo != 24*time.Second {
		t.Fatalf("下界 = %v，期望 24s", lo)
	}
	if hi < 35*time.Second || hi > 36*time.Second {
		t.Fatalf("上界 = %v，期望约 35.7s", hi)
	}
}
