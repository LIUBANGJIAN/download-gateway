---
title: "WebUI API：添加任务 - BitComet 使用帮助"
url: "https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/add-tasks/"
domain: "wiki-zh.bitcomet.com"
excerpt: "返回 WebUI API 总览"
date: "2026-09-26T09:51:12.341Z"
---

## 添加任务

[返回 WebUI API 总览](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/)

添加任务前先读取当前版本和平台的默认值，不要在第三方界面中复制一份固定配置。

## 获取新任务默认配置

```
/api/config/new_task/get
```

```
{
  "task_type": "BT"
}
```

`task_type` 使用目标任务类型，例如 `BT` 或 `HTTP`。响应中的 `task_config` 可用于填充保存目录、连接数和“稍后开始”等选项。

## 添加 HTTP/FTP 任务

```
/api/task/http/add
```

最小示例：

```
{
  "url": "https://example.com/file.iso",
  "save_folder": "C:\\Downloads",
  "start_later": false
}
```

常用字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `url` | string | HTTP、HTTPS 或 FTP URL |
| `save_folder` | string | BitComet 所在系统可访问的目录，不是浏览器本机路径 |
| `start_later` | boolean | 是否只创建任务而暂不开始 |
| `max_connection_count` | integer | 可选，单任务最大连接数；旧页面记录默认 5、范围 1-2000 |

成功响应包含 `task_id`。其他可选项应优先从新任务配置响应复制并按用户选择修改。

## 添加 BT 任务

```
/api/task/bt/add
```

通过 torrent URL：

```
{
  "torrent_url": "https://example.com/file.torrent",
  "torrent_file": "",
  "save_folder": "C:\\Downloads",
  "start_later": false
}
```

也可以令 `torrent_url` 为空，并在 `torrent_file` 传入 torrent 文件内容的 Base64 字符串。两者不能同时为空。成功响应包含 `task_id`。

不要把不受信任的本地文件路径直接当作 `torrent_url`；浏览器上传应读取文件内容并编码后提交。

## 添加磁力链接或多个 Torrent 链接

```
/api/task/torrent_links/add
```

```
{
  "torrent_links": "magnet:?xt=urn:btih:...\nhttps://example.com/a.torrent",
  "save_folder": "C:\\Downloads",
  "start_later": false
}
```

`torrent_links` 是包含一个或多个链接的字符串。提交后可轮询批量处理状态：

```
/api/task/batch_download/query
```

请求体为 `{}`，响应重点字段是 `batch_download_result`。批量添加可能异步完成，不要仅凭首次请求成功就假定所有任务均已建立。