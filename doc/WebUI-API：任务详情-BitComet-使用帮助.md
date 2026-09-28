---
title: "WebUI API：任务详情 - BitComet 使用帮助"
url: "https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/task-details/"
domain: "wiki-zh.bitcomet.com"
excerpt: "返回 WebUI API 总览"
date: "2026-09-26T09:51:15.511Z"
---

## 任务详情、文件、Tracker 与 Peer

[返回 WebUI API 总览](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/)

以下接口的 `task_id` 均为字符串。任务可能在请求期间停止或被删除，因此详情页要能处理 `task_id` 失效和空数组。

## 任务摘要

```
/api/task/summary/get
```

```
{"task_id":"1"}
```

响应重点字段包括 `task`、`task_detail`、`task_status` 和 `task_summary`。字段集合会因 HTTP/BT 类型和 BitComet 版本而不同。

## 文件列表与优先级

```
/api/task/files/get
```

```
{
  "task_id": "1",
  "keyword": "",
  "sort_order": "ascend",
  "sort_key": "name",
  "start": 0,
  "limit": 100
}
```

除 `task_id` 外，其余筛选、排序和分页字段可选。响应包含 `files`、`filtered_count`、`task` 和 `flag`。

设置文件优先级：

```
/api/task/files/set_priority
```

```
{
  "task_id": "1",
  "file_indexes": [0, 2],
  "priority": "normal"
}
```

`file_indexes` 来自文件列表。当前官方 WebUI 使用 `very_high`、`high`、`normal` 和 `disabled`；仍应以目标版本响应为准，不要用文件名代替稳定的索引。

## Tracker 与其他详情

| 接口 | 主要请求字段 | 主要响应字段 |
| --- | --- | --- |
| `/api/task/trackers/get` | `task_id` | `task`、`trackers` |
| `/api/task/servers/get` | `task_id`、`max_count` | `task`、`servers` |
| `/api/task/connections/get` | `task_id`、`max_count` | `task`、`connections` |
| `/api/task/logs/get` | `task_id`、`last_log_id` | `log_list` |
| `/api/task/piece_map/get` | `task_id` | `piece_list` |

`last_log_id` 用于增量获取日志。详情页离开后应停止轮询，避免无意义请求。

## 获取 Peer 列表

```
/api/task/peers/get
```

```
{
  "task_id": "1",
  "groups": ["peers_connected", "peers_connecting"],
  "max_count": 200,
  "sort_order": 0,
  "sort_key": 0,
  "columns": []
}
```

常见分组包括 `peers_connected`、`peers_connecting`、`peers_disconnected`、`peers_banned`、`ltseeds_connected`、`ltseeds_connecting` 和 `webseeds`。响应包含 `peers`、`peer_count` 和 `task`。

当前官方 WebUI 的 `sort_order`、`sort_key` 使用数值枚举，并可用 `columns` 指定返回字段。旧页面曾记录字符串排序键；第三方实现应以目标版本实际响应和官方 WebUI为准，不要混用两套格式。

## 封禁与解封 Peer

封禁：

```
/api/task/peers/ban_ip
```

```
{
  "task_id": "1",
  "ban_time": "ban_ip_1h",
  "ip_list": ["192.0.2.10"]
}
```

`ban_time` 常用值为 `ban_ip_5m`、`ban_ip_1h`、`ban_ip_1d`、`ban_ip_forever`。永久封禁会影响 IP Filter 手动列表，应在界面中明确提示。

解封：

```
/api/task/peers/unban_peers
```

```
{
  "task_id": "1",
  "unban_range": "unban_peers",
  "ip_list": ["192.0.2.10"]
}
```

`unban_range` 可为 `unban_peers`、`unban_peers_in_all_tasks`、`unban_all_peers` 或 `unban_all_tasks`。后两种范围较大，调用前应确认。