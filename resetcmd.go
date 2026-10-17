// `reset-password` 子命令：**忘记管理口令时的唯一合法补救路径**。
//
// # 为什么必须有这条命令
//
// 口令一旦在管理台改过，库里留下的是 argon2id 哈希；启动时 `main.go` 读到它就会
// **明确不再校验** `DISPATCH_ADMIN_PASSWORD`（日志原文：「环境变量中的初始口令不再参与校验」）。
// 哈希不可逆 —— 也就是说，用户忘了自己刚设的口令，就是死锁：
// 登录进不去 → 改不了 → 连界面上的「改口令」卡片都够不着。
//
// 在补这条命令之前，唯一的土办法是拿外部工具去改 `dispatch.db`：
// 运行镜像基于 alpine、以 uid 10001 运行、**不带 sqlite3**，
// 于是「把库拷出来 → 找台有 sqlite 的机器 → 删两行 → 拷回去」成了唯一出路，
// 或者干脆删库重来（任务、节点、审计全丢）。
// 对一个自托管产品来说，这等于没有恢复手段。
//
// # 为什么是子命令而不是「界面上的忘记密码」
//
// 自托管网关没有邮箱、没有短信、没有第二个管理员 —— 「忘记密码」流程里
// 用来证明「你是主人」的东西只能是**宿主机控制权**。
// 能 `docker exec` / 能改 compose 的人，本来就能改数据库、换镜像、删卷，
// 所以把恢复能力放在这里不降低安全性；反过来，任何**不**依赖宿主机控制权的
// 自助恢复（密保问题之类）都会额外长出一个可被攻击的面。
//
// # 三种用法
//
//	download-gateway reset-password                   生成随机口令并落库（推荐）
//	download-gateway reset-password -password <新口令>  指定新口令
//	download-gateway reset-password -clear             清掉口令，回退到环境变量/随机口令
//
// # ⚠️ 生效需要重启
//
// 本命令直接改库，**不通知**正在运行的服务进程 —— 那个进程的凭据在内存里，
// 它不会察觉库变了。所以输出末尾必须明确要求重启；不写这句，用户会以为命令没生效，
// 然后开始怀疑口令、怀疑大小写、怀疑输入法。
//
// # 与 Rust 侧的对齐
//
// 行为、参数、输出结构、审计文案与 `src/main.rs::reset_password_cmd` 保持一致：
// 两个实现对同一个 `dispatch.db` 必须能互相接管（先 Rust 起、再 Go 起，口令不变）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/adminpass"
	"github.com/LIUBANGJIAN/download-gateway/internal/config"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// ResetPasswordCmd 是重置管理口令的子命令名（与 Rust 侧逐字一致）。
const ResetPasswordCmd = "reset-password"

// resetPasswordUsage 是 `-h` 时打印的用法。
const resetPasswordUsage = `用法：
  download-gateway reset-password                    生成随机口令并写入数据库（推荐）
  download-gateway reset-password -password <新口令>   指定新口令（10–128 个字符）
  download-gateway reset-password -clear             清除口令，回退到 DISPATCH_ADMIN_PASSWORD / 启动时随机口令

说明：
  口令只写入 DISPATCH_DB 指向的数据库（默认 data/dispatch.db），不打印旧口令、也无法反推旧口令。
  命令本身不会重启服务；**改完必须重启**，运行中的进程才会重新读取口令。
`

// runResetPassword 执行 `reset-password` 子命令。返回错误由 [main] 打到 stderr 并以 1 退出。
//
// 参数解析用标准库 `flag`：`-password xxx`、`--password=xxx` 都接受（Go 的 flag 对单双横线一视同仁）。
func runResetPassword(args []string) error {
	fs := flag.NewFlagSet(ResetPasswordCmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		newPassword string
		clearOnly   bool
	)
	fs.StringVar(&newPassword, "password", "", "指定新管理口令（省略则随机生成）")
	fs.BoolVar(&clearOnly, "clear", false, "清除库中的管理口令，回退到环境变量/启动时随机口令")
	fs.Usage = func() { _, _ = io.WriteString(os.Stderr, resetPasswordUsage) }

	if err := fs.Parse(args); err != nil {
		// `-h` / `-help`：用法已由 fs.Usage 打印，按成功处理（`--help` 不该是错误退出）。
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("无法识别的参数 %q（本命令只接受 -password / -clear）",
			strings.Join(fs.Args(), " "))
	}
	if clearOnly && newPassword != "" {
		return errors.New("-clear 与 -password 不能同时使用：一个要清空、一个要写入")
	}
	// 格式先校验，**在打开数据库之前** —— 口令太短这种纯输入错误不该产生任何副作用。
	if newPassword != "" {
		if err := adminpass.ValidateNew(newPassword, ""); err != nil {
			return err
		}
	}

	cfg := config.FromEnv()
	// 先看库在不在：`store.Open` 会顺手建库 + 迁移，在「路径打错」的情况下
	// 静默造出一个空库，然后用户以为重置成功了，其实改的是另一个文件。
	if _, err := os.Stat(cfg.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("数据库 %s 不存在：请先启动一次服务完成初始化，或检查 DISPATCH_DB", cfg.DBPath)
		}
		return fmt.Errorf("检查数据库 %s 失败: %w", cfg.DBPath, err)
	}

	st, err := store.Open(cfg.DBPath, cfg.MigrationsDir)
	if err != nil {
		return fmt.Errorf("打开数据库 %s 失败: %w", cfg.DBPath, err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	now := ids.NowSecs()

	if clearOnly {
		if err := adminpass.ClearHash(ctx, st); err != nil {
			return err
		}
		auditPasswordReset(ctx, st, now, "管理口令已通过命令行清除（reset-password -clear）")
		fmt.Fprintf(os.Stdout, "已清除数据库 %s 中的管理口令。\n\n", cfg.DBPath)
		fmt.Fprintln(os.Stdout, "重启后生效：")
		fmt.Fprintln(os.Stdout, "  · 若设置了 DISPATCH_ADMIN_PASSWORD → 用该口令登录；")
		fmt.Fprintln(os.Stdout, "  · 若未设置 → 启动时随机生成，并打印在启动日志里（每次重启都会变）。")
		fmt.Fprintln(os.Stdout, "\n重启命令：docker restart <容器名>   或   systemctl restart download-gateway")
		return nil
	}

	password := newPassword
	generated := false
	if password == "" {
		// 与启动时的随机口令同源（`ids.NewHex128` = 16 字节随机 → 32 位小写 hex），
		// 不另造一套字符集：同一个产品里出现两种「随机口令长相」只会让用户更迷惑。
		password = ids.NewHex128()
		generated = true
	}

	phc, err := adminpass.Hash(password)
	if err != nil {
		return err
	}
	if err := adminpass.SaveHash(ctx, st, phc, now); err != nil {
		return err
	}
	auditPasswordReset(ctx, st, now, "管理口令已通过命令行重置（reset-password）")

	fmt.Fprintln(os.Stdout, "管理口令已重置。")
	if generated {
		fmt.Fprintf(os.Stdout, "  新口令（随机生成）：%s\n", password)
		fmt.Fprintln(os.Stdout, "  ⚠️ 这串口令只在这里显示一次，请立刻保存到密码管理器。")
	} else {
		fmt.Fprintln(os.Stdout, "  新口令：使用你通过 -password 指定的那一串（本命令不回显）。")
	}
	fmt.Fprintf(os.Stdout, "  写入位置：%s（app_config.%s）\n\n", cfg.DBPath, adminpass.KeyHash)
	fmt.Fprintln(os.Stdout, "⚠️ 正在运行的服务仍持有旧口令：必须重启才生效。")
	fmt.Fprintln(os.Stdout, "   容器部署：docker restart <容器名>")
	fmt.Fprintln(os.Stdout, "   裸机部署：systemctl restart download-gateway")
	fmt.Fprintln(os.Stdout, "   重启后环境变量 DISPATCH_ADMIN_PASSWORD 不再参与校验（库里已有新口令）。")
	return nil
}

// auditPasswordReset 写一条安全审计：口令被**离线**改过，事后必须能查到。
//
// 审计失败只记一行 stderr，不阻断 —— 与 HTTP handler 中 `auditPassword` 的处理方式一致：
// 「口令已经改成功了，却因为写审计失败而报错」会让用户重复执行命令。
func auditPasswordReset(ctx context.Context, st *store.Store, now int64, message string) {
	if _, err := st.Execute(ctx,
		"INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at) "+
			"VALUES ('warn', 'security', NULL, NULL, ?1, NULL, ?2)",
		message, now,
	); err != nil {
		fmt.Fprintf(os.Stderr, "警告：写审计失败（口令已生效，可忽略）：%v\n", err)
	}
}
