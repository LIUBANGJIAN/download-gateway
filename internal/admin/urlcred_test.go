package admin

import (
	"net/http"
	"strings"
	"testing"
)

// TestAdminLoginRejectsWrongUsername 验证用户名真正参与校验。
//
// 背景：加入用户名之前，登录只比口令 —— `username` 字段在协议里一直缺位，
// 前端也从不发送。现在两者都要对，且**提示语不得区分**「用户不存在」与
// 「口令错误」，否则等于告诉攻击者哪些用户名有效。
func TestAdminLoginRejectsWrongUsername(t *testing.T) {
	srv, client, _ := newAdminEnv(t)

	status, body := doReq(t, client, http.MethodPost, srv.URL+"/api/admin/login",
		`{"username":"root","password":"`+testAdminPassword+`"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("用户名错误应 401，实际 %d %s", status, body)
	}
	if !strings.Contains(string(body), "用户名或口令错误") {
		t.Fatalf("提示应统一为「用户名或口令错误」，实际 %s", body)
	}
	if strings.Contains(string(body), "用户名不存在") || strings.Contains(string(body), "用户名无效") {
		t.Fatalf("不得透露用户名是否存在：%s", body)
	}

	// 显式带默认用户名必须成功（这是升级后最常用的路径）。
	status, body = doReq(t, client, http.MethodPost, srv.URL+"/api/admin/login",
		`{"username":"admin","password":"`+testAdminPassword+`"}`)
	if status != http.StatusOK {
		t.Fatalf("正确用户名+口令应 200，实际 %d %s", status, body)
	}
}

// TestSplitURLCredentials 验证能从地址里拆出凭据。
//
// 用户习惯直接把面板上的 `http://operator:s3cret@host:port` 粘进"地址"栏。
// 不拆的后果有两个：口令明文进库又被回显，而真正用于连节点的
// user/password 字段还是空的 ⇒ 探测失败。
func TestSplitURLCredentials(t *testing.T) {
	cases := []struct {
		name                        string
		raw, user, pass             string
		wantURL, wantUser, wantPass string
	}{
		{
			name:    "带凭据的典型地址",
			raw:     "http://operator:s3cret@192.0.2.10:9085",
			wantURL: "http://192.0.2.10:9085", wantUser: "operator", wantPass: "s3cret",
		},
		{
			name:    "无凭据时原样返回",
			raw:     "http://192.0.2.10:9085",
			wantURL: "http://192.0.2.10:9085", wantUser: "", wantPass: "",
		},
		{
			name: "显式字段优先于地址里的值",
			raw:  "http://u1:p1@host:9085", user: "u2", pass: "p2",
			wantURL: "http://host:9085", wantUser: "u2", wantPass: "p2",
		},
		{
			name:    "只有用户名没有口令时不拆（避免把端口当口令）",
			raw:     "http://operator@host:9085",
			wantURL: "http://operator@host:9085", wantUser: "", wantPass: "",
		},
		{
			name:    "https 同样处理",
			raw:     "https://bob:secret@nas.example:8443",
			wantURL: "https://nas.example:8443", wantUser: "bob", wantPass: "secret",
		},
	}
	for _, c := range cases {
		gotURL, gotUser, gotPass := splitURLCredentials(c.raw, c.user, c.pass)
		if gotURL != c.wantURL || gotUser != c.wantUser || gotPass != c.wantPass {
			t.Fatalf("%s: 得到 (%q,%q,%q)，应为 (%q,%q,%q)",
				c.name, gotURL, gotUser, gotPass, c.wantURL, c.wantUser, c.wantPass)
		}
	}
}

// TestMaskURLCredentials 验证回显脱敏：口令不出现在界面上，其余部分原样保留。
func TestMaskURLCredentials(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://operator:s3cret@192.0.2.10:9085", "http://operator:***@192.0.2.10:9085"},
		{"https://bob:p%40ss@nas.example:8443", "https://bob:***@nas.example:8443"},
		{"http://192.0.2.10:9085", "http://192.0.2.10:9085"},
		{"", ""},
	}
	for _, c := range cases {
		if got := maskURLCredentials(c.in); got != c.want {
			t.Fatalf("%q 脱敏后应为 %q，实际 %q", c.in, c.want, got)
		}
	}
}

// TestCreateNodeWithCredentialsInURL 是端到端的回归：地址里带凭据、密码框留空。
//
// 这条曾经是 422 —— 校验用的是「拆分前的 password」，而用户把口令写在地址里，
// 那个变量本来就是空的。断言必须落到**存的到底是什么**上，只看状态码不够：
// 100 种"成功了但存错东西"都能返回 200。
func TestCreateNodeWithCredentialsInURL(t *testing.T) {
	srv, client, _ := newAdminEnv(t)
	if status, body := doReq(t, client, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+testAdminPassword+`"}`); status != http.StatusOK {
		t.Fatalf("登录失败: %d %s", status, body)
	}

	status, body := doReq(t, client, http.MethodPost, srv.URL+"/api/admin/nodes",
		`{"alias":"nas","base_url":"http://operator:s3cret@203.0.113.10:9085",`+
			`"weight":1,"max_concurrent":3,"max_rate_kbps":0,"role":"series"}`)
	if status != http.StatusOK {
		t.Fatalf("地址带凭据时应能创建（密码框可留空），实际 %d %s", status, body)
	}
	// 回显的地址不得再带口令。
	// 断言必须跟着上面那串字面量走：写成别的口令会变成恒真空转（原先是 "secret@"，
	// 字面量改成 s3cret 之后 "secret@" 不再是它的子串，这条就永远是绿的）。
	if strings.Contains(string(body), "s3cret@") {
		t.Fatalf("响应里的 base_url 不得含明文口令：%s", body)
	}
	if !strings.Contains(string(body), "http://203.0.113.10:9085") {
		t.Fatalf("base_url 应被清洗成 host:port，实际 %s", body)
	}
	if !strings.Contains(string(body), `"user":"operator"`) {
		t.Fatalf("地址里的用户名应回填到 user 字段：%s", body)
	}
	if !strings.Contains(string(body), `"password_set":true`) {
		t.Fatalf("地址里的口令应被加密存储（password_set=true）：%s", body)
	}
}
