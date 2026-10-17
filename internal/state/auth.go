// 本文件补齐 `internal/ingress/envelope.go` 中 `PublicAuthState` 接口所要求的两个
// 方法（该文件里以「NOTE (待接线点)」标注）。放在 `internal/state` 而不是 `ingress`，
// 因为**判定表**属于接入层、而**状态从哪儿取**属于状态容器 —— 两者分开才不会让
// 「多个 handler 各自内联一遍鉴权」的历史问题复发。
package state

// PublicTokenConfigured 返回对外口共享令牌及其是否已配置。
//
// 语义对齐 Rust `PublicState::public_token` 的读取：`nil` ⇒ 不校验（`configured=false`）。
// 注意**空串**与 `nil` 不同：环境变量显式设为空串时 `config.envOpt` 已按「空白视为未设置」
// 折叠为 nil，故此处不会出现「配了空串令牌 ⇒ 人人可过」的静默放宽。
func (s *PublicState) PublicTokenConfigured() (string, bool) {
	if s == nil || s.PublicToken == nil {
		return "", false
	}
	return *s.PublicToken, true
}

// IsDeviceToken 判定 token 是否由**本进程**签发（三段式握手产出的 device_token）。
//
// 令牌表在内存里，重启即失效 —— 因此重启后所有已配对客户端必须重新握手一次，
// 这与 Rust 侧 `HandshakeStore` 的内存语义一致。
func (s *PublicState) IsDeviceToken(token string) bool {
	if s == nil || s.Handshake.Tokens == nil || token == "" {
		return false
	}
	return s.Handshake.Tokens.IsDevice(token)
}
