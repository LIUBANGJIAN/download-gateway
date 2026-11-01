package ingress

import (
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

// ptr returns a pointer to s (helpers for the optional-token APIs).
func ptr(s string) *string { return &s }

// Ported from crates/dispatch-core/src/ingress/envelope.rs `mod tests`.

func TestBearerParsingIsCaseInsensitive(t *testing.T) {
	// Rust: bearer_parsing_is_case_insensitive
	h := http.Header{}
	h.Set("Authorization", "Bearer abc123")
	if got := BearerOf(h); got == nil || *got != "abc123" {
		t.Fatalf("Bearer abc123 -> %v", got)
	}
	h.Set("Authorization", "bearer xyz")
	if got := BearerOf(h); got == nil || *got != "xyz" {
		t.Fatalf("bearer xyz -> %v", got)
	}
	h.Set("Authorization", "Basic zzz")
	if got := BearerOf(h); got != nil {
		t.Fatalf("Basic zzz -> %v, want nil", *got)
	}
}

func TestTokenParamExtractionAndValidation(t *testing.T) {
	// Rust: token_param_extraction_and_validation
	// 未配置 -> 放行且移除 token 元素
	p := []any{"token:aaa", "gid"}
	if err := TokenParamOf(&p, nil); err != nil {
		t.Fatalf("unconfigured: %v", err)
	}
	if len(p) != 1 {
		t.Fatalf("token 元素应被移除, len=%d", len(p))
	}

	// 已配置 + 匹配 -> Ok
	p = []any{"token:secret"}
	if err := TokenParamOf(&p, ptr("secret")); err != nil {
		t.Fatalf("match: %v", err)
	}

	// 已配置 + 不匹配 -> Mismatch
	p = []any{"token:wrong"}
	if err := TokenParamOf(&p, ptr("secret")); !errors.Is(err, ErrTokenMismatch) {
		t.Fatalf("mismatch: %v", err)
	}

	// 已配置 + 缺失 -> Missing
	p = []any{"noslash"}
	if err := TokenParamOf(&p, ptr("secret")); !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("missing: %v", err)
	}
}

// authHeader 造一个 `Authorization: <原始值>` 头。
func authHeader(raw string) http.Header {
	h := http.Header{}
	if raw != "" {
		h.Set("Authorization", raw)
	}
	return h
}

// bearerHeader 造 `Authorization: Bearer <v>`。
func bearerHeader(v string) http.Header { return authHeader("Bearer " + v) }

// basicHeader 造 `Authorization: Basic base64(<user>:<pass>)`。
func basicHeader(user, pass string) http.Header {
	return authHeader("Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
}

func TestDecisionTableRows(t *testing.T) {
	// Rust: decision_table_rows（前 6 行逐字保留），后面是本轮新增的 Basic / 用户名行。
	configured := NewAuthPolicy(nil, ptr("secret"))
	withUser := NewAuthPolicy(ptr("ops"), ptr("secret"))
	unconfigured := NewAuthPolicy(nil, nil)

	cases := []struct {
		name   string
		policy AuthPolicy
		header http.Header
		device bool
		want   Authz
	}{
		{"row1 device_token 最高优先", configured, bearerHeader("x"), true, AuthzAllow},
		{"row2 Bearer 命中口令", configured, bearerHeader("secret"), false, AuthzAllow},
		{"row3 Bearer 不匹配", configured, bearerHeader("wrong"), false, AuthzDeny},
		{"row4 无任何凭据", configured, http.Header{}, false, AuthzDeny},
		{"row5 未配置 + Bearer", unconfigured, bearerHeader("whatever"), false, AuthzAllow},
		{"row6 未配置 + 空", unconfigured, http.Header{}, false, AuthzAllow},

		// ── 本轮新增：Basic 与用户名 ─────────────────────────────────────
		// 没配用户名 = 只验口令。这是老部署（只配过一个 token）的兼容路径：
		// 强行要求用户名会把它们全部挡在门外。
		{"Basic 未配用户名：只验口令", configured, basicHeader("随便填", "secret"), false, AuthzAllow},
		{"Basic 口令错", configured, basicHeader("随便填", "wrong"), false, AuthzDeny},
		// 配了用户名就必须两边都对 —— 否则 compose 里那个账号是装饰。
		{"Basic 配了用户名：两边都对", withUser, basicHeader("ops", "secret"), false, AuthzAllow},
		{"Basic 用户名错", withUser, basicHeader("admin", "secret"), false, AuthzDeny},
		{"Basic 口令对用户名错", withUser, basicHeader("nobody", "secret"), false, AuthzDeny},
		// Bearer 只有一个值，没有用户名位置 ⇒ 不参与用户名校验（向后兼容）。
		{"Bearer 绕过用户名（兼容旧调用）", withUser, bearerHeader("secret"), false, AuthzAllow},
		// 畸形 Basic 必须拒绝，而不是「解析失败就当没带凭据」放过去。
		{"Basic 无冒号", configured, authHeader("Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon"))), false, AuthzDeny},
		{"Basic 非 base64", configured, authHeader("Basic !!!"), false, AuthzDeny},
		{"Basic 空载荷", configured, authHeader("Basic "), false, AuthzDeny},
	}
	for _, tc := range cases {
		if got := tc.policy.Decide(tc.header, tc.device); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// BasicOf 的边界：这是新增解析器，写错会表现为「配了用户名却怎么都登不上」。
func TestBasicOfEdges(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantOK   bool
		wantUser string
		wantPass string
	}{
		{"正常", "Basic " + base64.StdEncoding.EncodeToString([]byte("ops:pw")), true, "ops", "pw"},
		{"大小写不敏感", "basic " + base64.StdEncoding.EncodeToString([]byte("ops:pw")), true, "ops", "pw"},
		{"口令里含冒号（首个冒号分界）", "Basic " + base64.StdEncoding.EncodeToString([]byte("ops:a:b")), true, "ops", "a:b"},
		{"空口令", "Basic " + base64.StdEncoding.EncodeToString([]byte("ops:")), true, "ops", ""},
		{"无冒号", "Basic " + base64.StdEncoding.EncodeToString([]byte("ops")), false, "", ""},
		{"非 base64", "Basic ###", false, "", ""},
		{"Bearer 不属于 Basic", "Bearer tok", false, "", ""},
		{"只有 scheme", "Basic ", false, "", ""},
		{"缺失", "", false, "", ""},
	}
	for _, tc := range cases {
		u, p, ok := BasicOf(authHeader(tc.raw))
		if ok != tc.wantOK {
			t.Fatalf("%s: ok=%v, want %v", tc.name, ok, tc.wantOK)
		}
		if ok && (u != tc.wantUser || p != tc.wantPass) {
			t.Fatalf("%s: got %q/%q, want %q/%q", tc.name, u, p, tc.wantUser, tc.wantPass)
		}
	}
}

func TestCtEqBasic(t *testing.T) {
	// Rust: ct_eq_basic
	if !CtEq("abc", "abc") {
		t.Fatal("equal strings")
	}
	if CtEq("abc", "abd") {
		t.Fatal("differing strings")
	}
	if CtEq("abc", "abcd") {
		t.Fatal("different-length strings")
	}
}

func TestIsOKCodeCaseInsensitive(t *testing.T) {
	// Rust (envelope.rs::tests, testing the re-exported is_ok_code):
	// ok_code_is_case_insensitive
	if !IsOKCode("OK") || !IsOKCode("ok") || !IsOKCode("") {
		t.Fatal("OK / ok / empty must count as success")
	}
	if IsOKCode("INVALID_TOKEN") {
		t.Fatal("INVALID_TOKEN must not count as success")
	}
}

// fakeState is a minimal PublicAuthState for exercising Authorize.
type fakeState struct {
	user    *string
	pass    *string
	devices map[string]bool
}

func (f fakeState) PublicAuth() (string, string, bool) {
	if f.pass == nil {
		return "", "", false
	}
	u := ""
	if f.user != nil {
		u = *f.user
	}
	return u, *f.pass, true
}

func (f fakeState) IsDeviceToken(t string) bool { return f.devices[t] }

// Authorize is the real entry point every protected route uses. envelope.rs
// doesn't unit-test it directly, so this pins its observable decisions.
func TestAuthorizeWiring(t *testing.T) {
	st := fakeState{pass: ptr("secret"), devices: map[string]bool{"devtok": true}}

	if Authorize(st, bearerHeader("devtok")) != AuthzAllow {
		t.Fatal("device token should be allowed")
	}
	if Authorize(st, bearerHeader("secret")) != AuthzAllow {
		t.Fatal("matching public token should be allowed")
	}
	if Authorize(st, bearerHeader("nope")) != AuthzDeny {
		t.Fatal("wrong token should be denied")
	}
	if Authorize(st, http.Header{}) != AuthzDeny {
		t.Fatal("no header with configured token should be denied")
	}

	// 未配用户名时 Basic 只验口令。
	if Authorize(st, basicHeader("随便", "secret")) != AuthzAllow {
		t.Fatal("Basic with correct password and no configured user should allow")
	}

	// 配了用户名：Authorize 必须把它从 state 里取出来参与判定。
	withUser := fakeState{user: ptr("ops"), pass: ptr("secret")}
	if Authorize(withUser, basicHeader("ops", "secret")) != AuthzAllow {
		t.Fatal("Basic with matching user + password should allow")
	}
	if Authorize(withUser, basicHeader("admin", "secret")) != AuthzDeny {
		t.Fatal("Basic with wrong user must be denied")
	}

	// Unconfigured public credentials + no device store => allow anything.
	open := fakeState{}
	if Authorize(open, http.Header{}) != AuthzAllow {
		t.Fatal("unconfigured policy should allow")
	}
	if Authorize(open, basicHeader("u", "p")) != AuthzAllow {
		t.Fatal("unconfigured policy should allow Basic too")
	}
}

// Envelope builders produce observable JSON objects with exact key sets.
func TestEnvelopeKeySets(t *testing.T) {
	ok := BitcometOK("v-test", map[string]any{"extra": 1})
	wantOK := map[string]any{
		"error_code":       "OK",
		"error_message":    "",
		"version":          "v-test",
		"platform":         "proxy",
		"file_size_prefix": "binary",
		"extra":            1,
	}
	if !reflect.DeepEqual(ok, wantOK) {
		t.Fatalf("BitcometOK = %#v, want %#v", ok, wantOK)
	}

	// data overrides base keys (Rust merges after building the base map).
	override := BitcometOK("v-test", map[string]any{"error_code": "X"})
	if override["error_code"] != "X" {
		t.Fatalf("data should override base keys: %#v", override)
	}

	status, errBody := BitcometErr("v-test", http.StatusUnauthorized, ECInvalidToken, "msg")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	wantErr := map[string]any{
		"error_code":       "INVALID_TOKEN",
		"error_message":    "msg",
		"version":          "v-test",
		"platform":         "proxy",
		"file_size_prefix": "binary",
	}
	if !reflect.DeepEqual(errBody, wantErr) {
		t.Fatalf("BitcometErr = %#v, want %#v", errBody, wantErr)
	}

	tl := BitcometTaskList("v-test", 2, []any{map[string]any{"a": 1}})
	wantTL := map[string]any{
		"error_code":          "OK",
		"filtered_task_count": int64(2),
		"platform":            "proxy",
		"tasks":               []any{map[string]any{"a": 1}},
		"version":             "v-test",
	}
	if !reflect.DeepEqual(tl, wantTL) {
		t.Fatalf("BitcometTaskList = %#v, want %#v", tl, wantTL)
	}

	// nil tasks must serialise as [] not null.
	tlNil := BitcometTaskList("v-test", 0, nil)
	if tasks, ok := tlNil["tasks"].([]any); !ok || tasks == nil {
		t.Fatalf("nil tasks should become empty []any, got %#v", tlNil["tasks"])
	}
}

// Extra: BearerOf edge cases (strict "len > prefix.len()" from the Rust ref).
func TestBearerOfEdges(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   *string
	}{
		{"absent", "", nil},
		{"empty scheme only", "Bearer ", nil},
		{"exactly prefix", "bearer ", nil},
		{"mixed case", "BeArEr tok", ptr("tok")},
		{"extra spaces trimmed", "  Bearer   tok  ", ptr("tok")},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.header != "" {
			h.Set("Authorization", tc.header)
		}
		got := BearerOf(h)
		if (got == nil) != (tc.want == nil) {
			t.Fatalf("%s: nil-ness mismatch, got %v", tc.name, got)
		}
		if got != nil && *got != *tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, *got, *tc.want)
		}
	}
}
