// `reset-password` 子命令的测试。
//
// 覆盖两类东西：
//  1. **纯输入校验**（在打开数据库之前就该被拦住）—— 口令太短、参数冲突、多余参数；
//  2. **落库效果**（真的写了哈希 / 真的清掉了 / 真的留了审计）。
//
// 第 2 类是重点：这条命令的全部价值就是「忘了口令还能回去」，
// 如果它只是打印一句「已重置」而库里没变，那是最坏的一种假成功。
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/adminpass"
	"github.com/LIUBANGJIAN/download-gateway/internal/config"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// newTempDB 建一个**已迁移**的临时库，并把配置指向它。
//
// 迁移目录用相对路径 "migrations"：`go test` 的工作目录就是包目录，
// 与生产容器里 `app` 用户读 `/opt/download-gateway/migrations` 的道理相同 ——
// 都要求迁移脚本随二进制分发，不能靠外部注入。
func newTempDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "dispatch.db")
	st, err := store.Open(dbPath, config.DefaultMigrations)
	if err != nil {
		t.Fatalf("准备临时库失败: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("关闭临时库失败: %v", err)
	}
	t.Setenv(config.EnvDB, dbPath)
	t.Setenv(config.EnvMigrations, config.DefaultMigrations)
	return dbPath
}

// securityAuditCount 数一数 event_log 里口令相关的审计条数。
func securityAuditCount(t *testing.T, dbPath string) int {
	t.Helper()
	st, err := store.Open(dbPath, config.DefaultMigrations)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	rows, err := st.Query(context.Background(),
		"SELECT COUNT(*) FROM event_log WHERE category = 'security'")
	if err != nil {
		t.Fatalf("查审计失败: %v", err)
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return 0
	}
	switch v := rows[0][0].(type) {
	case int64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

// 指定的新口令必须能登录：库里那条哈希要能校验通过。
func TestResetPasswordWritesVerifiableHash(t *testing.T) {
	dbPath := newTempDB(t)
	const pw = "correct horse battery"

	if err := runResetPassword([]string{"-password", pw}); err != nil {
		t.Fatalf("重置失败: %v", err)
	}

	st, err := store.Open(dbPath, config.DefaultMigrations)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	phc, found := adminpass.LoadHash(context.Background(), st)
	if !found {
		t.Fatal("库中没有写入口令哈希")
	}
	if !adminpass.VerifyHash(phc, pw) {
		t.Fatal("写入的哈希无法用新口令校验通过")
	}
	if adminpass.VerifyHash(phc, "wrong password") {
		t.Fatal("错误口令竟然通过了校验")
	}
	if n := securityAuditCount(t, dbPath); n != 1 {
		t.Fatalf("应留下 1 条安全审计，实际 %d 条", n)
	}
}

// `-clear` 必须把两条键都删掉，让启动逻辑回到「环境变量 / 随机口令」。
func TestResetPasswordClearRemovesBothKeys(t *testing.T) {
	dbPath := newTempDB(t)
	if err := runResetPassword([]string{"-password", "temporary-pass-1"}); err != nil {
		t.Fatalf("预置口令失败: %v", err)
	}
	if err := runResetPassword([]string{"-clear"}); err != nil {
		t.Fatalf("清除失败: %v", err)
	}

	st, err := store.Open(dbPath, config.DefaultMigrations)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	if _, found := adminpass.LoadHash(ctx, st); found {
		t.Fatal("清除后仍能读到口令哈希")
	}
	if got := adminpass.LoadUpdatedAt(ctx, st); got != 0 {
		t.Fatalf("清除后修改时刻应为 0，实际 %d", got)
	}
	// 幂等：再清一次也不算失败（用户可能连点了两下，或不确定上次成没成）。
	if err := runResetPassword([]string{"-clear"}); err != nil {
		t.Fatalf("重复清除应成功（幂等），实际: %v", err)
	}
}

// 纯输入错误必须在**碰数据库之前**被拦住：库文件不存在时也不该被这条路径创建出来。
func TestResetPasswordRejectsBadInputBeforeTouchingDB(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there.db")
	t.Setenv(config.EnvDB, missing)
	t.Setenv(config.EnvMigrations, config.DefaultMigrations)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"口令过短", []string{"-password", "short"}, "至少"},
		{"清空与指定互斥", []string{"-clear", "-password", "0123456789"}, "不能同时使用"},
		{"多余位置参数", []string{"extra"}, "无法识别的参数"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := runResetPassword(c.args)
			if err == nil {
				t.Fatal("应报错，实际成功")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应包含 %q，实际 %q", c.want, err.Error())
			}
			if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
				t.Fatal("输入校验失败时不应创建数据库文件")
			}
		})
	}
}

// 库不存在时要给出「先启动一次」的明确指引，而不是 SQLite 的 `unable to open database file`。
func TestResetPasswordMissingDBHasActionableError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there.db")
	t.Setenv(config.EnvDB, missing)
	t.Setenv(config.EnvMigrations, config.DefaultMigrations)

	err := runResetPassword([]string{"-clear"})
	if err == nil {
		t.Fatal("库不存在时应报错")
	}
	if !strings.Contains(err.Error(), "不存在") || !strings.Contains(err.Error(), "启动一次") {
		t.Fatalf("错误信息应指明「库不存在，先启动一次」，实际 %q", err.Error())
	}
}
