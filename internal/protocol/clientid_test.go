package protocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ported from crates/bitcomet-api/src/clientid.rs `mod tests`.

func TestNewUUIDHasV4Shape(t *testing.T) {
	// Rust: uuid_has_v4_shape
	id := NewUUID()
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("UUID 应有 5 段: %s", id)
	}
	wantLens := []int{8, 4, 4, 4, 12}
	for i, n := range wantLens {
		if len(parts[i]) != n {
			t.Fatalf("段 %d 长度 = %d, want %d: %s", i, len(parts[i]), n, id)
		}
	}
	if !strings.HasPrefix(parts[2], "4") {
		t.Fatalf("版本位应为 4: %s", id)
	}
	switch parts[3][0] {
	case '8', '9', 'a', 'b':
	default:
		t.Fatalf("variant 位应符合 RFC4122: %s", id)
	}
}

func TestTwoUUIDsDiffer(t *testing.T) {
	// Rust: two_uuids_differ
	if NewUUID() == NewUUID() {
		t.Fatal("两次生成的 UUID 不应相同")
	}
}

func TestCreatesThenReusesClientID(t *testing.T) {
	// Rust: creates_then_reuses_client_id
	dir := t.TempDir()
	path := filepath.Join(dir, "client_id")

	first, err := LoadOrCreateClientID(path)
	if err != nil {
		t.Fatalf("首次应生成: %v", err)
	}
	if strings.TrimSpace(first) == "" {
		t.Fatal("首次生成的 client_id 不应为空")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("应已落盘 %s: %v", path, err)
	}

	second, err := LoadOrCreateClientID(path)
	if err != nil {
		t.Fatalf("再次应复用: %v", err)
	}
	if first != second {
		t.Fatalf("两次读取必须得到同一个 client_id: %s != %s", first, second)
	}

	regen, err := RegenerateClientID(path)
	if err != nil {
		t.Fatalf("应能重建: %v", err)
	}
	if regen == first {
		t.Fatalf("重建后应是全新的 id: %s", regen)
	}
}

func TestEmptyFileIsTreatedAsMissing(t *testing.T) {
	// Rust: empty_file_is_treated_as_missing
	dir := t.TempDir()
	path := filepath.Join(dir, "client_id")
	if err := os.WriteFile(path, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := LoadOrCreateClientID(path)
	if err != nil {
		t.Fatalf("空文件应触发生成: %v", err)
	}
	if strings.TrimSpace(id) == "" {
		t.Fatal("生成结果不应为空")
	}
}

func TestStateDirEnvTakesPriority(t *testing.T) {
	// Rust: state_dir_env_takes_priority
	// 容器场景回归：DISPATCH_STATE_DIR 必须优先于其它来源。
	marker := filepath.Join(t.TempDir(), "dg-state-dir-probe")
	t.Setenv(StateDirEnv, marker)
	if got := ConfigDir(); got != marker {
		t.Fatalf("应优先采用 %s: got %q, want %q", StateDirEnv, got, marker)
	}
}

// Ported from crates/bitcomet-api/tests/client_id_persistence.rs.

func TestExplicitPathIsPersistedAndStable(t *testing.T) {
	// Rust: explicit_path_is_persisted_and_stable
	// 显式指定路径时：两次构造必须复用同一个 id，且文件确实落盘
	// （Rust 走 Client::new；这里等价地在 clientid 层验证同一可观测终态）。
	dir := t.TempDir()
	path := filepath.Join(dir, "client_id")

	id1, err := LoadOrCreateClientID(path)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := LoadOrCreateClientID(path)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("两次构造必须复用同一个 client_id: %s != %s", id1, id2)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取落盘文件: %v", err)
	}
	if strings.TrimSpace(string(onDisk)) != id1 {
		t.Fatalf("落盘内容 %q 应等于内存 client_id %q", strings.TrimSpace(string(onDisk)), id1)
	}
}

func TestDefaultPathPersistsUnderStateDirEnv(t *testing.T) {
	// Rust: default_profile_persists_under_state_dir_env
	dir := t.TempDir()
	t.Setenv(StateDirEnv, dir)

	if got := ConfigDir(); got != dir {
		t.Fatalf("%s 应被 ConfigDir() 优先采用: got %q", StateDirEnv, got)
	}
	id, err := LoadOrCreateClientID(DefaultClientIDPath())
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(dir, "client_id")
	onDisk, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("client_id 应落到 %s 下: %v", StateDirEnv, err)
	}
	if strings.TrimSpace(string(onDisk)) != id {
		t.Fatalf("落盘内容应等于内存 client_id")
	}
}

func TestExistingIDIsReusedNotRegenerated(t *testing.T) {
	// Rust: existing_id_is_reused_not_regenerated
	// 磁盘上已有 id 时必须复用，不得重新生成，且落盘内容不得被改写。
	dir := t.TempDir()
	t.Setenv(StateDirEnv, dir)
	path := filepath.Join(dir, "client_id")
	known := "11111111-1111-4111-8111-111111111111"
	if err := os.WriteFile(path, []byte(known), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadOrCreateClientID(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != known {
		t.Fatalf("磁盘上已有 client_id 时必须复用: got %q, want %q", got, known)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(onDisk)) != known {
		t.Fatalf("落盘内容不应被改写: %q", strings.TrimSpace(string(onDisk)))
	}
}

// Extra: an invalid-UTF-8 file is treated as missing (Rust's read_to_string
// would err on invalid UTF-8 and fall through to generation).
func TestInvalidUTF8FileIsTreatedAsMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client_id")
	if err := os.WriteFile(path, []byte{0xff, 0xfe, 0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := LoadOrCreateClientID(path)
	if err != nil {
		t.Fatalf("非法 UTF-8 应触发生成: %v", err)
	}
	if strings.TrimSpace(id) == "" {
		t.Fatal("生成结果不应为空")
	}
	// 落盘内容应被新 UUID 覆盖
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(onDisk)) != id {
		t.Fatalf("应把新 id 落盘覆盖脏内容")
	}
}
