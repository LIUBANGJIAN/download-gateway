---
title: "WebUI API 调用接口 - BitComet 使用帮助"
url: "https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/?utm_source=gemini"
domain: "wiki-zh.bitcomet.com"
excerpt: "BitComet 从 v2.09 开始提供 WebUI。官方 WebUI 使用 Vue、Vuetify 和 axios，通过 JSON API 与 BitComet 通信。第三方也可以实现自己的 WebUI，但应把这里的接口视为随 BitComet 版本演进的内部公开协议，而不是已有稳定版本承诺的 OpenAPI。"
date: "2026-09-26T09:49:51.683Z"
---

BitComet 从 v2.09 开始提供 WebUI。官方 WebUI 使用 Vue、Vuetify 和 axios，通过 JSON API 与 BitComet 通信。第三方也可以实现自己的 WebUI，但应把这里的接口视为**随 BitComet 版本演进的内部公开协议**，而不是已有稳定版本承诺的 OpenAPI。

本文档依据 2026-08-11 的 BitComet 后端与官方 WebUI 源码整理。实现时应保留未知字段、检查 `error_code`，并对目标 BitComet 版本做实际兼容性测试。

> **安全要求：** 第三方 WebUI 必须使用 HTTPS。登录报文中的加密只用于兼容认证协议，不能替代 TLS；不要把密码、`invite_token`、`device_token` 或文件访问密钥写入 URL、日志和错误上报。

## 建议阅读顺序

1.  [通用请求与认证](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/authentication/)：请求头、登录、Token、错误处理和安全边界。
2.  [任务列表与任务操作](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/task-list-actions/)：列表、批量选择、启动、停止、校验和删除。
3.  [添加任务](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/add-tasks/)：HTTP、BT、磁力链接和批量添加。
4.  [任务详情](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/task-details/)：摘要、文件、Tracker、连接、Peer 和日志。
5.  [配置接口](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/configuration/)：下载目录、连接、任务、IP Filter 等设置。
6.  [文件访问与播放](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/file-access/)：受控文件访问、播放和下载。
7.  [高级接口索引](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/advanced-api-index/)：状态、通知、CometID、RSS、移动设备等可选能力。

## 最小实现范围

一个可用的第三方 WebUI 至少需要：

-   完成登录并持久化自己的 `client_id` 与 `device_token`；
-   使用 `/api_v2/task_list/get` 展示任务；
-   使用 `/api_v2/tasks/action` 与 `/api_v2/tasks/delete` 操作任务；
-   使用 `/api/config/new_task/get` 和添加任务接口创建任务；
-   使用任务摘要、文件和 Tracker/Peer 接口展示详情；
-   使用 `/api/config/about/get` 或响应中的 `version` 做版本识别。

## 接口约定速查

| 项目 | 约定 |
| --- | --- |
| API 请求 | 通常为 `POST`，请求体为 JSON |
| 内容类型 | `Content-Type: application/json` |
| 客户端标识 | `Client-Type: BitComet WebUI` |
| 登录后认证 | `Authorization: Bearer <device_token>` |
| Token 无效 | HTTP 401，通常返回 `error_code: "INVALID_TOKEN"` |
| 成功判断 | 读取每个接口的 `error_code`；历史接口存在 `OK`/`ok` 差异 |
| 版本信息 | 普通 JSON 响应通常包含 `version`、`platform`、`file_size_prefix` |

完整认证示例和字段说明见[通用请求与认证](https://wiki-zh.bitcomet.com/webui_api%E8%B0%83%E7%94%A8%E6%8E%A5%E5%8F%A3/authentication/)。

## WebUI 调用本地视频播放器

本页面详细介绍 WebUI 调用本地视频播放器的功能说明、使用方法及注意事项，为用户提供完整的操作指引。

## 1\. 功能概述

WebUI 支持通过**自定义协议**（如 `vlc:*`、`potplayer:*`）唤起用户设备上已安装的本地视频播放器（如 VLC、PotPlayer），直接播放下载任务中指定视频文件的 HTTP 链接。该功能可规避浏览器播放组件的格式限制，充分利用本地播放器的高级功能。

## 2\. 核心优势

1.  **格式兼容性强**：支持播放浏览器原生不支持的特殊格式视频（如 MKV、FLV 高清格式）。
    
2.  **功能更丰富**：可调用本地播放器的高级解码、多音轨切换、字幕调节、倍速播放等专属功能。
    
3.  **性能提升**：本地播放器可调用硬件加速功能，提升大码率视频的播放流畅度。
    

## 3\. 前置条件

以下条件需全部满足，否则无法正常唤起本地播放器。

1.  本地设备已安装**支持的视频播放器**（官方推荐：[VLC 媒体播放器](https://www.videolan.org/)、[PotPlayer](https://potplayer.daum.net/)）。
    
2.  播放器已完成**自定义协议注册**（如 `vlc://` 协议需关联 VLC 播放器）。注册方法参考下一节。
    
3.  浏览器未拦截自定义协议调用（首次使用时，浏览器可能弹出提示，需选择“允许”或“始终允许”）。
    

## 4\. 附录：协议注册方法

### 4.1 VLC 播放器协议注册

正常安装 VLC 播放器后，还需使用以下批处理文件注册自定义协议：

```
@echo off
setlocal EnableExtensions DisableDelayedExpansion

:: This installer writes a fixed protocol handler under Program Files so the
:: browser-supplied URL is never inserted into PowerShell source code.
fltmc >nul 2>&1 || (
    echo ERROR: This script requires Administrator privileges.
    echo Please right-click it and select "Run as administrator".
    pause
    exit /b 1
)

:: Locate VLC in either registry view before changing the protocol handler.
set "player_exe="
for /f "tokens=2,*" %%A in ('reg.exe query "HKLM\SOFTWARE\VideoLAN\VLC" /ve 2^>nul ^| findstr.exe /R /C:"REG_SZ"') do set "player_exe=%%B"
if not defined player_exe for /f "tokens=2,*" %%A in ('reg.exe query "HKLM\SOFTWARE\WOW6432Node\VideoLAN\VLC" /ve 2^>nul ^| findstr.exe /R /C:"REG_SZ"') do set "player_exe=%%B"

if not defined player_exe (
    echo ERROR: VLC installation information was not found.
    pause
    exit /b 1
)
if not exist "%player_exe%" (
    echo ERROR: VLC executable was not found: %player_exe%
    pause
    exit /b 1
)
echo Found VLC executable: %player_exe%

:: Install the embedded handler in an administrator-protected directory.
set "install_dir=%ProgramFiles%\BitComet\tools"
set "handler_path=%install_dir%\vlc_protocol_handler.ps1"
if not exist "%install_dir%" mkdir "%install_dir%" >nul 2>&1
if not exist "%install_dir%" (
    echo ERROR: Failed to create handler directory: %install_dir%
    pause
    exit /b 1
)

set "VLC_INSTALLER_SOURCE=%~f0"
set "VLC_HANDLER_PATH=%handler_path%"
powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$ErrorActionPreference = 'Stop'; $lines = [IO.File]::ReadAllLines($env:VLC_INSTALLER_SOURCE); $begin = [Array]::IndexOf($lines, '# POWERSHELL_HANDLER_BEGIN'); $end = [Array]::IndexOf($lines, '# POWERSHELL_HANDLER_END'); if ($begin -lt 0 -or $end -le ($begin + 1)) { throw 'Embedded handler markers are invalid.' }; $payload = $lines[($begin + 1)..($end - 1)]; $temp = $env:VLC_HANDLER_PATH + '.tmp'; [IO.File]::WriteAllLines($temp, $payload, (New-Object Text.UTF8Encoding($false))); Move-Item -LiteralPath $temp -Destination $env:VLC_HANDLER_PATH -Force"
if errorlevel 1 (
    echo ERROR: Failed to install the VLC protocol handler.
    pause
    exit /b 1
)
set "VLC_INSTALLER_SOURCE="
set "VLC_HANDLER_PATH="

:: Register the protocol machine-wide. The URL remains a quoted argument to a
:: fixed -File script and is never parsed as part of a -Command expression.
set "powershell_exe=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"
set "protocol_command=\"%powershell_exe%\" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File \"%handler_path%\" \"%%1\""
reg.exe add "HKLM\SOFTWARE\Classes\vlc" /ve /t REG_SZ /d "URL:VLC Protocol" /f >nul || goto :registration_failed
reg.exe add "HKLM\SOFTWARE\Classes\vlc" /v "URL Protocol" /t REG_SZ /d "" /f >nul || goto :registration_failed
reg.exe add "HKLM\SOFTWARE\Classes\vlc\shell\open\command" /ve /t REG_SZ /d "%protocol_command%" /f >nul || goto :registration_failed

echo VLC protocol registered successfully.
echo Handler installed at: %handler_path%
endlocal
pause
exit /b 0

:registration_failed
echo ERROR: Failed to write the VLC protocol registration.
endlocal
pause
exit /b 1

# POWERSHELL_HANDLER_BEGIN
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string] $ProtocolUrl
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

# Convert the custom protocol value into one validated HTTP(S) media URL.
function ConvertFrom-VlcProtocolUrl {
    param(
        [Parameter(Mandatory = $true)]
        [string] $Value
    )

    $prefix = 'vlc://'
    if ([string]::IsNullOrWhiteSpace($Value) -or
        -not $Value.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'The VLC protocol URL must start with vlc://.'
    }

    $target = $Value.Substring($prefix.Length)

    # Browsers serialize vlc://http://... as vlc://http//...; restore only
    # the two explicitly supported inner schemes before URI validation.
    if ($target.StartsWith('http//', [StringComparison]::OrdinalIgnoreCase)) {
        $target = 'http://' + $target.Substring('http//'.Length)
    }
    elseif ($target.StartsWith('https//', [StringComparison]::OrdinalIgnoreCase)) {
        $target = 'https://' + $target.Substring('https//'.Length)
    }

    $uri = $null
    if (-not [Uri]::TryCreate($target, [UriKind]::Absolute, [ref] $uri)) {
        throw 'The VLC protocol target is not an absolute URL.'
    }

    if (($uri.Scheme -ne 'http' -and $uri.Scheme -ne 'https') -or
        [string]::IsNullOrWhiteSpace($uri.Host) -or
        -not [string]::IsNullOrEmpty($uri.UserInfo)) {
        throw 'Only HTTP(S) URLs without embedded credentials are allowed.'
    }

    return $uri.AbsoluteUri
}

# Read VLC's executable path from protected HKLM registry views.
function Get-VlcExecutablePath {
    $views = @(
        [Microsoft.Win32.RegistryView]::Registry64,
        [Microsoft.Win32.RegistryView]::Registry32
    )

    foreach ($view in $views) {
        $baseKey = $null
        $vlcKey = $null
        try {
            $baseKey = [Microsoft.Win32.RegistryKey]::OpenBaseKey(
                [Microsoft.Win32.RegistryHive]::LocalMachine,
                $view
            )
            $vlcKey = $baseKey.OpenSubKey('SOFTWARE\VideoLAN\VLC')
            if ($null -eq $vlcKey) {
                continue
            }

            $candidate = [string] $vlcKey.GetValue('')
            if (-not [string]::IsNullOrWhiteSpace($candidate) -and
                (Test-Path -LiteralPath $candidate -PathType Leaf)) {
                return $candidate
            }
        }
        finally {
            if ($null -ne $vlcKey) {
                $vlcKey.Dispose()
            }
            if ($null -ne $baseKey) {
                $baseKey.Dispose()
            }
        }
    }

    throw 'VLC executable was not found.'
}

# HANDLER_MAIN_BEGIN
try {
    $targetUrl = ConvertFrom-VlcProtocolUrl -Value $ProtocolUrl
    $playerExe = Get-VlcExecutablePath

    # The executable and URL are separate arguments; no dynamic code is used.
    & $playerExe $targetUrl
}
catch {
    Write-Error $_.Exception.Message
    exit 1
}
# POWERSHELL_HANDLER_END
```

将以上代码保存为 `vlc_reg_v2.bat`，右键单击并选择“以管理员身份运行”。用户只需保存并运行这一个 BAT 文件；脚本会把内嵌的 PowerShell 处理程序安装到 `%ProgramFiles%\BitComet\tools\vlc_protocol_handler.ps1`，并注册 VLC 自定义协议。

### 4.2 PotPlayer 播放器协议注册

正常安装 PotPlayer 播放器（64 位版）后，默认已配置 `potplayer` 自定义协议。如果自定义协议失效，可使用以下批处理文件进行修复：

```
@echo off
setlocal enabledelayedexpansion

:: Check if the script is running with Administrator privileges
fltmc >nul 2>&1 || (
    echo ERROR: This script requires Administrator privileges!
    echo Please right-click and select "Run as administrator".
    pause
    exit /b 1
)

:: Find PotPlayer installation directory
for /f "tokens=1,2* delims=:" %%a in (
  'reg query "HKLM\SOFTWARE\DAUM\PotPlayer64" /v "ProgramPath" 2^>nul ^| findstr "ProgramPath"'
) do (
  for /f "tokens=1,2,3" %%l in ("%%a") do (
    set "player_disk=%%n"
  )
  set "player_exe=!player_disk!:%%b"
)

:: Verify PotPlayerMini64.exe exists
if not exist "!player_exe!" (
  echo PotPlayer executable not found: !player_exe!
  pause
  exit /b 1
) else (
  echo Found PotPlayer executable path: !player_exe!
)

:: Start registering registry entries

:: Create potplayer root key with default value
reg add "HKCR\potplayer" /ve /t REG_SZ /d "URL:PotPlayer Protocol" /f >nul

:: Create URL Protocol entry (empty value)
reg add "HKCR\potplayer" /v "URL Protocol" /t REG_SZ /d "" /f >nul

:: Create shell\open\command entry with properly quoted executable path
reg add "HKCR\potplayer\shell\open\command" /ve /t REG_SZ /d "\"!player_exe!\" \"%%1\"" /f >nul

:: Verify registration result
if %errorlevel% equ 0 (
  echo PotPlayer protocol registered successfully!
) else (
  echo Failed to write to registry. Please run this script as Administrator.
  pause
  exit /b 1
)

endlocal
pause
```

将此 BAT 文件保存到本地后，右键单击“以管理员权限运行”，即可注册 PotPlayer 自定义协议。

## 彗星通行证

### 什么是彗星通行证？

彗星通行证是BitComet推出的统一帐号系统，拥有彗星通行证不仅能直接登录BitComet软件享受资源快速下载，还可在所有支持产品中无需再次注册直接登录。

### 如何注册彗星通行证？

请访问以下页面：[注册彗星通行证](http://www.cometpass.com/passport/register)

### 如何在BitComet客户端登录彗星通行证？

打开BitComet程序，主菜单 -> "彗星通行证(P)" -> "登陆彗星通行证"，弹出登录对话框，输入帐号、密码。

### 如何找回登录密码？

请访问以下页面：[忘记密码](http://www.cometpass.com/passport/retrieve)

### 如何查看我的积分和等级？

请访问以下页面：[我的积分](http://www.cometpass.com/accounts/score)

### 彗星通行证的积分规则是什么？

累计积分＝在线时长积分＋文件上传量积分

1、注册用户在线1小时得2分，每天20分封顶；

2、注册用户上传文件每10MB积1分,每天100分封顶；

3、注册用户BT下载，HTTP/FTP下载、上传积分，暂未计算。

积分更新：标准格林威治时间的O点 【北京时间：早晨 8：00】

### 彗星通行证可以加速下载吗？

是的！在BitComet客户端登录彗星通行证可以加速下载。在BT本身的连接不改变的情况下，增加长效种子下载连接数，等级越高的通行证用户能够连接到的长效种子最大连接数就越多，连接越多下载速度越快。（具体数据参考下图）

另外，如果没有登录彗星通行证，或因为服务器原因登录失败，并不会影响您的正常下载。这种情况下，长效种子下载连接数上限为默认的40个。

-   **积分等级以及称号：**

| 等级 | 积分 | 称号 | 称号(英文) | 最大资源 |
| --- | --- | --- | --- | --- |
| 1 | 0 | 彗星新兵 | Recruit | 41 |
| 2 | 300 | 彗星列兵 | Private | 42 |
| 3 | 600 | 彗星下士 | Corporal | 43 |
| 4 | 1000 | 彗星中士 | Sergeant | 44 |
| 5 | 1500 | 彗星上士 | Staff Sergeant | 45 |
| 6 | 2100 | 彗星少尉 | Second Lieutenant | 46 |
| 7 | 2800 | 彗星少尉 | Second Lieutenant | 47 |
| 8 | 3600 | 彗星中尉 | Lieutenant | 48 |
| 9 | 4500 | 彗星中尉 | Lieutenant | 49 |
| 10 | 5500 | 彗星上尉 | Captain | 50 |
| 11 | 6600 | 彗星上尉 | Captain | 51 |
| 12 | 7800 | 彗星上尉 | Captain | 52 |
| 13 | 9100 | 彗星少校 | Major | 53 |
| 14 | 10500 | 彗星少校 | Major | 54 |
| 15 | 12000 | 彗星少校 | Major | 55 |
| 16 | 13600 | 彗星中校 | Lieutenant Colonel | 56 |
| 17 | 15300 | 彗星中校 | Lieutenant Colonel | 57 |
| 18 | 17100 | 彗星中校 | Lieutenant Colonel | 58 |
| 19 | 19000 | 彗星中校 | Lieutenant Colonel | 59 |
| 20 | 21000 | 彗星上校 | Colonel | 60 |
| 21 | 23100 | 彗星上校 | Colonel | 61 |
| 22 | 25300 | 彗星上校 | Colonel | 62 |
| 23 | 27600 | 彗星上校 | Colonel | 63 |
| 24 | 30000 | 彗星上校 | Colonel | 64 |
| 25 | 32500 | 彗星大校 | Senior Colonel | 65 |
| 26 | 35100 | 彗星大校 | Senior Colonel | 66 |
| 27 | 37800 | 彗星大校 | Senior Colonel | 67 |
| 28 | 40600 | 彗星大校 | Senior Colonel | 68 |
| 29 | 43500 | 彗星大校 | Senior Colonel | 69 |
| 30 | 46500 | 彗星少将 | Major General | 70 |
| 31 | 49600 | 彗星少将 | Major General | 71 |
| 32 | 52800 | 彗星少将 | Major General | 72 |
| 33 | 56100 | 彗星少将 | Major General | 73 |
| 34 | 59500 | 彗星少将 | Major General | 74 |
| 35 | 63000 | 彗星少将 | Major General | 75 |
| 36 | 66600 | 彗星中将 | Lieutenant General | 76 |
| 37 | 70300 | 彗星中将 | Lieutenant General | 77 |
| 38 | 74100 | 彗星中将 | Lieutenant General | 78 |
| 39 | 78000 | 彗星中将 | Lieutenant General | 79 |
| 40 | 82000 | 彗星中将 | Lieutenant General | 80 |
| 41 | 86100 | 彗星中将 | Lieutenant General | 81 |
| 42 | 90300 | 彗星上将 | General | 82 |
| 43 | 94800 | 彗星上将 | General | 83 |
| 44 | 99000 | 彗星上将 | General | 84 |
| 45 | 103500 | 彗星上将 | General | 85 |
| 46 | 108100 | 彗星上将 | General | 86 |
| 47 | 112800 | 彗星上将 | General | 87 |
| 48 | 117600 | 彗星元帅 | Marshal | 88 |
| 49 | 127600 | 彗星元帅 | Marshal | 89 |
| 50 | 137600 | 彗星元帅 | Marshal | 90 |
| 51 | 147600 | 彗星元帅 | Marshal | 91 |
| 52 | 157600 | 彗星元帅 | Marshal | 92 |
| 53 | 167600 | 彗星大元帅 | Generalissimo | 93 |
| 54 | 177600 | 彗星大元帅 | Generalissimo | 94 |
| 55 | 187600 | 彗星大元帅 | Generalissimo | 95 |
| 56 | 197600 | 彗星大元帅 | Generalissimo | 96 |
| 57 | 207600 | 彗星大元帅 | Generalissimo | 97 |
| 58 | 257600 | 彗星元首 | Sovereign | 98 |
| 59 | 307600 | 彗星元首 | Sovereign | 99 |
| 60 | 357600 | 彗星元首 | Sovereign | 100 |
| 61 | 407600 | 彗星元首 | Sovereign | 101 |
| 62 | 457600 | 彗星元首 | Sovereign | 102 |

BitComet软件隐私政策旨在向您介绍在使用BitComet软件和服务时，我们如何处理您的个人信息。

我们收集的信息

-   BitComet软件仅向BitComet自动发送标准的，数量有限的信息，这些信息可能会保留在BitComet的服务器日志中。但是我们不会将这些信息和您的个人身份信息相关联。BitComet软件不会发送任何有关您下载的文件的信息（如下载任务的链接地址）或者种子文件对应的文件信息给 BitComet，除非您在HTTP/FTP下载时启用“自动寻找镜像”。
-   如果您启用HTTP/FTP下载的“自动寻找镜像站点”功能，当您开始下载时，可能会将您正在下载的任务信息包含链接地址发送给 BitComet服务器，用于帮助您寻找更多的镜像，提高下载速度。但是这些信息并不会与您的个人身份信息相关联。
-   如果您进行的是BitTorrent下载， BitComet软件会将正在下载的任务标识信息（如种子文件的Hash代码）发送给该任务指定的Tracker以寻找其他用户并和他们交换该任务对应的文件的数据内容。而不会将相关的信息发送给BitComet本身。并且，除了该任务对应的文件内容，其他文件内容不会被发送给服务器或者其他用户。
-   为您提供访问CometZone，彗星通行证等服务的BitComet软件的相关功能受这些产品各自的隐私政策约束。
-   BitComet软件中包含的第三方站点的链接或者第三方搜索功能会将搜索查询等信息发送到非BitComet运作的站点，这些链接或者搜索的隐私政策不受BitComet软件隐私政策制约。
-   BitComet软件会定期与BitComet服务器联系，以自动检查最新的版本。在联系过程中只检查您电脑中BitComet软件的相关技术数据，比如上一次更新时间，这些数据不包含您的个人身份信息。

使用：

-   为了更好的为您提供服务，我们可能处理一些与您个人身份信息完全无关而来自于您的数据。比如，BitComet软件可能将你发出的搜索请求处理并且转发给你指定的站点。不过这一过程完全受您控制，您可以选择您要发送请求的站点。
-   同时，我们也可能存储一些您曾经提交的数据。比如BitComet可能将您曾经提交的下载任务的链接地址存储在服务器上，可能在将来把这些链接地址作为镜像提供给其他用户。但是服务器不会记录是谁提交了这些数据，也就是说这些数据不包含您的个人身份信息。
-   此外，我们会利用您的BitComet软件更新时提交的信息整理出BitComet软件总体的使用情况，以便提高BitComet软件的服务。从这些整体数据中，无法判断出任何个人信息。比如：当您的BitComet软件自动更新时，软件会自动联系BitComet服务器检查最新的软件版本，此时我们可能会记录您安装的BitComet软件的版本，所在的地理位置，操作系统语言等信息。并将其余其他用户提供的信息汇总在一起，形成一个总数，记录下来，而非将单个用户提交的数据记录下来。

您的选择：

-   您随时可以在“下载任务”的“高级属性”中停止使用“自动寻找镜像站点”。您利用BitComet软件进行的任何下载活动都不会与 BitComet的服务器发生联系。
    
-   如果您不同意本隐私政策，可以不使用或者停止使用BitComet软件。如果BitComet软件的上述隐私政策发生重大变化，我们将会在 www.BitComet.com上宣布，届时您可以选择停止使用BitComet软件。
    
    ```
    BitComet软件隐私政策 2006年12月13日
    ```
    

## BitComet 常见问题

## 软件介绍

### 什么是 BitComet(比特彗星)?

BitComet 是一款功能强大、速度快捷、易于使用、完全免费的BitTorrent下载客户端。您可以使用BitComet打开torrent文件进行BT下载。

### BitComet 支持哪些操作系统?

目前只支持 Windows。(Windows 98/Me/2000/XP/2003/Vista/2008/7)

在 Windows2000/XP 系列平台支持 Unicode, 以及 ICS/ICF, UPnP。

## 使用答疑

### "任务名.piece\_part.bc!"是什么文件?

　　如果您下载的BT任务含有多个文件，并且只勾选了其中的部分文件下载，那么下载目录里就可能出现一个特殊的文件："任务名.piece\_part.bc!"。这个文件的作用要从bittorrent下载协议对多文件下载的处理说起。早期bittorrent协议里的文件分块Hash校验码是把文件分成固定大小的数据块后依次进行Hash计算生成的。如果一个BT任务里有多个文件，那么Hash计算到某个文件末尾最后一个分块时，会把下一个文件头部的数据直接拼接到一起来计算，这样就会造成该分块的Hash校验码与前后相邻的两个文件头尾各一部分数据都有关。如果文件大小比分块大小还要小，那么这个分块甚至会包含多个小文件的数据。在这种情况下，为了在下载数据后能够正确校验这个数据块的正确性，就不得不把该数据块所有相关文件的头尾部分数据下载回来后再一起进行Hash校验。如果用户选择了只下载这个分块所有相关文件里的部分文件，那么其它没有选中下载的文件也需要下载这个分块里的一小段数据。这些额外下载的文件数据就被保存到了"任务名.piece\_part.bc!"里。

　　由此可见，"任务名.piece\_part.bc!" 文件里的数据是用来对相邻文件边界处的分块进行Hash校验的。在BitComet v1.01及其以前版本，并没有 "任务名.piece\_part.bc!" 的设计，因而对相邻文件边界处的分块数据校验是不完善的：仅在下载该分块时会进行一次Hash校验，任务停止后再进行完整性检查就会忽略这个数据不完整的分块，只检查其它分块。为了修复这个隐患，BitComet 从 v1.02版起，开始使用 "任务名.piece\_part.bc!" 文件保存相邻文件边界处的分块数据。为了尽量减少这个临时文件的大小，避免浪费磁盘空间，只有部分数据被用户选中需要下载、部分数据没有被用户选中的分块才会被写入这个文件。

　　值得指出的是，为了避免出现一个分块含有多个文件数据造成的麻烦，BitComet从v0.85版开始引入了"文件边界按分块大小对齐"的功能。其原理是：通过在制作torrent文件时向文件最后一个分块里加入无用的填充数据，使该分块不再含有下一个文件的数据，从而避免了下载该文件时还需要下载相邻文件头部少量数据的复杂处理。对这类改进型的torrent文件，BitComet不会生成 "任务名.piece\_part.bc!" 文件，但仍然可对选中下载的单个文件进行完整的Hash检查。详细介绍请参阅下一个问题。

### 文件列表里为什么会出现”*padding\_file*?*如果您看到此文件，请升级到BitComet(比特彗星)0.85或以上版本*\_\_\_"?

　　这些特殊的文件用于附加到多文件BT任务里每个文件的结尾处，使下一个文件的起始位置与Hash校验分块的边界对齐。BitComet从v0.85版开始引入了"文件边界按分块大小对齐"的功能。其原理是：在制作含有多文件的torrent文件时，向每个文件最后一个分块里加入无用的填充数据，使该分块不再含有下一个文件的数据，从而避免了下载该文件时还需要下载相邻文件头部少量数据的复杂处理。为了兼容旧版本的BitComet以及其它的bittorrent客户端，这部分无用的填充数据以"padding\_file"的形式存在于每个文件的后面。BitComet在英文界面下生成的这类特殊文件命名为”*padding\_file*?如果你看到这个文件，请升级到BitComet 0.85或者更高版本\_\_\_\_"。

　　**提示**：由于这个特殊文件本身不含有任何有用数据，BitComet v0.85及以后版本在BT任务的文件列表中会自动隐藏这些文件，并且也不会去下载这些文件的数据，以免浪费网络带宽。如果您在使用旧版本的BitComet或其它的bittorrent客户端时看到了这些特殊文件，可以选择不要下载这些文件以节省网络带宽及磁盘空间。

### 为什么有的BT任务下载到99.9%后等了很长时间都无法完成?

　　可能造成这个现象的原因比较多，目前已知的原因包括：

-   torrent文件发布时间较早，已经没有完整的BT种子可供下载了，BT任务健康度小于100%。这种情况下除非有人补种，或有人提供长效种子上传，或在emule插件里能够找到相同文件继续下载，否则永远无法完成。不过对于视频文件而言，差一点点数据基本不会影响正常播放了。
-   BT任务里除了有视频文件外还有一些很小的图片或文本文件，视频文件已经通过长效种子很快下载完成了，图片等小文件没有长效种子源，下载很慢。这种情况可以选择不下载图片等小文件。
-   早期BitComet软件的bug。对于相邻文件边界处的分块，早期BitComet软件可能会由于下载到错误数据而反复重新下载，造成长时间无法完成。对这种情况首先推荐升级到最新版BitComet。对旧版BitComet可以尝试先停止任务后再重新启动任务，也能提高快速下载完成的几率。

### 为什么有的BT任务下载完成后文件进度会变成99.9%?

　　可能造成这个现象的原因比较多，目前已知的原因包括：

-   用户不小心删除了"任务名.piece\_part.bc!"文件。这个文件里含有相邻文件边界处的分块数据，删除后会造成文件边界处的分块数据无法进行Hash检查，从而使文件进度下降到99.9%。遇到这种情况可以先对BT任务进行完整性检查，然后再启动任务下载一会儿即可恢复到100%。
-   用户退出BitComet后删除了下载的部分文件，下次运行BitComet时再切换相关文件的选中下载状态，未删除的文件进度也可能会变成99.9%。这是由于用户手工删除的文件含有相邻文件边界处的分块数据，造成未删除的文件头尾分块不完整、无法进行Hash检查，从而引起文件进度下降。解决方法同上。为避免发生这种情况，对要删除的文件应先在BitComet中切换为禁止下载后再删除。这样操作的话BitComet就会将文件边界处的分块数据保存到"任务名.piece\_part.bc!"文件，从而避免之后发生文件进度下降。
-   早期BitComet软件的bug。早期BitComet软件在切换文件选中下载状态时的bug会造成文件进度下降。这种情况只需要重新检查任务完整性即可恢复到100%。

### 重新启动BitComet后任务列表丢失了怎么办?

-   可能造成这个现象的原因：异常关闭BitComet，下载列表保存失败，导致任务丢失。
    
-   恢复任务列表中BT任务的方法：
    
    1.  打开 种子存档，在种子存档列表中保存有已添加过的BT任务的种子文件。双击种子文件，提示：任务已经不存在，是否创建新任务。选择"是"，弹出BT任务下载对话框，“保存设置” 中设置任务的下载路径和原来任务的下载路径一致。确认下载，自动检查完整性。已经下载完的任务，显示已完成；未下载完成的任务，继续原来的进度下载。
    2.  V1.18及其之后版本，有自动备份任务列表的功能。在BitComet安装目录下，自动生成Downloads.xml.xxxxxxxx.back(xxxxxxxx为某个日期)。（Win\_Vista/7下还可能在“%SystemDrive%\\Users\\用户\\AppData\\Local\\VirtualStore\\Program Files\\BitComet”目录下生成备份文件）先关闭BitComet程序，到相应目录下删除Downloads.xml文件，再使用最近日期的back文件恢复Downloads.xml（去掉后面的日期后缀，重命名文件为Downloads.xml）。再次打开BitComet就可以看到丢失的任务列表。（最近新添加的任务可能无法恢复，需要按照方法一恢复。）
-   **提示**：v1.18之前版本用户若多次发生任务列表丢失的情况，请升级到V1.18及其以上版本，备份任务列表。
    
    -   手动备份任务列表和选项设置的方法： 打开 主菜单 -> “文件(F)” -> "导入和导出(I)"，导出 .bc\_bak 后缀的备份文件。当任务列表丢失后，打开 主菜单 -> “文件(F)” -> "导入和导出(I)"，再导入 .bc\_bak 后缀的备份文件。

### 如何将未下载完成的BT任务转移到另一台电脑继续下载?

请按照以下步骤转移未下载完成的BT任务。

1.  在BitComet 任务列表中选中 未下载完成的BT任务，点击鼠标右键，选择“种子文件另存为...”，保存种子文件到移动存储设备上。
2.  仍然在BitComet 任务列表中选中 未下载完成的BT任务。点击 工具栏 -> “查看” 按钮。 找到BT任务的下载目录或下载文件，将下载目录或下载文件保存到移动存储设备上。**注意**：不要修改目录名称或文件名称。 1. 通过移动存储设备将第1步和第2步中保存的 下载目录或下载文件 、种子文件 拷贝到另一台电脑上。 1. 在新的电脑上打开BitComet程序，添加 第1步中保存的种子文件，弹出BT任务下载对话框。 1. 在BT任务下载对话框中，设置“保存位置”为第3步中 下载目录或下载文件在新电脑中的路径。 1. 最后点击“立即下载”，会提示下载目录已经存在，是否继续下载。选择“是” ，自动检查完整性后，会继续原来的进度下载。

V1.20及其以后版本支持导入BitComet、uTorrent、迅雷的未下载完成的BT任务继续下载。

### 怎样用电驴插件继续下载没有种子的BT任务?

1.  打开“全局选项”->[“电驴下载”](https://wiki-zh.bitcomet.com/bitcomet%E5%85%A8%E5%B1%80%E9%80%89%E9%A1%B9/#%E7%94%B5%E9%A9%B4%E4%B8%8B%E8%BD%BD)，确认已经安装匹配版本的电驴插件，并已经启动电驴插件。
2.  在BitComet 任务列表中选中 没有种子的BT任务，在任务信息面板中打开“[文件列表](https://wiki-zh.bitcomet.com/%E4%BB%BB%E5%8A%A1%E4%BF%A1%E6%81%AF%E9%80%89%E9%A1%B9%E5%8D%A1/#%E6%96%87%E4%BB%B6)”面板。
3.  在文件列表中选中想要下载的文件，点击鼠标右键，选择“搜索此文件的ED2K链接(S)”，弹出 搜索ED2K链接对话框。
4.  在搜索ED2K链接对话框中，点击搜索，找出搜索结果中匹配的文件，设置ED2K链接。（可以参考[使用电驴插件](https://wiki-zh.bitcomet.com/%E4%BD%BF%E7%94%A8%E7%94%B5%E9%A9%B4%E6%8F%92%E4%BB%B6/)。）
5.  设置ED2K链接成功后，重新启动BT任务。

### DHT节点数为0，怎么解决?

-   可能造成这个现象的原因：当前网络上默认的DHT节点处于维护状态，新用户无法直接加入DHT网络。
    
-   处理办法：
    
    1.  一般来说打开BitComet后，等待一会儿就可以连上DHT节点。如果没有连接上，只要打开一个包含DHT信息的种子，通过连接已经连入DHT节点的用户即可加入DHT网络。 也就是说打开一个热门种子，进行上传或下载，基本上能很快连入DHT网络。
    2.  在已经连入DHT网络的情况下，备份 安装目录或Application Data目录的rules子目录下dhtnodes.dat文件。当以后遇到DHT节点数为0的时候，可以覆盖dhtnodes.dat。

### 高级设置里的P2PCache是什么?

P2PCache是指利用缓存技术，将P2P内容保存在缓存服务器上，这样使用P2P方式下载的用户就可以直接从缓存服务器上获取相应资料，从而加速用户的下载速度，同时减轻运营商Internet出口带宽压力，有力保障用户正常使用网络。该功能需要ISP支持。

详细信息可参阅网站 [www.p2pcache.org](http://www.p2pcache.org/)

### 左下方的通行证登录区域显示为乱码，怎么办?

这个问题通常由于Outlook引起的，导致IE不能正确打开MHT文件。

方法一、请按以下步骤修复：

1.  Windows开始菜单 -> 运行 -> 输入"regsvr32 inetcomm.dll"(不要引号)，确定。
2.  如果上一步操作因为"缺少inetcomm.dll文件"导致失败，请从"C:\\Windows\\System32\\Dllcache"目录中拷贝inetcomm.dll到"C:\\Windows\\System32".
3.  重复步骤1，如果出现"缺少相应模块"提示而导致操作失败，请找到msoert2.dll和inetres.dll，将它们复制到"C:\\Windows\\System32".
4.  重复步骤1，应该可以工作。

方法二、请按以下步骤修复：

-   下载附件，解压后，双击.reg文件，即可。![修正登录区域乱码](https://wiki-zh.bitcomet.com/media/mht.zip)

### 什么是Magnet URI(磁链)?

[magnet URI 计划](http://magnet-uri.sourceforge.net/)是一个开放的标准，规范定义了Magnet Links(磁力链接)。Magnet URI(磁链)主要用于寻找P2P网络中的可用资源，其资源定位方式是基于内容本身或元数据，而不是资源的名字或位置。一般意义上的URI可划分为URN和URL两类，Magnet URI根据上述特性可以认为是一种统一资源名称(URN)，而不是统一资源定位符(URL)。虽然它可以使用在其它应用上，但主要用途还是P2P方面，因为它可以不依赖网络服务器寻找到资源。

Magnet URI最常见的应用是根据文件内容的hash生成一个独特的指纹，有点类似于图书出版物编号ISBN。Magnet URI的一个优势是开放性和平台独立性：同一个Magnet URI可以在几乎所有的操作系统平台上进行下载上传的数据分享。由于Magnet URI是简洁的纯文本格式，所以可以通过电子邮件或即时消息的形式进行分享传播。

在BT下载程序中的应用：发布者可根据一个torrent文件的hash生成一个Magnet URI，再进一步利用DHT网络来传播这个Magnet URI对应的torrent文件，从而让其他用户能够进行BT下载。对普通用户而言，与常规BT下载的区别就是可先利用Magnet URI来获取torrent文件，而不是直接从网站下载torrent文件。在获得torrent文件后的下载方式和常规BT下载方式一样。

Magnet URI通常包含一个或多个参数，这些参数的顺序并不重要，参数的格式和HTTP链接结尾部分的查询字符串类似。最常见的参数是“xt”，例如：

magnet:?xt=urn:sha1:YNCKHTQCWBTRNJIV4WNAE52SJUQCZO5C

Magnet URI的一些常用参数：（[详见wikipedia](http://en.wikipedia.org/wiki/Magnet_URI_scheme))

-   dn (Display Name) - 资源名称
-   xl (eXact Length) - 资源大小
-   xt (eXact Topic) - 资源特征码
-   as (Acceptable Source) - 文件的在线网络链接
-   xs (eXact Source) - P2P链接
-   kt (Keyword Topic) - 搜索关键词
-   mt (Manifest Topic) - 用一个URI指向一个列表
-   tr (address Tracker) - Tracker服务器地址

BitComet从v1.17版本开始支持Magnet URI：文件菜单里可以打开Magnet URI进行下载，任务列表右键菜单里可以得到已存在任务的Magnet URI。

BitComet支持的Magnet URI参数见([附表](https://wiki.bitcomet.com/inside-bitcomet/#magnet-uri-format)）。