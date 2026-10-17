package ingress

import (
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

func TestDecisionTableRows(t *testing.T) {
	// Rust: decision_table_rows
	configured := NewAuthPolicy(ptr("secret"))
	unconfigured := NewAuthPolicy(nil)

	cases := []struct {
		name   string
		policy AuthPolicy
		bearer *string
		device bool
		want   Authz
	}{
		{"row1 device_token wins", configured, ptr("x"), true, AuthzAllow},
		{"row2 public token matches", configured, ptr("secret"), false, AuthzAllow},
		{"row3 public token mismatch", configured, ptr("wrong"), false, AuthzDeny},
		{"row4 no bearer, configured", configured, nil, false, AuthzDeny},
		{"row5 unconfigured, bearer", unconfigured, ptr("whatever"), false, AuthzAllow},
		{"row6 unconfigured, no bearer", unconfigured, nil, false, AuthzAllow},
	}
	for _, tc := range cases {
		if got := tc.policy.Decide(tc.bearer, tc.device); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
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
	token   *string
	devices map[string]bool
}

func (f fakeState) PublicTokenConfigured() (string, bool) {
	if f.token == nil {
		return "", false
	}
	return *f.token, true
}

func (f fakeState) IsDeviceToken(t string) bool { return f.devices[t] }

// Authorize is the real entry point every protected route uses. envelope.rs
// doesn't unit-test it directly, so this pins its observable decisions.
func TestAuthorizeWiring(t *testing.T) {
	st := fakeState{token: ptr("secret"), devices: map[string]bool{"devtok": true}}
	bearerHeader := func(v string) http.Header {
		h := http.Header{}
		h.Set("Authorization", v)
		return h
	}

	if Authorize(st, bearerHeader("Bearer devtok")) != AuthzAllow {
		t.Fatal("device token should be allowed")
	}
	if Authorize(st, bearerHeader("Bearer secret")) != AuthzAllow {
		t.Fatal("matching public token should be allowed")
	}
	if Authorize(st, bearerHeader("Bearer nope")) != AuthzDeny {
		t.Fatal("wrong token should be denied")
	}
	if Authorize(st, http.Header{}) != AuthzDeny {
		t.Fatal("no header with configured token should be denied")
	}

	// Unconfigured public token + no device store => allow anything.
	open := fakeState{}
	if Authorize(open, http.Header{}) != AuthzAllow {
		t.Fatal("unconfigured policy should allow")
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
