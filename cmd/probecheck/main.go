// Command probecheck 对一台 BitComet 节点做逐层诊断：TCP → 登录握手 → 业务调用。
//
// 为什么要这个工具：`dispatch.ProbeAndPersist` 只回一个 `degraded` + 一句话，
// 排查时看不出是「端口不通」「口令错」「App 访问没开」还是「版本不兼容」。
// 这里把每一层的原始响应打出来，避免上线后再靠猜。
//
// 用法：
//
//	go run ./cmd/probecheck <base_url> <user> <password>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "用法: probecheck <base_url> <user> <password>")
		os.Exit(2)
	}
	baseURL, user, pass := os.Args[1], os.Args[2], os.Args[3]

	fmt.Printf("节点: %s\n", baseURL)
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		fmt.Printf("  [FAIL] base_url 解析失败: %v\n", err)
		os.Exit(1)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}

	// L1: TCP
	start := time.Now()
	conn, dialErr := net.DialTimeout("tcp", host, 3*time.Second)
	if dialErr != nil {
		fmt.Printf("  [FAIL] L1 TCP 不通: %v\n", dialErr)
		os.Exit(1)
	}
	_ = conn.Close()
	fmt.Printf("  [OK]   L1 TCP 通 (%d ms)\n", time.Since(start).Milliseconds())

	// L2: 登录握手
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	profile := protocol.NewNodeProfile(baseURL, user, pass)
	cl, err := protocol.NewClient(profile, 10)
	if err != nil {
		fmt.Printf("  [FAIL] 构造客户端失败: %v\n", err)
		os.Exit(1)
	}

	loginStart := time.Now()
	if err := cl.Login(ctx); err != nil {
		fmt.Printf("  [FAIL] L2 登录失败 (%d ms): %v\n", time.Since(loginStart).Milliseconds(), err)
		os.Exit(1)
	}
	fmt.Printf("  [OK]   L2 登录成功 (%d ms)\n", time.Since(loginStart).Milliseconds())

	// L3: 业务调用
	about, err := cl.About(ctx)
	if err != nil {
		fmt.Printf("  [FAIL] L3 about 调用失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  [OK]   L3 about 成功\n")
	for _, k := range []string{"version", "product_name", "app_name"} {
		if v, ok := about[k]; ok {
			fmt.Printf("         %s = %v\n", k, v)
		}
	}

	// L4: 能不能建任务（只读探测，不真提交）
	cfg, err := cl.NewTaskConfig(ctx, "http")
	if err != nil {
		fmt.Printf("  [FAIL] L4 new_task/get 失败（可能无下载权限）: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  [OK]   L4 new_task/get 成功 —— 节点可接任务\n")

	// L5: 打印建任务时的关键字段，确认 save_folder 的默认值从哪来。
	b, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Printf("  --- new_task/get 原始返回 ---\n%s\n", string(b))
}
