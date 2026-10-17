// 代理自签三段式握手：`ip_verify` / `login` / `device_token/get`。
//
// # 为什么要实现
//
// 真实 BitComet 插件在**探测阶段**就会打这三条；不实现 ⇒ 真客户端走不到
// `/api/task/http/add`，兼容面用真客户端**测不了**。它是入口，不是锦上添花。
//
// # 攻击面（如实写明）
//
// `client_id` 由客户端自报、**且同时是解密口令** ⇒ 任何人用自造的 `client_id`
// 加密任意明文都能通过 RNCryptor 解密。**「解密成功」零身份证明**，
// 该握手在语义上等价于「公开可握手」。真正承载凭据的是**密文里的 `password` 字段**。
// 因此 `verifyLoginCredentials` 里「已配置 `DISPATCH_PUBLIC_TOKEN` 时必须
// `password == token`」那一步是**承重墙**：缺了它，「配置了 token」的安全承诺
// 会被 device_token 签发链路整体绕过。
//
// token **未配置**时放行是**有意的内网可用性取舍**（等价于任务路由默认不校验）。
package ingress

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
	"github.com/LIUBANGJIAN/download-gateway/internal/state"
)

const (
	// handshakeBodyLimit 是握手请求体上限（64 KiB 显式闸门，独立于 Router 的 8 MiB）。
	handshakeBodyLimit = 64 * 1024
	// loginRatePerMin 是 `/api/webui/login` 按来源 IP 的限速阈值（次/分钟）。
	// 6800 是公开口，登录要做 PBKDF2×10000，是个 CPU 放大面。
	loginRatePerMin = 30
)

// loginReq 是 `/api/webui/login` 请求体。
type loginReq struct {
	// ClientID 客户端自报标识（同时是解密口令）。
	ClientID string `json:"client_id"`
	// Authentication Base64 的 RNCryptor v3 密文（含 username/password）。
	Authentication *string `json:"authentication"`
	// Bypass 免密码标记；我们**不支持** bypass，传 true 直接拒。
	Bypass *bool `json:"bypass"`
}

// deviceTokenReq 是 `/api/device_token/get` 请求体。
type deviceTokenReq struct {
	InviteToken *string `json:"invite_token"`
	DeviceID    *string `json:"device_id"`
	DeviceName  *string `json:"device_name"`
	Platform    *string `json:"platform"`
}

// 握手失败原因（与 Rust `LoginFail` 一一对应，仅用于日志）。
const (
	loginFailDecrypt            = "decrypt_failed"
	loginFailShape              = "shape_invalid"
	loginFailCredentialMismatch = "credential_mismatch"
)

// verifyLoginCredentials 校验登录凭据。
//
// 成功返回解出的 `username`（仅用于日志），**绝不**返回密文或口令。
func verifyLoginCredentials(st *state.PublicState, clientID, authentication string) (string, string) {
	plain, err := protocol.DecryptFromBase64(authentication, clientID)
	if err != nil {
		return "", loginFailDecrypt
	}
	var payload map[string]any
	if err := json.Unmarshal(plain, &payload); err != nil {
		return "", loginFailShape
	}
	pw, ok := payload["password"].(string)
	if !ok {
		return "", loginFailShape
	}
	username, _ := payload["username"].(string)

	// ★承重墙：token 已配置时，密文里的 password 必须等于它，
	// 否则 device_token 签发链路会把「配置了 token」的安全承诺整体绕过。
	if st.PublicToken != nil && !CtEq(pw, *st.PublicToken) {
		return "", loginFailCredentialMismatch
	}
	return username, ""
}

// webuiLogin 是第 2 段：口令登录，换取一次性 `invite_token`。
func (s *server) webuiLogin(w http.ResponseWriter, r *http.Request) {
	body, okBody := readBody(w, r)
	if !okBody || len(body) > handshakeBodyLimit {
		status, resp := BitcometErr(s.version, http.StatusRequestEntityTooLarge, ECInvalidRequest, "请求体过大")
		writeJSON(w, status, resp)
		return
	}
	tokens := s.st.Handshake.Tokens
	if tokens == nil {
		status, resp := BitcometErr(s.version, http.StatusInternalServerError, ECInvalidRequest, "握手令牌表未初始化")
		writeJSON(w, status, resp)
		return
	}

	now := ids.NowSecs()
	if !tokens.CheckLoginRate(clientIP(r), now) {
		slog.Warn("握手登录触发限速", "ip", clientIP(r).String(), "limit_per_min", loginRatePerMin)
		status, resp := BitcometErr(s.version, http.StatusTooManyRequests, ECInvalidRequest, "登录尝试过于频繁")
		writeJSON(w, status, resp)
		return
	}

	var req loginReq
	if err := decodeJSON(body, &req); err != nil {
		status, resp := BitcometErr(s.version, http.StatusBadRequest, ECInvalidRequest, err.Error())
		writeJSON(w, status, resp)
		return
	}
	if req.Bypass != nil && *req.Bypass {
		status, resp := BitcometErr(s.version, http.StatusBadRequest, ECInvalidRequest, "不支持 bypass 免密码登录")
		writeJSON(w, status, resp)
		return
	}
	if req.Authentication == nil {
		status, resp := BitcometErr(s.version, http.StatusUnauthorized, ECInvalidToken, "Invalid token")
		writeJSON(w, status, resp)
		return
	}

	prefix := clientIDPrefix(req.ClientID)
	authRequired := s.st.PublicToken != nil
	username, fail := verifyLoginCredentials(s.st, req.ClientID, *req.Authentication)
	if fail != "" {
		slog.Warn("handshake_login_failed", "client_id_prefix", prefix, "reason", fail)
		s.writeSecurityLogLevel(r, "warn", "handshake_login_failed", map[string]any{
			"stage":            "login",
			"reason":           fail,
			"client_id_prefix": prefix,
		})
		// 失败一律 401 + INVALID_TOKEN：不向调用方区分「配置缺失」与「凭据不符」。
		status, resp := BitcometErr(s.version, http.StatusUnauthorized, ECInvalidToken, "Invalid token")
		writeJSON(w, status, resp)
		return
	}

	invite := tokens.IssueInvite(req.ClientID, now)
	slog.Info("handshake_login_ok",
		"client_id_prefix", prefix, "auth_required", authRequired, "username_len", len(username))
	s.writeSecurityLogLevel(r, "info", "handshake_login_ok", map[string]any{
		"auth_required":    authRequired,
		"client_id_prefix": prefix,
		"username_len":     len(username),
	})
	writeJSON(w, http.StatusOK, BitcometOK(s.version, map[string]any{"invite_token": invite}))
}

// deviceTokenGet 是第 3 段：`Bearer <invite_token>` + `body.invite_token`
// 双校验，换取 `device_token`。
func (s *server) deviceTokenGet(w http.ResponseWriter, r *http.Request) {
	body, okBody := readBody(w, r)
	if !okBody || len(body) > handshakeBodyLimit {
		status, resp := BitcometErr(s.version, http.StatusRequestEntityTooLarge, ECInvalidRequest, "请求体过大")
		writeJSON(w, status, resp)
		return
	}
	tokens := s.st.Handshake.Tokens
	if tokens == nil {
		status, resp := BitcometErr(s.version, http.StatusInternalServerError, ECInvalidRequest, "握手令牌表未初始化")
		writeJSON(w, status, resp)
		return
	}
	var req deviceTokenReq
	if err := decodeJSON(body, &req); err != nil {
		status, resp := BitcometErr(s.version, http.StatusBadRequest, ECInvalidRequest, err.Error())
		writeJSON(w, status, resp)
		return
	}

	now := ids.NowSecs()
	bearer := BearerOf(r.Header)
	hadBearer := bearer != nil

	// 若同时给了 Bearer 与 body.invite_token，二者必须一致。
	if bearer != nil && req.InviteToken != nil && *bearer != *req.InviteToken {
		s.writeSecurityLogLevel(r, "warn", "invite_rejected", map[string]any{
			"had_bearer":      hadBearer,
			"body_has_invite": true,
		})
		status, resp := BitcometErr(s.version, http.StatusUnauthorized, ECInvalidToken, "Invalid token")
		writeJSON(w, status, resp)
		return
	}

	invite := bearer
	if invite == nil {
		invite = req.InviteToken
	}
	if invite == nil {
		status, resp := BitcometErr(s.version, http.StatusUnauthorized, ECInvalidToken, "Invalid token")
		writeJSON(w, status, resp)
		return
	}

	deviceID, redeemed := tokens.RedeemInvite(*invite, now)
	if !redeemed {
		s.writeSecurityLogLevel(r, "warn", "invite_rejected", map[string]any{
			"had_bearer":      hadBearer,
			"body_has_invite": req.InviteToken != nil,
		})
		status, resp := BitcometErr(s.version, http.StatusUnauthorized, ECInvalidToken, "Invalid token")
		writeJSON(w, status, resp)
		return
	}

	token := tokens.IssueDevice(deviceID)
	prefix := clientIDPrefix(deviceID)
	slog.Info("device_token_issued", "device_id_prefix", prefix)
	s.writeSecurityLogLevel(r, "info", "device_token_issued", map[string]any{
		"client_id_prefix": prefix,
		"device_id_prefix": prefix,
	})
	writeJSON(w, http.StatusOK, BitcometOK(s.version, map[string]any{"device_token": token}))
}

// clientIDPrefix 取 client_id 前 8 个**字符**（日志脱敏：绝不记全串）。
func clientIDPrefix(clientID string) string {
	runes := []rune(clientID)
	if len(runes) > 8 {
		runes = runes[:8]
	}
	return string(runes)
}

// clientIP 取来源 IP。**不读** `X-Forwarded-For`：那由调用方任意伪造，
// 拿它做限流等于把限流关掉。反代部署时按部署文档配可信代理。
func clientIP(r *http.Request) netip.Addr {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr()
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return addr
	}
	return netip.Addr{}
}
