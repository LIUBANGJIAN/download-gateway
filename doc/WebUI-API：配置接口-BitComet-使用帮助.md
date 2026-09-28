---
title: "WebUI API：配置接口 - BitComet 使用帮助"
url: "https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/configuration/"
domain: "wiki-zh.bitcomet.com"
excerpt: "返回 WebUI API 总览"
date: "2026-09-26T09:51:21.260Z"
---

## 配置接口

[返回 WebUI API 总览](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/)

配置对象会随平台和版本变化。安全的修改方式是先调用对应 `get` 接口，保留未知字段，只修改用户明确选择的项，再把完整对象传给 `set` 接口。

## 下载目录

| 接口 | 请求 |
| --- | --- |
| `/api/config/directories/get` | `{}` |
| `/api/config/directories/set` | `{"directories_config": {...}}` |
| `/api/config/directories/add` | `{"dir_path":"C:\\Downloads"}` |
| `/api/config/directories/remove` | `{"dir_path":"C:\\Downloads"}` |

目录是 BitComet 主机上的路径。不要把浏览器设备上的路径或未经用户确认的网络共享路径直接提交。

## 网络连接

```
/api/config/connection/get
/api/config/connection/set
```

读取请求为 `{}`；设置请求把读取到的对象放在 `connection_config` 中：

```
{
  "connection_config": {
    "max_download_speed": 0,
    "max_upload_speed": 0,
    "enable_listen_tcp": true,
    "listen_port_tcp": 6082
  }
}
```

`0` 通常表示不限速。端口、IPv6、代理和 UDP 等字段依平台而异；示例不是可直接覆盖整个配置的最小对象。

## 其他常用配置组

下列接口大多采用成对的 `get`/`set` 形式：

| 功能 | 获取 | 设置 |
| --- | --- | --- |
| 任务默认值 | `/api/config/tasks/get` | `/api/config/tasks/set` |
| BT 任务 | `/api/config/bt_task/get` | `/api/config/bt_task/set` |
| Tracker | `/api/config/bt_tracker/get` | `/api/config/bt_tracker/set` |
| 长效种子 | `/api/config/ltseed/get` | `/api/config/ltseed/set` |
| 磁盘缓存 | `/api/config/disk_cache/get` | `/api/config/disk_cache/set` |
| 计划任务 | `/api/config/scheduler/get` | `/api/config/scheduler/set` |
| 远程访问 | `/api/config/remote_access/get` | `/api/config/remote_access/set` |

设置接口的外层字段名通常与功能对应，例如 `tasks_config`、`connection_config` 或 `directories_config`。应参考同版本官方 WebUI 发出的请求，不要仅按接口名猜字段。

## IP Filter

| 接口 | 用途 |
| --- | --- |
| `/api/config/ipfilter/get` | 读取配置 |
| `/api/config/ipfilter/set` | 写入配置 |
| `/api/config/ipfilter/upload` | 导入列表 |
| `/api/config/ipfilter/download` | 导出 `data_file` |
| `/api/config/ipfilter/clear` | 清除 `data_file` |
| `/api/config/ipfilter/update` | 更新远程列表 |
| `/api/config/ipfilter/query` | 查询异步导入/更新状态 |

导入示例：

```
{
  "content_base64": "<Base64 encoded list>",
  "data_type": "data_file",
  "import_type": "replace"
}
```

`data_type` 可为 `data_file` 或 `manual_list`；`import_type` 常用 `replace`、`merge`。旧接口行为中，下载和清除只针对 `data_file`，不应宣称能够导出或清空手动列表。

## 版本与全局状态

```
/api/config/about/get
/api/footer_status/get
```

两者请求体均为 `{}`。`about` 适合展示版本、平台和版权信息；`footer_status` 适合获取全局速率、端口或连接状态。实现兼容性判断时优先使用明确版本字段，不要根据某个可翻译的显示字符串推断版本。