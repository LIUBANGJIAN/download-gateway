// Package protocol implements the on-the-wire primitives the download-gateway
// uses to talk to BitComet WebUI nodes.
//
// This file ports crates/bitcomet-api/src/profile.rs 1:1.
package protocol

import "strings"

// DefaultDeviceName is the device name reported to BitComet WebUI nodes
// (one of the login parameters). See crates/bitcomet-api/src/lib.rs.
const DefaultDeviceName = "dispatch-proxy"

// NodeProfile holds the connection parameters of a single BitComet node.
//
// It corresponds to the `base_url` / `user` / `pass_enc` / `device_name`
// columns of the `node` table. Note that the password is stored encrypted
// (`pass_enc`); decryption is the caller's responsibility — this type only
// ever carries plaintext.
//
// ClientIDPath mirrors the Rust `Option<PathBuf>`: an empty string means
// "use DefaultClientIDPath()".
type NodeProfile struct {
	// BaseURL, e.g. http://node.example:9085 (trailing slashes are stripped by
	// NormalizedBase).
	BaseURL string
	// Username is the WebUI user name.
	Username string
	// Password is the WebUI password (plaintext).
	Password string
	// DeviceName is reported to the node; it shows up in the node's bound-device
	// list.
	DeviceName string
	// ClientIDPath is the client_id persistence path; empty means default.
	ClientIDPath string
}

// NewNodeProfile builds a profile using the default device name.
func NewNodeProfile(baseURL, username, password string) NodeProfile {
	return NodeProfile{
		BaseURL:    baseURL,
		Username:   username,
		Password:   password,
		DeviceName: DefaultDeviceName,
	}
}

// NormalizedBase returns the base URL with all trailing slashes removed.
// Mirrors Rust's `trim_end_matches('/')`.
func (p NodeProfile) NormalizedBase() string {
	return strings.TrimRight(p.BaseURL, "/")
}
