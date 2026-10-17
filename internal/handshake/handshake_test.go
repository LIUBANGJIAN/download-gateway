package handshake

import (
	"fmt"
	"net/netip"
	"testing"
)

// ---------------------------------------------------------------------------
// 与 Rust `crates/dispatch-core/src/ingress/handshake.rs::tests` 逐条对照
// （`verify_credentials_paths` 需 `PublicState`+`envelope`，属 handler 层，留 T04）
// ---------------------------------------------------------------------------

// 对照 `invite_is_one_time_and_expires`。
func TestInviteIsOneTimeAndExpires(t *testing.T) {
	st := NewStore()
	inv := st.IssueInvite("dev-1", 1000)
	if got, ok := st.RedeemInvite(inv, 1000); !ok || got != "dev-1" {
		t.Fatalf("首次兑换应得 dev-1，实际 (%q,%v)", got, ok)
	}
	// 一次性：第二次兑换失败
	if _, ok := st.RedeemInvite(inv, 1000); ok {
		t.Fatalf("invite 应是一次性，第二次兑换必须失败")
	}
	// 过期：TTL 之外不可兑换
	inv2 := st.IssueInvite("dev-2", 2000)
	if _, ok := st.RedeemInvite(inv2, 2000+InviteTTLSecs+1); ok {
		t.Fatalf("过期 invite 不可兑换")
	}
	// 边界：正好在 TTL 时刻仍有效（Rust 是 `expires_at > now`，now=expires 即失效）
	inv3 := st.IssueInvite("dev-3", 3000)
	if _, ok := st.RedeemInvite(inv3, 3000+InviteTTLSecs); ok {
		t.Fatalf("now == expires_at 时应失效（Rust 判据是 expires_at > now）")
	}
}

// 对照 `device_table_is_capped_and_evicts_oldest`。
func TestDeviceTableIsCappedAndEvictsOldest(t *testing.T) {
	st := NewStore()
	tokens := make([]string, 0, DeviceCap+10)
	for i := 0; i < DeviceCap+10; i++ {
		tokens = append(tokens, st.IssueDevice(fmt.Sprintf("dev-%d", i)))
	}
	if got := st.DeviceCount(); got != DeviceCap {
		t.Fatalf("device 表必须被软上限约束为 %d，实际 %d", DeviceCap, got)
	}
	for _, old := range tokens[:10] {
		if st.IsDevice(old) {
			t.Fatalf("最旧的 device_token 应被淘汰：%s", old)
		}
	}
	if !st.IsDevice(tokens[len(tokens)-1]) {
		t.Fatalf("最新的 device_token 应仍在表内")
	}
}

// 对照 `login_rate_limit_allows_30_then_denies`。
func TestLoginRateLimitAllows30ThenDenies(t *testing.T) {
	st := NewStore()
	ip := netip.MustParseAddr("198.51.100.7")
	for i := 0; i < LoginRatePerMin; i++ {
		if !st.CheckLoginRate(ip, 1000) {
			t.Fatalf("第 %d 次（≤%d）应放行", i+1, LoginRatePerMin)
		}
	}
	if st.CheckLoginRate(ip, 1000) {
		t.Fatalf("第 %d 次应被拒", LoginRatePerMin+1)
	}
	// 换一个 IP 不受影响
	ip2 := netip.MustParseAddr("198.51.100.8")
	if !st.CheckLoginRate(ip2, 1000) {
		t.Fatalf("另一 IP 不应受第一个 IP 的限速影响")
	}
	// 窗口滑动后恢复
	if !st.CheckLoginRate(ip, 1000+61) {
		t.Fatalf("窗口滑动 61s 后应恢复放行")
	}
}

// 本包新增：到期 invite **即便已过期也必须被移除**（Rust `remove` 先于过期判定）。
// 这条能杀掉「先判过期、未过期才删」的变异。
func TestRedeemRemovesInviteEvenWhenExpired(t *testing.T) {
	st := NewStore()
	inv := st.IssueInvite("dev-x", 1000)
	// 过期兑换：返回失败，但内部必须已删除该 token
	if _, ok := st.RedeemInvite(inv, 1000+InviteTTLSecs+100); ok {
		t.Fatalf("过期 invite 兑换应失败")
	}
	// 再次兑换同一 token：仍失败（证明已被移除，而非残留）
	if _, ok := st.RedeemInvite(inv, 1000); ok {
		t.Fatalf("已被移除的 invite 再次兑换必须失败")
	}
}
