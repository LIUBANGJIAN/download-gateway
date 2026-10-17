// Package protocol implements the on-the-wire primitives the download-gateway
// uses to talk to BitComet WebUI nodes.
//
// This file ports crates/bitcomet-api/src/client.rs 1:1 (the BitComet WebUI
// client: three-stage login + protected calls with a single 401 re-login).
//
// # 三段式认证（design/12 §4.2）
//
//	POST /api/webui/login        Header: Client-Type: BitComet WebUI
//	                             Body: { client_id, authentication: RNCryptor(b64) }
//	  -> invite_token
//	POST /api/device_token/get   Header: Client-Type, Authorization: Bearer <invite_token>
//	                             Body: { invite_token, device_id, device_name, platform }
//	  -> device_token
//	  -> 后续所有 API 使用 Authorization: Bearer <device_token>
//
// # 两条实现纪律
//
//  1. 401 只自动重登一次（官方文档：不应无限重试）。
//  2. 成功值不硬编码：历史接口存在 OK / ok 大小写差异，统一走 IsOKCode。
package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ClientType is the official client-identity header value.
const ClientType = "BitComet WebUI"

// UserAgent is the User-Agent sent to nodes. Rust builds it as
// `download-gateway/{CARGO_PKG_VERSION}`; here it defaults to the unknown
// version and is intended to be overridden by main via the version injection
// point (待接线点).
var UserAgent = "download-gateway/0.0.0-unknown"

// Client sentinel errors. Error text is contract (mirrors ClientError).
var (
	// ErrNotLoggedIn is returned when a protected call is attempted before login.
	ErrNotLoggedIn = errors.New("尚未登录：请先调用 login()")
	// ErrTokenRejected is returned when a 401 persists after one re-login.
	ErrTokenRejected = errors.New("设备 Token 被拒（已重登一次仍失败）")
	// ErrUnauthorized is the internal 401 marker used by the retry branch.
	ErrUnauthorized = errors.New("unauthorized")
)

// MissingFieldError is returned when a response lacks a required field.
type MissingFieldError struct {
	Field string
}

func (e *MissingFieldError) Error() string {
	return fmt.Sprintf("响应缺少字段 `%s`", e.Field)
}

// LoginError is returned when a login-stage endpoint answers a non-2xx status.
type LoginError struct {
	Status int
	Detail string
}

func (e *LoginError) Error() string {
	return fmt.Sprintf("登录失败（HTTP %d）: %s", e.Status, e.Detail)
}

// RemoteError is returned when a node business error_code is not a success value.
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("节点返回错误 error_code=%s: %s", e.Code, e.Message)
}

// IsOKCode decides whether a BitComet error_code counts as success.
//
// 官方文档提醒历史接口存在 OK / ok 的大小写差异，因此不要写 code == "OK"。
// Empty (after trim) counts as success.
func IsOKCode(code string) bool {
	t := strings.TrimSpace(code)
	return t == "" || asciiLower(t) == "ok"
}

// checkErrorCode mirrors client.rs::check_error_code: a response without a
// string error_code is treated as success; otherwise a non-OK code yields a
// RemoteError.
func checkErrorCode(v map[string]any) error {
	codeRaw, ok := v["error_code"]
	if !ok {
		return nil
	}
	code, ok := codeRaw.(string)
	if !ok {
		return nil
	}
	if IsOKCode(code) {
		return nil
	}
	message := ""
	if m, ok := v["error_message"].(string); ok {
		message = m
	}
	return &RemoteError{Code: code, Message: message}
}

// shortText truncates a response body to 300 characters (plus an ellipsis) for
// diagnostics, mirroring client.rs::short.
func shortText(text string) string {
	const max = 300
	r := []rune(text)
	if len(r) <= max {
		return text
	}
	return string(r[:max]) + "…"
}

// Client is a BitComet WebUI client bound to one node profile.
type Client struct {
	http        *http.Client
	baseURL     string
	profile     NodeProfile
	clientID    string
	deviceToken *string // nil == not logged in
}

// NewClient constructs a client and loads/creates the client_id (it does NOT
// make any network request). timeoutSecs drives both the connect timeout
// (clamped to [1,30]) and the overall request timeout (>= 1).
func NewClient(profile NodeProfile, timeoutSecs uint64) (*Client, error) {
	connect := int64(timeoutSecs)
	if connect < 1 {
		connect = 1
	}
	if connect > 30 {
		connect = 30
	}
	total := int64(timeoutSecs)
	if total < 1 {
		total = 1
	}
	transport := &http.Transport{
		// F11: nodes are on the internal network and must never be hijacked by
		// system proxy variables. A nil Proxy disables proxy use.
		Proxy:       nil,
		DialContext: (&net.Dialer{Timeout: time.Duration(connect) * time.Second}).DialContext,
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(total) * time.Second,
	}

	path := profile.ClientIDPath
	if path == "" {
		path = DefaultClientIDPath()
	}
	clientID, err := LoadOrCreateClientID(path)
	if err != nil {
		return nil, fmt.Errorf("client_id 读写失败: %w", err)
	}

	return &Client{
		http:     httpClient,
		baseURL:  profile.NormalizedBase(),
		profile:  profile,
		clientID: clientID,
	}, nil
}

// ClientID returns the client_id in use.
func (c *Client) ClientID() string { return c.clientID }

// BaseURL returns the normalized node base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// HasToken reports whether a device_token is held.
func (c *Client) HasToken() bool { return c.deviceToken != nil }

// Profile returns the node connection parameters.
func (c *Client) Profile() NodeProfile { return c.profile }

func (c *Client) url(path string) string { return c.baseURL + path }

// postJSON sends a POST with Client-Type, User-Agent, Content-Type and body
// (marshalled with sorted keys, matching serde_json's BTreeMap order), and
// returns the status code and the raw response body text.
func (c *Client) postJSON(ctx context.Context, path string, body any, headers map[string]string) (int, string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, "", fmt.Errorf("JSON 处理失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewReader(raw))
	if err != nil {
		return 0, "", fmt.Errorf("HTTP 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Client-Type", ClientType)
	req.Header.Set("User-Agent", UserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("HTTP 请求失败: %w", err)
	}
	return resp.StatusCode, string(data), nil
}

// parseJSONObject parses text into a JSON object; any non-object or invalid JSON
// degrades to a nil map, mirroring serde_json's `unwrap_or(Value::Null)`.
func parseJSONObject(text string) map[string]any {
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return nil
	}
	return v
}

// Login performs the full three-stage login and leaves the client ready to use.
func (c *Client) Login(ctx context.Context) error {
	// ── 第 1 段：用 client_id 作口令加密凭据，换取 invite_token ──
	plaintext, err := json.Marshal(map[string]any{
		"username": c.profile.Username,
		"password": c.profile.Password,
	})
	if err != nil {
		return fmt.Errorf("JSON 处理失败: %w", err)
	}
	authentication, err := EncryptToBase64(plaintext, c.clientID)
	if err != nil {
		return fmt.Errorf("RNCryptor 失败: %w", err)
	}

	body := map[string]any{
		"client_id":      c.clientID,
		"authentication": authentication,
	}
	status, text, err := c.postJSON(ctx, "/api/webui/login", body, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &LoginError{Status: status, Detail: shortText(text)}
	}
	v := parseJSONObject(text)
	if err := checkErrorCode(v); err != nil {
		return err
	}
	inviteToken, ok := v["invite_token"].(string)
	if !ok {
		return &MissingFieldError{Field: "invite_token"}
	}

	// ── 第 2 段：用 invite_token 换 device_token ──
	body2 := map[string]any{
		"invite_token": inviteToken,
		"device_id":    c.clientID,
		"device_name":  c.profile.DeviceName,
		"platform":     "webui",
	}
	headers := map[string]string{"Authorization": "Bearer " + inviteToken}
	status, text, err = c.postJSON(ctx, "/api/device_token/get", body2, headers)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &LoginError{Status: status, Detail: shortText(text)}
	}
	v = parseJSONObject(text)
	if err := checkErrorCode(v); err != nil {
		return err
	}
	token, ok := v["device_token"].(string)
	if !ok {
		return &MissingFieldError{Field: "device_token"}
	}

	c.deviceToken = &token
	return nil
}

// EnsureLogin logs in if no device_token is held.
func (c *Client) EnsureLogin(ctx context.Context) error {
	if c.deviceToken == nil {
		return c.Login(ctx)
	}
	return nil
}

// callOnce performs a single protected call (no 401 retry).
func (c *Client) callOnce(ctx context.Context, path string, body any) (map[string]any, error) {
	if c.deviceToken == nil {
		return nil, ErrNotLoggedIn
	}
	headers := map[string]string{"Authorization": "Bearer " + *c.deviceToken}
	status, text, err := c.postJSON(ctx, path, body, headers)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	v := parseJSONObject(text)
	if status < 200 || status >= 300 {
		return nil, &RemoteError{Code: fmt.Sprintf("HTTP_%d", status), Message: shortText(text)}
	}
	if err := checkErrorCode(v); err != nil {
		return nil, err
	}
	return v, nil
}

// Call performs a protected call, automatically re-logging in ONCE on 401.
func (c *Client) Call(ctx context.Context, path string, body any) (map[string]any, error) {
	v, err := c.callOnce(ctx, path, body)
	if errors.Is(err, ErrUnauthorized) {
		c.deviceToken = nil
		if loginErr := c.Login(ctx); loginErr != nil {
			return nil, loginErr
		}
		v2, err2 := c.callOnce(ctx, path, body)
		if errors.Is(err2, ErrUnauthorized) {
			return nil, ErrTokenRejected
		}
		return v2, err2
	}
	return v, err
}

// FetchTaskList fetches the task list (raw JSON; field modelling is a later task).
// Endpoint: POST /api_v2/task_list/get, sending `{}` when no params.
func (c *Client) FetchTaskList(ctx context.Context) (map[string]any, error) {
	if err := c.EnsureLogin(ctx); err != nil {
		return nil, err
	}
	return c.Call(ctx, "/api_v2/task_list/get", map[string]any{})
}

// AddHTTPTask adds an HTTP/FTP task and returns the node response (with task_id).
//
// save_folder is not sent by default (keep the node's original download folder);
// only when the caller passes one explicitly is it included.
func (c *Client) AddHTTPTask(ctx context.Context, url string, saveFolder *string, startLater bool) (map[string]any, error) {
	if err := c.EnsureLogin(ctx); err != nil {
		return nil, err
	}
	m := map[string]any{
		"url":         url,
		"start_later": startLater,
	}
	if saveFolder != nil {
		m["save_folder"] = *saveFolder
	}
	return c.Call(ctx, "/api/task/http/add", m)
}

// AddTorrentLinks adds magnet/torrent links (possibly multi-line).
func (c *Client) AddTorrentLinks(ctx context.Context, links string, saveFolder *string, startLater bool) (map[string]any, error) {
	if err := c.EnsureLogin(ctx); err != nil {
		return nil, err
	}
	m := map[string]any{
		"torrent_links": links,
		"start_later":   startLater,
	}
	if saveFolder != nil {
		m["save_folder"] = *saveFolder
	}
	return c.Call(ctx, "/api/task/torrent_links/add", m)
}

// About reads the node's about info (used by T03's authenticated health probe:
// it only looks at error_code and about_info).
func (c *Client) About(ctx context.Context) (map[string]any, error) {
	if err := c.EnsureLogin(ctx); err != nil {
		return nil, err
	}
	return c.Call(ctx, "/api/config/about/get", map[string]any{})
}

// NewTaskConfig reads the new-task default config (task_type is BT or HTTP).
func (c *Client) NewTaskConfig(ctx context.Context, taskType string) (map[string]any, error) {
	if err := c.EnsureLogin(ctx); err != nil {
		return nil, err
	}
	return c.Call(ctx, "/api/config/new_task/get", map[string]any{"task_type": taskType})
}
