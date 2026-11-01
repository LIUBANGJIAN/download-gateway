// 本文件补齐 `internal/ingress/envelope.go` 中 `PublicAuthState` 接口所要求的方法
// （该文件里以「NOTE (待接线点)」标注）。放在 `internal/state` 而不是 `ingress`，
// 因为**判定表**属于接入层、而**状态从哪儿取**属于状态容器 —— 两者分开才不会让
// 「多个 handler 各自内联一遍鉴权」的历史问题复发。
package state

// PublicAuth 返回对外口的用户名 / 口令 / 是否已配置。
//
// 语义：`password == "" && !configured` ⇒ 不校验（`configured=false`）。
// 注意**空串**与未配置不同：环境变量显式设为空白时 `config.envOpt` 已折叠为 nil，
// 故此处不会出现「配了空串口令 ⇒ 人人可过」的静默放宽。
//
// 用户名为空表示**不校验用户名**（只认口令），这是给老部署的兼容路径：
// 它们只配过一个 token，没有用户名这个概念。
func (s *PublicState) PublicAuth() (user, password string, configured bool) {
	if s == nil || s.PublicPassword == nil {
		return "", "", false
	}
	if s.PublicUser != nil {
		user = *s.PublicUser
	}
	return user, *s.PublicPassword, true
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
