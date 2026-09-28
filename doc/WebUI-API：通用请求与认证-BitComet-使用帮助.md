---
title: "WebUI API：通用请求与认证 - BitComet 使用帮助"
url: "https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/authentication/"
domain: "wiki-zh.bitcomet.com"
excerpt: "返回 WebUI API 总览"
date: "2026-09-26T09:51:05.741Z"
---

## 通用请求与认证

[返回 WebUI API 总览](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/)

## 通用请求

官方 WebUI 对 JSON API 使用以下约定：

```
POST /api_v2/task_list/get HTTP/1.1
Content-Type: application/json
Client-Type: BitComet WebUI
Authorization: Bearer <device_token>
```

-   请求体必须是合法 JSON；没有参数时发送 `{}`。
-   除登录前接口外，`Authorization` 使用设备 Token。
-   不要依赖浏览器 Cookie 作为 API 身份。
-   普通 JSON 响应通常包含 `error_code`、`version`、`platform` 和 `file_size_prefix`。`file_size_prefix` 可能为 `decimal` 或 `binary`。
-   历史接口的成功值存在 `OK`、`ok` 等大小写差异。不要全局硬编码一个值；按具体接口处理，并优先把非成功值及 `error_message` 展示给用户。

设备 Token 无效时，后端返回 HTTP 401，响应通常包含：

```
{
  "error_code": "INVALID_TOKEN",
  "error_message": "Invalid token"
}
```

收到 401 后应清除本地 Token 并重新登录，不应无限重试。

## 认证流程

```
生成并持久化 client_id（UUID）
  -> 可选 POST /api/webui/ip_verify
  -> POST /api/webui/login
  -> 获得 invite_token
  -> POST /api/device_token/get
  -> 获得 device_token
  -> 后续 API 使用 Bearer device_token
```

`client_id` 用来稳定标识当前第三方客户端实例，也是登录认证密文的口令。首次运行生成一个 UUID，之后继续使用同一个值；不要每次请求重新生成。

### 1\. 检查是否允许免密码登录

```
POST /api/webui/ip_verify

{}
```

响应中的 `bypass_eligible` 表示当前请求是否满足部署端的免密码条件。它是可选兼容能力，第三方实现不能假设本机、内网或带 `Client-Type` 就一定允许绕过登录。失败时回退到用户名密码登录。

### 2\. 用户名密码登录

先构造认证明文：

```
{"username":"<username>","password":"<password>"}
```

以 `client_id` 作为 RNCryptor 口令加密完整 JSON 字符串，再发送 Base64 结果：

```
{
  "client_id": "<UUID>",
  "authentication": "<Base64 RNCryptor v3 data>"
}
```

接口：

```
/api/webui/login
```

成功响应包含短期用途的 `invite_token`。如果部署明确允许免密码登录，官方 WebUI 会向同一接口发送：

```
{
  "client_id": "<UUID>",
  "bypass": true
}
```

不要自行放宽免密码条件；是否允许完全由 BitComet 端决定。

### 3\. 换取设备 Token

接口：

```
/api/device_token/get
```

请求头使用刚取得的邀请 Token：

```
Authorization: Bearer <invite_token>
```

请求体：

```
{
  "invite_token": "<invite_token>",
  "device_id": "<与 client_id 相同的 UUID>",
  "device_name": "My WebUI @ Windows",
  "platform": "webui"
}
```

成功响应中的 `device_token` 用于后续 API。当前实现没有基于时间的自动过期检查，但用户删除绑定设备、Token 无效或服务端状态变化后会失效；客户端仍应正确处理 401。

## 登录密文格式

为实现互操作，用户名密码登录需要兼容以下 RNCryptor v3 格式：

| 参数 | 值 |
| --- | --- |
| 数据格式 | RNCryptor v3，版本字节 `3`，options 字节 `1` |
| 对称加密 | AES-256-CBC |
| 填充 | PKCS#7 |
| 加密 salt | 随机 8 bytes |
| HMAC salt | 随机 8 bytes |
| IV | 随机 16 bytes |
| 密钥派生 | PBKDF2-HMAC-SHA1，10000 次，输出 32 bytes |
| 完整性校验 | HMAC-SHA256，输出 32 bytes |
| 口令 | 当前客户端持久化的 `client_id` |
| 最终编码 | Base64 |

二进制布局如下：

```
version(1) | options(1) | encryptionSalt(8) | hmacSalt(8) |
IV(16) | ciphertext(variable) | HMAC(32)
```

应使用经过验证的 RNCryptor v3 实现，或严格按照规范生成随机 salt/IV、派生两把独立密钥，并在解密前验证 HMAC。

## HTTPS 与敏感数据

> **登录加密不能替代 HTTPS。** `client_id` 和用它作为口令生成的 `authentication` 在同一个请求中传输；能够截获或篡改 HTTP 的攻击者仍可破坏认证安全。

当前后端对 loopback 和官方 WebUI 兼容路径存在非 TLS 例外，因此不能把“路由标记需要 SSL”误写成“所有部署都会拒绝 HTTP”。第三方 WebUI 必须主动使用 HTTPS，验证证书，不允许静默降级到 HTTP。

-   不在 URL、日志、分析事件或崩溃报告中记录密码、邀请 Token、设备 Token、认证密文和文件访问密钥。
-   Token 仅存放在受同源策略保护的位置；防止 XSS 比“隐藏接口路径”更重要。
-   不把内部固定 salt、私有派生常量或部署白名单复制到第三方客户端。这些都不是 WebUI 互操作协议的一部分。