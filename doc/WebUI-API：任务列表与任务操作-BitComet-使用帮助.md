---
title: "WebUI API：任务列表与任务操作 - BitComet 使用帮助"
url: "https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/task-list-actions/"
domain: "wiki-zh.bitcomet.com"
excerpt: "返回 WebUI API 总览"
date: "2026-09-26T09:51:08.805Z"
---

## 任务列表与任务操作

[返回 WebUI API 总览](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/)

本页接口均使用设备 Token。列表、操作和删除优先采用 `/api_v2/` 版本，以支持分页和“选择全部筛选结果”。

## 获取任务列表

```
/api_v2/task_list/get
```

典型请求：

```
{
  "state_group": "ALL",
  "task_type": "ALL",
  "tag_filter": "ALL",
  "sort_key": "Name",
  "sort_order": "ascend",
  "keyword": "",
  "start": 0,
  "limit": 50,
  "excluded_task_ids": []
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `state_group` | string | `ALL`、`DOWNLOADING`、`DOWNLOAD_PAUSED`、`SEEDING`、`SEED_PAUSED`、`COMPLETED`、`UNCOMPLETED`、`ACTIVE`、`INACTIVE` |
| `task_type` | string | `ALL`、`BT`、`HTTP` |
| `tag_filter` | string | `ALL`、`UNTAGGED` 或 `TAG_{tag}` |
| `sort_key` | string | 如 `Name`、`Size`、`Progress`、`LeftTime`、`Download`、`Upload`、`DownloadSize`、`UploadSize` |
| `sort_order` | string | `ascend`、`descend`、`unsorted` |
| `keyword` | string | 名称关键字；空字符串表示不过滤 |
| `start` / `limit` | integer | 可选分页；`start >= 0` 且 `limit > 0` |
| `excluded_task_ids` | string\[\] | 可选；校正“全选筛选结果”状态 |

响应重点字段为 `tasks`、`filtered_task_count` 和可选的 `valid_excluded_task_ids`。任务对象会随版本增加字段，应按需读取而不是拒绝未知字段。

## 获取多任务补充信息

```
/api/tasks/info/get
```

```
{
  "task_ids": ["1", "2"],
  "key": "<所需信息键>"
}
```

响应使用 `info_list` 返回与请求相关的补充信息。`key` 的可用值可能随版本变化，第三方实现应做能力探测。

## 选择任务

v2 操作接口支持两种互斥选择格式。

明确列出任务：

```
{
  "task_ids": ["1", "2"]
}
```

选择当前筛选结果中的全部任务，但排除少数项：

```
{
  "select_all_filtered": true,
  "excluded_task_ids": ["9"],
  "filter_snapshot": {
    "state_group": "DOWNLOADING",
    "task_type": "ALL",
    "tag_filter": "ALL",
    "keyword": "linux"
  }
}
```

第二种格式能避免先下载全部任务 ID。筛选快照必须与用户点击“全选”时看到的条件一致。

## 启动、停止与校验

```
/api_v2/tasks/action
```

在上述选择对象中加入 `action`：

```
{
  "task_ids": ["1", "2"],
  "action": "start"
}
```

重要操作值：

-   `start`：开始或继续任务；
-   `stop`：暂停任务；
-   `tracker_update`：立即更新 Tracker；
-   `hash_check`：重新校验数据。

响应可能包含更新后的 `tasks`。使用“选择全部筛选结果”后，客户端通常应重新加载列表。

## 删除与清理

```
/api_v2/tasks/delete
```

选择格式与操作接口相同，`action` 常用值为：

-   `delete_task`：删除任务，保留已下载文件；
-   `delete_all`：删除任务及相关文件；
-   `cleanup_unfinished`：清理未完成任务；
-   `cleanup_all`：清理全部任务。

> 删除文件不可逆。第三方 WebUI 必须在调用 `delete_all`、`cleanup_unfinished` 或 `cleanup_all` 前显示明确确认，并在成功后刷新整个任务列表。

旧接口 `/api/tasks/action` 仍被部分功能使用，但新实现应优先采用两个 v2 接口，避免把删除动作发送到普通操作端点。