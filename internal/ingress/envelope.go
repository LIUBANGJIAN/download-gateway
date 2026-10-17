// Package ingress implements the public-facing (:6800) ingress layer of the
// download-gateway.
//
// This file ports crates/dispatch-core/src/ingress/envelope.rs 1:1: the shared
// inbound-authorisation table (AuthPolicy/Authorize/BearerOf/TokenParamOf), the
// BitComet unified envelopes, and the error-code constants.
//
// 收敛到一个文件的原因（主理人合并裁决）：鉴权判定表只有一份实现，任何受保护
// 路由都调 Authorize，不得各自内联一遍；信封与错误码同处，避免多处分叉。
package ingress

import (
	"errors"
	"net/http"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
)

// BitComet error_code constants.
const (
	// ECOK is the BitComet success error_code.
	ECOK = "OK"
	// ECInvalidToken is the BitComet auth-failure error_code.
	ECInvalidToken = "INVALID_TOKEN"
	// ECInvalidRequest is the BitComet bad-request error_code.
	ECInvalidRequest = "INVALID_REQUEST"
	// Aria2Unauthorized is the Aria2 auth-failure error code.
	Aria2Unauthorized int64 = -1
	// Platform is the `platform` value the proxy reports (distinguishes it from
	// a real BitComet node).
	Platform = "proxy"
)

// Authz is the inbound-authorisation decision.
type Authz int

const (
	// AuthzAllow means the request is allowed.
	AuthzAllow Authz = iota
	// AuthzDeny means the request is rejected (respond 401 INVALID_TOKEN, or
	// aria2 -1).
	AuthzDeny
)

// AuthPolicy encapsulates the two inbound-auth sources: a proxy-issued
// device_token (highest priority) plus an optional public token.
//
// A nil PublicToken means "not configured" (no check).
type AuthPolicy struct {
	// PublicToken is DISPATCH_PUBLIC_TOKEN; nil means no check.
	PublicToken *string
}

// NewAuthPolicy builds an AuthPolicy from an optional public token.
func NewAuthPolicy(publicToken *string) AuthPolicy {
	return AuthPolicy{PublicToken: publicToken}
}

// Decide implements the decision table (design-2 §(4)) row by row:
//
//	| Bearer | is proxy device_token | public token configured | equal | result |
//	|:------:|:---------------------:|:-----------------------:|:-----:|:------:|
//	|  yes   |          yes          |          any            |   —   | Allow  |
//	|  yes   |          no           |          yes            |  yes  | Allow  |
//	|  yes   |          no           |          yes            | no/空 | Deny   |
//	|  no    |          —            |          yes            |   —   | Deny   |
//	|  yes   |          no           |          no             |   —   | Allow  |
//	|  no    |          —            |          no             |   —   | Allow  |
func (p AuthPolicy) Decide(bearer *string, deviceTokenValid bool) Authz {
	// Row 1: a token issued by this process has the highest priority.
	if deviceTokenValid {
		return AuthzAllow
	}
	if p.PublicToken == nil {
		// Rows 5 and 6.
		return AuthzAllow
	}
	// Rows 2, 3 and 4.
	if bearer != nil && CtEq(*bearer, *p.PublicToken) {
		return AuthzAllow
	}
	return AuthzDeny
}

// PublicAuthState is the slice of public state that inbound authorisation
// needs. It is an interface so that envelope.go does not depend on the
// (not-yet-written) internal/state package; *state.PublicState is expected to
// satisfy it.
//
// NOTE (待接线点): if internal/state settles on different method names, adapt
// here — the decision table itself is unchanged.
type PublicAuthState interface {
	// PublicTokenConfigured returns the configured public token and whether one
	// is configured at all.
	PublicTokenConfigured() (token string, configured bool)
	// IsDeviceToken reports whether token was issued by this proxy process.
	IsDeviceToken(token string) bool
}

// Authorize is the unified inbound check: every protected BitComet route calls
// it.
func Authorize(st PublicAuthState, headers http.Header) Authz {
	bearer := BearerOf(headers)
	deviceOK := false
	if bearer != nil {
		deviceOK = st.IsDeviceToken(*bearer)
	}
	var publicToken *string
	if tok, configured := st.PublicTokenConfigured(); configured {
		publicToken = &tok
	}
	return NewAuthPolicy(publicToken).Decide(bearer, deviceOK)
}

// headerValue performs a case-insensitive header lookup, mirroring axum's
// HeaderMap::get (which is case-insensitive). Returns "" when absent or empty.
func headerValue(h http.Header, name string) string {
	if vs, ok := h[http.CanonicalHeaderKey(name)]; ok && len(vs) > 0 {
		return vs[0]
	}
	for k, vs := range h {
		if len(vs) > 0 && strings.EqualFold(k, name) {
			return vs[0]
		}
	}
	return ""
}

// BearerOf extracts the token from an `Authorization: Bearer <t>` header
// (case-insensitive prefix). Returns nil when the header is missing, not a
// Bearer scheme, or has an empty token (mirrors the strict `len > prefix.len()`
// check in the Rust reference).
func BearerOf(headers http.Header) *string {
	raw := strings.TrimSpace(headerValue(headers, "Authorization"))
	const prefix = "bearer "
	if len(raw) > len(prefix) && strings.EqualFold(raw[:len(prefix)], prefix) {
		v := strings.TrimSpace(raw[len(prefix):])
		return &v
	}
	return nil
}

// TokenErr values (mirror of the Rust TokenErr enum; messages are contract).
var (
	// ErrTokenMissing is returned when a token is required but absent.
	ErrTokenMissing = errors.New("缺少 token")
	// ErrTokenMismatch is returned when the supplied token does not match.
	ErrTokenMismatch = errors.New("token 不匹配")
)

// TokenParamOf extracts and validates the `"token:<t>"` element of an Aria2
// `params` array.
//
// Behaviour: it REMOVES every string param starting with `token:` from *params;
// when expected is non-nil it requires the token to be present and equal, and
// errors otherwise; when expected is nil (not configured) it allows either.
//
// *params is passed by pointer so that, like the Rust `&mut Vec<Value>`, the
// caller observes the removal (Go slices do not shrink through the caller's
// own header otherwise).
func TokenParamOf(params *[]any, expected *string) error {
	var found *string
	p := *params
	i := 0
	for i < len(p) {
		if s, ok := p[i].(string); ok && strings.HasPrefix(s, "token:") {
			tok := s[len("token:"):]
			found = &tok
			p = append(p[:i], p[i+1:]...)
			continue
		}
		i++
	}
	*params = p

	if expected == nil {
		return nil
	}
	if found == nil {
		return ErrTokenMissing
	}
	if CtEq(*found, *expected) {
		return nil
	}
	return ErrTokenMismatch
}

// CtEq is a length-checked, constant-time string comparison (self-written, no
// subtle dependency) to avoid timing side channels.
func CtEq(a, b string) bool {
	ab, bb := []byte(a), []byte(b)
	if len(ab) != len(bb) {
		return false
	}
	var diff byte
	for i := range ab {
		diff |= ab[i] ^ bb[i]
	}
	return diff == 0
}

// IsOKCode re-exports protocol.IsOKCode (mirror of envelope.rs's
// `pub use bitcomet_api::is_ok_code`): the proxy-side "inbound success value is
// case-insensitive" check (OK / ok / empty all count as success).
func IsOKCode(code string) bool { return protocol.IsOKCode(code) }

// BitcometOK builds the unified BitComet success envelope plus business fields
// (`platform:"proxy"`). Entries of data overwrite the base keys.
//
// `version` is passed in because Go has no crate-level VERSION constant here; it
// is injected in main via ldflags.
func BitcometOK(version string, data map[string]any) map[string]any {
	m := map[string]any{
		"error_code":       ECOK,
		"error_message":    "",
		"version":          version,
		"platform":         Platform,
		"file_size_prefix": "binary",
	}
	for k, v := range data {
		m[k] = v
	}
	return m
}

// BitcometErr builds the unified failure envelope (error code + readable
// message). The HTTP status is returned alongside so the caller writes it.
func BitcometErr(version string, httpStatus int, code, msg string) (int, map[string]any) {
	body := map[string]any{
		"error_code":       code,
		"error_message":    msg,
		"version":          version,
		"platform":         Platform,
		"file_size_prefix": "binary",
	}
	return httpStatus, body
}

// BitcometTaskList builds the `/api_v2/task_list/get` envelope: the top-level
// key set is aligned exactly with the T02-measured set.
func BitcometTaskList(version string, count int64, tasks []any) map[string]any {
	if tasks == nil {
		tasks = []any{}
	}
	return map[string]any{
		"error_code":          ECOK,
		"filtered_task_count": count,
		"platform":            Platform,
		"tasks":               tasks,
		"version":             version,
	}
}
