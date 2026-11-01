// Package adminpass 是管理台凭据的**唯一**实现：拿部署时注入的明文，做定长比较。
//
// # 口令只存在于两处：docker-compose.yml 与进程内存
//
// 从本轮起，管理账号密码**不再落库、不再由界面修改**：
//
//   - 库里 `app_config` 不再有 `admin.password_hash` / `admin.password_updated_at`
//     （V2 迁移会清掉老部署残留的两行）；
//   - `/api/admin/password` 与 `/api/admin/password/reset-request` 两个入口已删除；
//   - `reset-password` 子命令、一次性临时口令（原 temp.go）整套删除。
//
// # 为什么砍掉「库里存哈希」
//
// 上一版是「env 初始口令 + 库里哈希覆盖」的混合模式，代价是**两个真相源**：
// 在界面改过口令之后，compose 里那一行就悄悄失效了（日志会说「环境变量中的初始
// 口令不再参与校验」，但没人会天天翻启动日志）。于是「我明明配了 X，为什么登录
// 不上」成了一种必然会发生的问题；为了给这个死锁兜底，又得维护 reset-password
// 子命令和一次性临时口令 —— 三条补救路径全是混合模式自己造出来的。
//
// 单来源之后只剩一句话：**compose 里写什么就是什么，改完重启即生效。**
// 「忘记口令」退化成宿主机操作（改 compose → `docker compose up -d`）。
// 能改 compose 的人本来就能改库、换镜像、删卷，所以这并没有降低安全性；
// 反过来，任何**不**依赖宿主机控制权的自助恢复都会额外长出一个可被攻击的面。
//
// # 没配口令时仍然随机生成
//
// 否则新部署第一步就卡死（进不去界面）。这条兜底只存在于内存与启动日志里，
// 不落库、重启即换 —— 日志里会明确提示「这是临时口令，请写进 compose」。
//
// # 为什么独立成一个「叶子包」
//
// 需要它的有两处：`internal/state`（登录时校验）与 `internal/admin`（登录 handler）。
// `admin → state` 已是既成依赖，把凭据放 `admin` 会让 `state` 反向依赖成环，
// 放 `state` 又让 `admin` 穿过状态容器去调纯函数。下沉到本包后两边都只单向依赖它。
// 依赖仅标准库，因此在 layerguard 的 rank 表里登记为**层 1**（与 `config` / `secret` 同级）。
package adminpass

import (
	"crypto/subtle"
	"strings"
	"sync"
)

// DefaultUser 是未配置 `DISPATCH_ADMIN_USER` 时的管理台登录用户名。
//
// 保持 admin 是为了向后兼容：老部署只配了口令，升级后前端仍按 admin 登录，
// 不该因为多了一个字段就登不进去。
const DefaultUser = "admin"

// Credential 是管理台凭据。构造后**只读**（`WithUser` 仅在启动阶段链式调用一次）。
//
// 零值不可用，必须用 [NewCredential]。
type Credential struct {
	mu    sync.RWMutex
	user  string
	plain string
}

// NewCredential 用部署时注入的明文口令构造（未配置时是启动随机生成的临时值）。
func NewCredential(plain string) *Credential {
	return &Credential{user: DefaultUser, plain: plain}
}

// WithUser 设置登录用户名（空白时保持默认）。返回自身便于链式调用。
func (c *Credential) WithUser(user string) *Credential {
	if c == nil {
		return c
	}
	if u := strings.TrimSpace(user); u != "" {
		c.mu.Lock()
		c.user = u
		c.mu.Unlock()
	}
	return c
}

// User 返回登录用户名（用于日志与界面提示，**不是**秘密）。
func (c *Credential) User() string {
	if c == nil {
		return DefaultUser
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.user == "" {
		return DefaultUser
	}
	return c.user
}

// VerifyUser 校验登录用户名。
//
// 用定长比较是为了与口令校验保持一致的手感（也避免把「用户名对不对」变成
// 一个可测的时间差）。长度不同时 ConstantTimeCompare 必然返回 0 —— 用户名本身
// 不是秘密，它在 docker-compose 里明文写着。
//
// 空白输入按 [DefaultUser] 处理：老前端（或直接 curl）不带 username 时，
// 只要部署用的就是默认用户名，照样能登录。
func (c *Credential) VerifyUser(input string) bool {
	// nil 必须在最前面挡掉：下面的 User() 对 nil 会回默认值，
	// 若不留这一行，`(*Credential)(nil).VerifyUser("admin")` 会返回 true ——
	// 一个「没配置凭据」的状态反而放行 admin。
	if c == nil {
		return false
	}
	want := c.User()
	got := strings.TrimSpace(input)
	if got == "" {
		got = DefaultUser
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// Verify 校验输入口令（与部署配置里的明文做定长定时比较）。
func (c *Credential) Verify(input string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	plain := c.plain
	c.mu.RUnlock()
	return subtle.ConstantTimeCompare([]byte(input), []byte(plain)) == 1
}
