// Package ids 提供标识符生成：GID / task_id（ULID）/ 128 位随机十六进制。
//
// 三者都是纯函数（除随机源外无副作用），可独立单测。
// 本包是 Go 版对 `crates/dispatch-core/src/ids.rs` 的 **1:1 移植**：
// 行为判据以 Rust 侧既有单测为准，逐条对照关系写在 `ids_test.go` 的注释里。
//
// 随机源统一走 `crypto/rand`（标准库），**不引入第三方 rand 库** ——
// 与 Rust 侧「只需要密码学安全随机字节，`getrandom` 已足够」的取舍一致。
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// NowMs 返回当前 Unix 毫秒。Rust 侧为 `ids::now_ms()`。
func NowMs() int64 { return time.Now().UnixMilli() }

// NowSecs 返回当前 Unix 秒。Rust 侧为 `ids::now_secs()`。
func NowSecs() int64 { return time.Now().Unix() }

// randReader 是随机源。生产恒为 crypto/rand.Reader；
// 仅测试会替换它，用来**真正触发**「随机源故障」这条降级分支。
var randReader io.Reader = rand.Reader

// randomBytes 读取 n 个密码学安全随机字节。
//
// 与 Rust 侧 `random_bytes` 对齐：随机源不可用时**不 panic**，回落全零，
// 标识唯一性交给 `task.gid UNIQUE` 约束 + CreateTask 的换 GID 重试兜底。
//
// ⚠️ 这个降级分支在标准库下**不可达**（Go 1.24 起 `crypto/rand.Read` 不再返回错误），
// 所以它只可能被「注入一个会失败的 Reader」的测试覆盖 —— 见
// `TestRandomBytesFallsBackToZeroWhenSourceFails`。
// 保留它不是装饰：它把「万一随机源坏了会怎样」这个语义写成了可执行断言。
func randomBytes(n int) []byte {
	buf := make([]byte, n)
	if _, err := io.ReadFull(randReader, buf); err != nil {
		clear(buf)
	}
	return buf
}

// NewGID 生成 16 位小写十六进制 GID（正则 `^[0-9a-f]{16}$`）。
//
// 随机 64 位；**不保证**进程内唯一 —— 唯一性由 `task.gid UNIQUE` 约束
// 加上 CreateTask 的换 GID 重试共同保证（`02 §4.1.4`）。
func NewGID() string {
	return fmt.Sprintf("%016x", binary.BigEndian.Uint64(randomBytes(8)))
}

// crockford 是 Crockford Base32 字母表（去掉了易混淆的 I/L/O/U）。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewTaskID 生成 26 字符 Crockford Base32 ULID：48bit 毫秒时间戳 + 80bit 随机。
//
// 时间前缀（前 10 字符）随时间**单调不减**，便于按创建顺序排序。
// 负时间戳按 0 处理（与 Rust 侧 `now_ms.max(0)` 一致）。
func NewTaskID(nowMs int64) string {
	var buf [16]byte
	var ts uint64
	if nowMs > 0 {
		ts = uint64(nowMs)
	}
	// 高 48 位放大端毫秒时间戳。
	// 等价于 Rust 侧逐字节写 buf[0..6]（ts>>40 … ts），并让 buf[6]、buf[7] 为 0 ——
	// 紧接着会被下面的随机字节覆盖。
	binary.BigEndian.PutUint64(buf[0:8], ts<<16)
	// 低 80 位放随机。
	copy(buf[6:16], randomBytes(10))
	return encodeULID(buf)
}

// NewHex128 生成 128bit 十六进制串（32 字符）：会话 sid / invite_token / device_token。
func NewHex128() string {
	return hex.EncodeToString(randomBytes(16))
}

// encodeULID 把 16 字节（128bit）编码成 26 字符 ULID 字符串。
//
// 26 字符 × 5bit = 130bit，最高的 2bit 恒为零（即首字符取值 `0..=7`）。
func encodeULID(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])
	var out [26]byte
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = crockford[lo&0x1f]
		// 128bit 整体右移 5 位（跨 64 位边界搬运）。
		lo = (lo >> 5) | (hi << 59)
		hi >>= 5
	}
	return string(out[:])
}
