// Package protocol implements the on-the-wire primitives the download-gateway
// uses to talk to BitComet WebUI nodes.
//
// This file ports crates/bitcomet-api/src/clientid.rs 1:1.
//
// client_id 的生成与持久化。为什么必须持久化：client_id 既是设备标识，又是
// 登录密文（RNCryptor）的口令。实测表明：client_id 为空会导致登录静默失败；
// 每次运行重新生成也会让服务端视为新设备、无法稳定绑定。因此首次运行生成
// UUID v4 写入本地文件，之后复用。
//
// 路径选择（T02 相对参考实现的改造，见 clientid.rs 文档头）：
//
//  1. DISPATCH_STATE_DIR —— 容器里显式指向 /data（与 SQLite 同卷，一起持久化）
//  2. XDG_CONFIG_HOME —— Linux / macOS 惯例
//  3. APPDATA —— Windows 惯例（与参考实现行为一致）
//  4. HOME / USERPROFILE —— 兜底
//  5. 可执行文件目录 —— 最后手段
package protocol

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// AppDirName is the application state directory name.
const AppDirName = "download-gateway"

// StateDirEnv is the environment variable that explicitly selects the state
// directory (set to /data inside containers).
const StateDirEnv = "DISPATCH_STATE_DIR"

// ConfigDir returns the directory in which the client_id is stored.
//
// It mirrors clientid.rs::config_dir, including the asymmetry between the
// XDG_CONFIG_HOME / APPDATA branches (which append AppDirName directly) and the
// HOME / USERPROFILE branch (which appends ".config" then AppDirName).
func ConfigDir() string {
	if v := strings.TrimSpace(os.Getenv(StateDirEnv)); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		return filepath.Join(v, AppDirName)
	}
	if v := strings.TrimSpace(os.Getenv("APPDATA")); v != "" {
		return filepath.Join(v, AppDirName)
	}
	for _, key := range []string{"HOME", "USERPROFILE"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return filepath.Join(v, ".config", AppDirName)
		}
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// DefaultClientIDPath returns the default client_id persistence path.
func DefaultClientIDPath() string {
	return filepath.Join(ConfigDir(), "client_id")
}

// NewUUID returns a random (version 4, RFC 4122) UUID string in lowercase
// hyphenated form.
//
// As in the Rust reference (uuid::Uuid::new_v4), the signature is infallible;
// an entropy-source failure is treated as unrecoverable and panics.
func NewUUID() string {
	id, err := newUUID()
	if err != nil {
		panic(fmt.Sprintf("protocol: generate uuid: %v", err))
	}
	return id
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// LoadOrCreateClientID reads the client_id at path; if the file is missing,
// empty/whitespace-only, or not valid UTF-8, it generates a fresh UUID v4,
// persists it (creating parent directories as needed) and returns it.
// The returned value is guaranteed non-empty.
func LoadOrCreateClientID(path string) (string, error) {
	// A read error, invalid UTF-8, or blank content all fall through to
	// generation, matching `fs::read_to_string(..)` + `trim().is_empty()`.
	if data, err := os.ReadFile(path); err == nil && utf8.Valid(data) {
		if t := strings.TrimSpace(string(data)); t != "" {
			return t, nil
		}
	}

	id := NewUUID()
	if parent := filepath.Dir(path); parent != "" {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(id), 0o644); err != nil {
		return "", err
	}
	return id, nil
}

// RegenerateClientID deletes and recreates the client_id file, for the
// "file content corrupted, server rejected it" self-healing path.
func RegenerateClientID(path string) (string, error) {
	_ = os.Remove(path)
	return LoadOrCreateClientID(path)
}
