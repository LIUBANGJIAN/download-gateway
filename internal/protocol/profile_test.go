package protocol

import "testing"

// Ported from crates/bitcomet-api/src/profile.rs `mod tests`.

func TestNodeProfileTrailingSlashesAreStripped(t *testing.T) {
	// Rust: trailing_slashes_are_stripped
	p := NewNodeProfile("http://example:9085///", "u", "p")
	if got := p.NormalizedBase(); got != "http://example:9085" {
		t.Fatalf("NormalizedBase() = %q, want %q", got, "http://example:9085")
	}
}

func TestNodeProfileDefaultDeviceNameIsUsed(t *testing.T) {
	// Rust: default_device_name_is_used
	// 默认设备名必须可预测 —— 这是"节点侧设备列表里上报的名字"这一可观测行为。
	p := NewNodeProfile("http://example:9085", "u", "p")
	if p.DeviceName != "dispatch-proxy" {
		t.Fatalf("DeviceName = %q, want %q", p.DeviceName, "dispatch-proxy")
	}
	if p.DeviceName != DefaultDeviceName {
		t.Fatalf("DeviceName = %q, want DefaultDeviceName", p.DeviceName)
	}
}

// Extra: password/base/username carried through verbatim, and a base URL with
// no trailing slash is unchanged.
func TestNodeProfileCarriesFieldsVerbatim(t *testing.T) {
	p := NewNodeProfile("http://node.example:9085", "alice", "s3cr3t")
	if p.BaseURL != "http://node.example:9085" || p.Username != "alice" || p.Password != "s3cr3t" {
		t.Fatalf("fields not carried verbatim: %+v", p)
	}
	if got := p.NormalizedBase(); got != "http://node.example:9085" {
		t.Fatalf("NormalizedBase() = %q", got)
	}
	if p.ClientIDPath != "" {
		t.Fatalf("ClientIDPath = %q, want empty (default)", p.ClientIDPath)
	}
}
