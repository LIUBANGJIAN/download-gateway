---
title: "WebUI API：高级接口索引 - BitComet 使用帮助"
url: "https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/advanced-api-index/"
domain: "wiki-zh.bitcomet.com"
excerpt: "返回 WebUI API 总览"
date: "2026-09-26T09:51:26.796Z"
---

## 高级接口索引与兼容性

[返回 WebUI API 总览](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/)

本页用于发现可选能力，不是 113 个后端路由的逐字段规范。Android、Repeater、内部证书交换和部署专用接口不应被普通第三方 WebUI 直接复用。

## 监控与界面状态

| 接口 | 用途 |
| --- | --- |
| `/api/flow_graph/get` | 获取速率图数据 |
| `/api/global_logs/get` | 获取全局日志 |
| `/api/statistics_list/get` | 获取统计列表 |
| `/api/footer_status/get` | 获取全局状态栏数据 |
| `/api/notifications/get` | 获取通知 |
| `/api/notifications/action` | 对通知执行操作 |
| `/api/webui/action` | WebUI 生命周期/界面动作；仅在确认当前版本语义后使用 |

轮询页面不可见时应降低频率或停止。日志、Peer 和统计接口要设置数量上限，避免用高频全量请求拖慢 BitComet。

## 配置扩展

-   Client Filter：`/api/config/client_filter/get`、`set`、`upload`、`download`、`clear`、`update`、`query`；
-   Tracker 列表更新：`/api/config/bt_tracker/update`、`/api/config/bt_tracker/query`；
-   移动端配置：`/api/config/mobile_app/get`、`/api/config/mobile_app/set`；
-   绑定设备：`/api/config/bound_devices/get`、`/api/config/bound_device/rename`、`/api/config/bound_device/remove`。

删除绑定设备会使相应设备 Token 失效。管理界面必须清楚显示正在删除哪台设备，并避免删除当前会话而没有重新登录路径。

CometID：

```
/api/cometid/sign_in
/api/cometid/sign_out
/api/cometid/query
```

RSS 路由包括 feed 列表、单 feed、条目筛选/排序和条目动作，例如：

```
/api/rss_feeds/get
/api/rss_feeds/add
/api/rss_feeds/action
/api/rss_feed/get_items
/api/rss_feed/filter_items
/api/rss_feed/sort_items
/api/rss_feed/items_action
```

这些模块有各自的状态机和隐私要求。只列出按钮而不实现错误、加载和异步状态会造成误操作，建议作为第二阶段功能。

## Torrent 元数据与任务属性

高级详情可能使用：

```
/api/task/property/get
/api/task/property/set
/api/torrent/getSummary
/api/torrent/getMetadata
/api/torrent/cancelDownload
```

属性写入和元数据下载都具有明显的任务类型前置条件。调用前先根据任务对象判断类型，并处理不支持或任务已经消失的响应。

## 兼容性策略

1.  登录后读取 `version`、`platform` 和 `/api/config/about/get`。
2.  对可选模块做功能探测；404、`INVALID_REQUEST` 或缺字段时隐藏相应功能。
3.  忽略未知响应字段，给缺少的可选字段设置安全默认值。
4.  严格区分 HTTP 401、网络错误和业务 `error_code`。
5.  对目标 BitComet 版本运行任务创建、启动/停止、删除、配置读写和文件播放的端到端测试。