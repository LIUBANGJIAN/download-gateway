// Package adminpass 负责管理口令的**哈希、校验与持久化**。
//
// # 为什么独立成一个“叶子包”
//
// 需要它的有两处：`internal/state`（状态容器，要在登录时校验）与 `internal/admin`
// （HTTP handler，要提供“改口令”接口）。而 `admin → state` 已经是既成依赖；
// 若把哈希函数放进 `admin`，`state` 反向依赖 `admin` 就成环；放进 `state`，
// `admin` 又得穿过状态容器去调纯函数。
//
// 下沉到本包后两边都只**单向**依赖它。依赖仅：标准库 + `internal/store`（层 0），
// 因此在 layerguard 的 rank 表里登记为**层 1**（与 `config` / `secret` 同级）。
//
// # 与 Rust 侧必须逐字节兼容
//
// 哈希格式与 `crates/dispatch-core/src/admin/password.rs` 完全一致：
// argon2id / v=19 / m=19456 KiB, t=2, p=1 / 16 字节盐 / 32 字节输出，
// PHC 串用 RFC 4648 **标准**字母表的 **raw**（无 `=` 填充）base64。
//
// 这条兼容性是硬要求：用户可能先用 Rust 版在管理台设过口令，之后换成 Go 版，
// 同一个 `dispatch.db` 必须还能登录。已由 `scripts/password-e2e.sh` 的跨实现用例覆盖
// （Go 进程直接读 Rust 进程写下的库）。
package adminpass

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// app_config 里的键名（与 Rust 侧 `KEY_HASH` / `KEY_UPDATED_AT` 逐字一致）。
const (
	// KeyHash 落库的 argon2id PHC 串。
	KeyHash = "admin.password_hash"
	// KeyUpdatedAt 上次修改时刻（Unix 秒）。
	KeyUpdatedAt = "admin.password_updated_at"
)

// 新口令长度限制（按**字符**计，与 Rust 侧 `MIN_LEN` / `MAX_LEN` 一致）。
const (
	// MinLen 最少字符数。
	MinLen = 10
	// MaxLen 最多字符数。
	MaxLen = 128
)

// argon2id 参数。改动这里会让**旧的哈希仍可校验**（参数写在 PHC 串里），
// 但新哈希会用新参数 —— 无需迁移。
const (
	argonMemory      uint32 = 19456 // KiB，OWASP 推荐档
	argonIterations  uint32 = 2
	argonParallelism uint8  = 1
	argonSaltLen            = 16
	argonKeyLen      uint32 = 32
)

// 解析 PHC 串时的上限，防止被篡改的库把进程拖死（一条 `m=4294967295` 就够呛）。
const (
	maxMemory      uint32 = 1 << 20 // 1 GiB
	maxIterations  uint32 = 32
	maxParallelism uint8  = 16
)

// ErrWeakPassword 表示新口令不满足长度/字符要求。
var ErrWeakPassword = errors.New("新口令不符合要求")

// Credential 是管理口令的**当前凭据**，支持运行时热替换。
//
// 零值不可用，必须用 [NewCredential]。
type Credential struct {
	mu    sync.RWMutex
	hash  string // 非空 = 用户在管理台设过，走 argon2id
	plain string // hash 为空时用它（环境变量注入 / 启动时随机生成）
	// temp 是「登录页自助找回」用的一次性临时口令，详见 temp.go。
	// 它与上面的真实口令**并存**：原口令照常可用，临时口令单独受 TTL 与频率约束。
	temp tempState
	// now 是时钟注入点（测试用）；nil 时取系统时间。
	now func() int64
}

// clock 返回当前 Unix 秒。抽出来是为了让 TTL 用例不必真的 sleep 10 分钟。
func (c *Credential) clock() int64 {
	if c.now != nil {
		return c.now()
	}
	return time.Now().Unix()
}

// NewCredential 用初始明文口令构造（环境变量或随机口令）。
func NewCredential(plain string) *Credential {
	return &Credential{plain: plain}
}

// Verify 校验输入口令。
//
// 有哈希走 argon2id；没有则与环境变量/随机口令做**定长定时**比较。
// 真实口令不匹配时再试一次性临时口令（登录页自助找回用，见 temp.go）——
// 它是**额外凭据**，不是替代品：原口令永远不会因为生成了临时口令而失效。
func (c *Credential) Verify(input string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	hash, plain := c.hash, c.plain
	c.mu.RUnlock()
	if hash != "" {
		if VerifyHash(hash, input) {
			return true
		}
	} else if subtle.ConstantTimeCompare([]byte(input), []byte(plain)) == 1 {
		return true
	}
	return c.VerifyTemp(input)
}

// SetHash 把凭据切换成落库后的哈希（改口令成功后调用）。
//
// 顺手清掉临时口令：口令已经换了，那串「救急用的」就不该再有效
// （否则改完口令的 10 分钟内，日志里的临时口令仍能登进来）。
func (c *Credential) SetHash(phc string) {
	c.mu.Lock()
	c.hash = phc
	c.plain = ""
	c.temp.clearLocked()
	c.mu.Unlock()
}

// Source 返回当前凭据来源：`database` / `environment`（与 Rust `AdminCredential::source` 一致）。
func (c *Credential) Source() string {
	if c == nil {
		return "environment"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.hash != "" {
		return "database"
	}
	return "environment"
}

// ---------------------------------------------------------------------------
// 哈希
// ---------------------------------------------------------------------------

// Hash 生成 argon2id 的 PHC 串。
func Hash(plain string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成随机盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(plain), salt, argonIterations, argonMemory, argonParallelism, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		19, argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// parsedPHC 是解析后的 argon2id 哈希。
type parsedPHC struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	salt        []byte
	key         []byte
}

// parsePHC 解析 `$argon2id$v=19$m=..,t=..,p=..$salt$hash`。
//
// 只认 argon2id / v=19：其它算法或版本一律判失败（而不是“尽量理解”），
// 免得把一条格式怪异的串误判成通过。
func parsePHC(s string) (parsedPHC, bool) {
	parts := strings.Split(s, "$")
	// 首字符是 `$` ⇒ parts[0] 为空，正常共 6 段。
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return parsedPHC{}, false
	}

	var memory, iterations uint64
	var parallelism uint64
	for _, field := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			return parsedPHC{}, false
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return parsedPHC{}, false
		}
		switch k {
		case "m":
			memory = n
		case "t":
			iterations = n
		case "p":
			parallelism = n
		default:
			return parsedPHC{}, false
		}
	}
	if memory == 0 || memory > uint64(maxMemory) ||
		iterations == 0 || iterations > uint64(maxIterations) ||
		parallelism == 0 || parallelism > uint64(maxParallelism) {
		return parsedPHC{}, false
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return parsedPHC{}, false
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return parsedPHC{}, false
	}
	return parsedPHC{
		memory:      uint32(memory),
		iterations:  uint32(iterations),
		parallelism: uint8(parallelism),
		salt:        salt,
		key:         key,
	}, true
}

// VerifyHash 校验口令与 PHC 串是否匹配。
//
// 串损坏时返回 false（不 panic、更不放行）。
func VerifyHash(phc, input string) bool {
	p, ok := parsePHC(phc)
	if !ok {
		return false
	}
	got := argon2.IDKey([]byte(input), p.salt, p.iterations, p.memory, p.parallelism, uint32(len(p.key)))
	return subtle.ConstantTimeCompare(got, p.key) == 1
}

// ValidateNew 校验新口令的**格式**（旧口令是否正确由调用方判断）。
func ValidateNew(next, current string) error {
	n := len([]rune(next))
	if n < MinLen {
		return fmt.Errorf("%w：至少 %d 个字符（当前 %d 个）", ErrWeakPassword, MinLen, n)
	}
	if n > MaxLen {
		return fmt.Errorf("%w：最多 %d 个字符（当前 %d 个）", ErrWeakPassword, MaxLen, n)
	}
	for _, r := range next {
		// 控制字符（换行、制表符等）在输入框里看不见，却会让「我以为输的是这个」变成谜案。
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w：不能包含控制字符（换行、制表符等）", ErrWeakPassword)
		}
	}
	if next == current {
		return fmt.Errorf("%w：新口令不能与当前口令相同", ErrWeakPassword)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 存取
// ---------------------------------------------------------------------------

// LoadHash 读库里的哈希。读失败只返回 false（调用方回退到环境变量/随机口令），
// 不让“读不出配置”把管理口整个打死。
func LoadHash(ctx context.Context, st *store.Store) (string, bool) {
	rows, err := st.Query(ctx, "SELECT value FROM app_config WHERE key = ?1", KeyHash)
	if err != nil || len(rows) == 0 || len(rows[0]) == 0 {
		return "", false
	}
	s, ok := rows[0][0].(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

// LoadUpdatedAt 读“上次修改时刻”；从未改过返回 0。
func LoadUpdatedAt(ctx context.Context, st *store.Store) int64 {
	rows, err := st.Query(ctx, "SELECT value FROM app_config WHERE key = ?1", KeyUpdatedAt)
	if err != nil || len(rows) == 0 || len(rows[0]) == 0 {
		return 0
	}
	switch v := rows[0][0].(type) {
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	case int64:
		return v
	default:
		return 0
	}
}

// ClearHash 删除库里的口令哈希与修改时刻，让凭据回退到「环境变量 / 启动时随机口令」。
//
// 仅供 `reset-password -clear` 这条**人工补救**路径使用：忘记口令时把库里的哈希清掉，
// 重启后按环境变量（或随机口令）登录。正常改口令走 [SaveHash]，不要用本函数。
//
// 与 [SaveHash] 一样，删除是幂等的：本来就没有这两行时不算失败 ——
// 「已经清过了」和「刚清掉」对调用者的意义相同，没必要让用户分辨。
func ClearHash(ctx context.Context, st *store.Store) error {
	if _, err := st.Execute(ctx,
		"DELETE FROM app_config WHERE key IN (?1, ?2)",
		KeyHash, KeyUpdatedAt,
	); err != nil {
		return fmt.Errorf("清除口令哈希失败: %w", err)
	}
	return nil
}

// SaveHash 落库一条哈希 + 修改时刻。
func SaveHash(ctx context.Context, st *store.Store, phc string, now int64) error {
	if _, err := st.Execute(ctx,
		"INSERT INTO app_config (key, value, updated_at) VALUES (?1, ?2, ?3) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
		KeyHash, phc, now,
	); err != nil {
		return fmt.Errorf("保存口令哈希失败: %w", err)
	}
	if _, err := st.Execute(ctx,
		"INSERT INTO app_config (key, value, updated_at) VALUES (?1, ?2, ?3) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
		KeyUpdatedAt, strconv.FormatInt(now, 10), now,
	); err != nil {
		return fmt.Errorf("保存口令修改时刻失败: %w", err)
	}
	return nil
}
