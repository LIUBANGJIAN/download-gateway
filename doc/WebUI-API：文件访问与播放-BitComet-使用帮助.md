---
title: "WebUI API：文件访问与播放 - BitComet 使用帮助"
url: "https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/file-access/"
domain: "wiki-zh.bitcomet.com"
excerpt: "返回 WebUI API 总览"
date: "2026-09-26T09:51:23.903Z"
---

## 文件访问、下载与播放

[返回 WebUI API 总览](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/)

文件内容接口不直接使用设备 Token。客户端先用设备 Token 换取短时、限定到任务文件的访问密钥，再用该密钥请求内容。不要尝试自行生成访问密钥。

## 取得访问密钥

```
/api/file/getAccessKey
```

```
{
  "task_id": "1",
  "file_index": "0"
}
```

此 JSON 请求需要：

```
Authorization: Bearer <device_token>
```

成功响应包含 `access_key`。`task_id` 和 `file_index` 必须来自任务及文件列表。

## 请求文件内容

兼容查询形式：

```
/api/file/getContent?fid=<URL-encoded access_key>
```

下载模式可追加：

```
&mode=download
```

带文件名的形式更适合浏览器播放和保存：

```
/api/file/getContent/<URL-encoded access_key>/<URL-encoded file_name>
```

`file_name` 必须逐段 URL 编码，不能包含未转义的 `/`、`?` 或 `#`。不要把设备 Token 拼进文件 URL。

## 安全与生命周期

-   `access_key` 是 bearer credential；知道完整 URL 的任何人都可能在有效期内读取对应文件。
-   不在外部播放器命令行、Referer、访问日志或第三方分析中长期暴露访问 URL。
-   获取新密钥后立即使用；过期或返回 400/403 时重新走 `getAccessKey`，不要猜测密钥格式。
-   播放器应支持 HTTP Range，并正确处理任务未完成、文件被移动、优先级被禁用或 BitComet 退出等情况。
-   页面关闭或切换文件时停止无用的预取请求。

若需要调用本机播放器，另见 [WebUI 调用本地视频播放器](https://wiki-zh.bitcomet.com/webui_%E8%B0%83%E7%94%A8%E6%9C%AC%E5%9C%B0%E8%A7%86%E9%A2%91%E6%92%AD%E6%94%BE%E5%99%A8/)。该功能涉及浏览器扩展或本地协议处理程序，不属于远程 API 本身。