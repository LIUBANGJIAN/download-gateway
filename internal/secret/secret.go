// Package secret 提供节点密码的**可逆**封装（供管理台「眼睛」按钮回看原文）。
//
// # 1:1 移植来源
//
// `crates/dispatch-core/src/secret.rs` 全量：`SecretBox`（`WithKey`/`Resolve`/`Wrap`/`Open`）、
// `KeyFilePath`、密钥文件解析顺序、以及全部错误分支与**错误文案**。
//
// # 为什么复用 `internal/protocol`（RNCryptor v3），而不是引入新的加密库
//
// `node.pass_enc` 的需求是「**口令派生的对称可逆封装**」。`internal/protocol` 恰好提供
// `Encrypt`/`Decrypt` 这一对：RNCryptor v3 = PBKDF2-HMAC-SHA1(10000 轮) 派生密钥 →
// AES-256-CBC → HMAC-SHA256。它的**逐字节正确性**由跨语言互操作测试钉住。
// 为一个已在依赖树里、且已被验证过的实现另找算法，属于「用更高风险换更低收益」。
//
// # 主密钥从哪来（按优先级）
//
//  1. 环境变量 `DISPATCH_SECRET_KEY`（非空白即用；容器部署推荐显式注入）；
//  2. `<库文件所在目录>/node-secret.key` 文件 —— 不存在则**生成 32 字节随机并落盘**。
//
// 第 2 条是「开箱即用」的保证：不配任何环境变量，`docker compose up` 之后「眼睛」也能用，
// 且**重启后仍能解密**。若每次启动都换密钥，历史密文会全部变成废纸（安静型故障）。
//
// ⚠️ **诚实边界**：默认方案里主密钥与密文同处一台机器。它防的是**窥屏 / 截图 / 日志泄露**，
// **不防**已经拿到整机文件系统的人。要防后者，请用 `DISPATCH_SECRET_KEY` 把密钥挪到容器外。
package secret

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
)

// 常量（逐字对照 Rust）。
const (
	// KeyEnv 是主密钥的环境变量名。
	KeyEnv = "DISPATCH_SECRET_KEY"
	// KeyFileName 是落盘密钥的文件名（与库文件同目录）。
	KeyFileName = "node-secret.key"
	// keyBytes 是生成密钥的字节数（十六进制后 64 字符）。
	keyBytes = 32
	// keyMinLen 是主密钥的最小长度（环境变量注入时校验）。
	keyMinLen = 16
)

// 哨兵错误。文案是契约的一部分（下游判据按它断言），**逐字照搬** Rust：
//
//	Io(#[from] std::io::Error)  → "io: {0}"
//	Encrypt(String)             → "加密失败：{0}"
//	Decrypt(String)             → "解密失败：{0}"
//	NotUtf8                     → "解密结果不是合法 UTF-8"
//
// 用法：`fmt.Errorf("%w：%s", ErrEncrypt, detail)` 渲染为 `加密失败：detail`，
// 同时 errors.Is(err, ErrEncrypt) 成立。
var (
	// ErrIO 对应 Rust `SecretError::Io`（渲染前缀 `io: `）。
	ErrIO = errors.New("io")
	// ErrEncrypt 对应 Rust `SecretError::Encrypt`（渲染前缀 `加密失败：`）。
	ErrEncrypt = errors.New("加密失败")
	// ErrDecrypt 对应 Rust `SecretError::Decrypt`（渲染前缀 `解密失败：`）。
	ErrDecrypt = errors.New("解密失败")
	// ErrNotUTF8 对应 Rust `SecretError::NotUtf8`。
	ErrNotUTF8 = errors.New("解密结果不是合法 UTF-8")
)

// randReader 是随机源。生产恒为 crypto/rand.Reader；仅测试会替换它，
// 用来**真正触发**「随机源故障」这条降级分支（与 `internal/ids` 的策略一致）。
var randReader io.Reader = rand.Reader

// logf 是日志出口；仅测试会替换它，用来断言降级/告警分支确实「吵」了。
var logf = log.Printf

// SecretBox 是节点密码的加解密器（对照 Rust `struct SecretBox`）。
//
// 内部只持有一个字符串主密钥；Go 用指针传递（Rust 侧 `Clone` 廉价）。
type SecretBox struct {
	key string
}

// WithKey 用**显式密钥**构造（测试与 `DISPATCH_SECRET_KEY` 路径共用）。
//
// 对照 Rust `SecretBox::with_key`。
func WithKey(key string) *SecretBox {
	return &SecretBox{key: key}
}

// Resolve 解析主密钥：环境变量优先，其次密钥文件，最后生成。
//
// 返回 `(加解密器, generated, err)`；`generated == true` 表示**本次新生成**了密钥文件，
// 调用方应打印一次提示（用户至少该知道机器上多了一个文件）。
//
// 对照 Rust `SecretBox::resolve`。
func Resolve(dbPath string) (*SecretBox, bool, error) {
	// ① 环境变量优先：非空白即用（先 trim 再判空）。
	if v, ok := os.LookupEnv(KeyEnv); ok {
		t := strings.TrimSpace(v)
		if t != "" {
			if len(t) < keyMinLen {
				return nil, false, fmt.Errorf(
					"%w：%s 过短（%d 字符）：至少 %d 字符，否则派生强度不足",
					ErrEncrypt, KeyEnv, len(t), keyMinLen,
				)
			}
			return WithKey(t), false, nil
		}
	}

	// ② 密钥文件。
	file := KeyFilePath(dbPath)
	if b, err := os.ReadFile(file); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return WithKey(t), false, nil
		}
	}

	// ③ 生成并落盘。
	key := hex.EncodeToString(randomKeyBytes())
	if dir := filepath.Dir(file); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, false, fmt.Errorf("%w: %w", ErrIO, err)
		}
	}
	if err := os.WriteFile(file, []byte(key+"\n"), 0o644); err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrIO, err)
	}
	restrictPermissions(file)
	return WithKey(key), true, nil
}

// Wrap 加密明文，返回 Base64 密文（可直接存进 `node.pass_enc`）。
//
// 对照 Rust `SecretBox::wrap`（失败渲染为 `加密失败：<底层> `）。
func (s *SecretBox) Wrap(plaintext string) (string, error) {
	enc, err := protocol.EncryptToBase64([]byte(plaintext), s.key)
	if err != nil {
		return "", fmt.Errorf("%w：%s", ErrEncrypt, err.Error())
	}
	return enc, nil
}

// Open 解密密文（`node.pass_enc`）回明文。
//
// 对照 Rust `SecretBox::open`：解密失败渲染为 `解密失败：<底层> `；
// 解密成功但结果不是合法 UTF-8 时返回 `ErrNotUTF8`。
func (s *SecretBox) Open(tokenB64 string) (string, error) {
	raw, err := protocol.DecryptFromBase64(tokenB64, s.key)
	if err != nil {
		return "", fmt.Errorf("%w：%s", ErrDecrypt, err.Error())
	}
	if !utf8.Valid(raw) {
		return "", ErrNotUTF8
	}
	return string(raw), nil
}

// KeyFilePath 返回密钥文件路径：与库文件**同目录**，便于 `VOLUME /data` 一起持久化。
//
// 对照 Rust `key_file_path`：库路径无父目录（如裸 `dispatch.db`）时退化为当前目录下的
// `node-secret.key`，**不 panic**。
func KeyFilePath(dbPath string) string {
	dir := filepath.Dir(dbPath)
	if dir == "." || dir == "" {
		return KeyFileName
	}
	return filepath.Join(dir, KeyFileName)
}

// randomKeyBytes 读取 keyBytes 个密码学安全随机字节；失败时回落全零并**大声报错**。
//
// 与 Rust `random_key_bytes` 对齐：不 panic。标准库下此分支**不可达**
// （Go 1.24 起 `crypto/rand.Read` 不再返回错误），故它只可能被「注入失败 Reader」的
// 测试覆盖（见 secret_test.go::TestRandomKeyBytesFallsBackToZero）。
func randomKeyBytes() []byte {
	buf := make([]byte, keyBytes)
	if _, err := io.ReadFull(randReader, buf); err != nil {
		clear(buf)
		logf("生成节点密钥时获取安全随机数失败：已回落全零，请立即用 %s 显式注入密钥", KeyEnv)
	}
	return buf
}

// restrictPermissions 尽量收紧密钥文件权限（Unix 0600）。Windows 上无对应语义，跳过。
func restrictPermissions(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		logf("无法把密钥文件权限收紧到 0600：%v", err)
	}
}
