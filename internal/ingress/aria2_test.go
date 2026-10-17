package ingress

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAddURISingleParamDoesNotPanic 是一条**回归**用例，守一个真实踩到的 panic。
//
// 2026-10-18 实测：`params` 只有一段（`[["https://…/a.mkv"]]`）时，
// `addURI` 里直接写 `params[1].(map[string]any)` ⇒ 索引越界 ⇒ handler panic。
//
// 为什么它长期没被发现：容器冒烟的 74 条断言里**没有一条**打过 `aria2.addUri`
// （只打了 getVersion / tellStatus / getGlobalStat 这些只读方法）。
// 而 panic 在 HTTP 层的表现是"连接被重置"，客户端看到的是"网关挂了"，
// 于是排障方向会跑偏到网络/端口上。
//
// 这条用例同时覆盖两种形态：**单参数**（绝大多数客户端）与**带 options**。
func TestAddURISingleParamDoesNotPanic(t *testing.T) {
	ts := newPublicServer(t, nil)

	body := `{"jsonrpc":"2.0","id":"1","method":"aria2.addUri",` +
		`"params":[["https://example.invalid/Show.Name.S01E01.1080p.mkv"]]}`
	resp, err := http.Post(ts.URL+"/jsonrpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d，期望 200（panic 会让连接被重置）", resp.StatusCode)
	}
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if out.Error != nil {
		t.Fatalf("返回错误: code=%d", out.Error.Code)
	}
	if len(out.Result) != 16 {
		t.Fatalf("GID = %q，期望 16 位十六进制", out.Result)
	}
}

// TestAddURIWithOptionsMapsFields 守 `out`/`pause` 的映射：
// `out` 落到 `task.name`（分组识别就靠它），`pause=true` 落到 `not_started`。
func TestAddURIWithOptionsMapsFields(t *testing.T) {
	ts := newPublicServer(t, nil)

	body := `{"jsonrpc":"2.0","id":"2","method":"aria2.addUri",` +
		`"params":[["https://example.invalid/x.bin"],{"out":"Show.Name.S01E02.1080p.mkv","pause":"true"}]}`
	resp, err := http.Post(ts.URL+"/jsonrpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if out.Result == "" {
		t.Fatal("未返回 GID")
	}

	// 用 tellStatus 回查：name 与 paused 都必须落库（这是可观测终态）
	tell := `{"jsonrpc":"2.0","id":"3","method":"aria2.tellStatus","params":["` + out.Result + `"]}`
	resp2, err := http.Post(ts.URL+"/jsonrpc", "application/json", strings.NewReader(tell))
	if err != nil {
		t.Fatalf("回查请求失败: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()

	var status struct {
		Result struct {
			Status string `json:"status"`
			Files  []struct {
				Path string `json:"path"`
			} `json:"files"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&status); err != nil {
		t.Fatalf("解析回查失败: %v", err)
	}
	if status.Result.Status != "paused" {
		t.Fatalf("status = %q，期望 paused（pause=true）", status.Result.Status)
	}
	if len(status.Result.Files) != 1 || status.Result.Files[0].Path != "Show.Name.S01E02.1080p.mkv" {
		t.Fatalf("files = %+v，期望单文件且路径为 out 的值", status.Result.Files)
	}
}
