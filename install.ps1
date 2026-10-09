<#
  openlist-mount Windows 一键安装脚本
  用法(管理员 PowerShell):
    irm https://raw.githubusercontent.com/pelico/openlist-mount/main/install.ps1 | iex
    或自定义仓库:
    $env:REPO="owner/repo"; irm .../install.ps1 | iex
    或本地:
    .\install.ps1 -Repo "" -BinDir .\dist

  安装内容:
    1. WinFsp 驱动(rclone 在 Windows 上挂载磁盘必需)
    2. rclone
    3. openlist-mount.exe(托盘程序)
    4. 计划任务:登录后自动启动托盘
#>
[CmdletBinding()]
param(
  [string]$Repo    = $(if ($env:REPO) { $env:REPO } else { 'pelico/openlist-mount' }),
  [string]$Version = $(if ($env:VERSION) { $env:VERSION } else { 'latest' }),
  [string]$BinDir  = $env:BIN_DIR,
  [string]$InstallDir = $(if ($env:INSTALL_DIR) { $env:INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\openlist-mount' }),
  [string]$DataDir = $(if ($env:DATA_DIR) { $env:DATA_DIR } else { Join-Path $env:LOCALAPPDATA 'openlist-mount' }),
  [string]$Addr    = $(if ($env:ADDR) { $env:ADDR } else { ':7777' }),
  [switch]$NoAutoStart,
  [switch]$SkipDeps
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

function log($m)  { Write-Host "[$(Get-Date -Format HH:mm:ss)] $m" -ForegroundColor Cyan }
function ok($m)   { Write-Host "[$(Get-Date -Format HH:mm:ss)] ✓ $m" -ForegroundColor Green }
function warn($m) { Write-Host "[$(Get-Date -Format HH:mm:ss)] ! $m" -ForegroundColor Yellow }
function die($m)  { Write-Host "[$(Get-Date -Format HH:mm:ss)] ✗ $m" -ForegroundColor Red; exit 1 }

# ---- 0. 管理员检查 ----
$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
  die "请以【管理员身份】运行 PowerShell 后再执行本脚本(安装 WinFsp 需要管理员权限)。"
}
ok "管理员权限已确认"

# ---- 1. 架构 ----
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'a64' } else { 'x64' }
$rcloneZipArch = if ($arch -eq 'a64') { 'arm64' } else { 'amd64' }
# 发布产物名:ARM64 用 openlist-mount-a64.exe,其余用 openlist-mount.exe
$exeName = if ($arch -eq 'a64') { 'openlist-mount-a64.exe' } else { 'openlist-mount.exe' }
ok "系统架构: $arch"

$tmp = Join-Path $env:TEMP ("openlist-mount-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

try {
  # ---- 2. WinFsp ----
  if (-not $SkipDeps) {
    $winfspDll = @(
      (Join-Path $env:ProgramFiles 'WinFsp\bin\winfsp-x64.dll'),
      (Join-Path ${env:ProgramFiles(x86)} 'WinFsp\bin\winfsp-x64.dll'),
      (Join-Path $env:ProgramFiles 'WinFsp\bin\winfsp-a64.dll')
    ) | Where-Object { $_ -and (Test-Path $_) }

    if ($winfspDll) {
      ok "WinFsp 已安装"
    } else {
      log "未检测到 WinFsp,开始下载安装..."
      $msi = Join-Path $tmp 'winfsp.msi'
      Invoke-WebRequest -Uri 'https://winfsp.dev/rel/' -OutFile $msi -UseBasicParsing
      log "静默安装 WinFsp..."
      $p = Start-Process msiexec.exe -ArgumentList "/i", "`"$msi`"", '/qn', '/norestart' -Wait -PassThru
      if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { die "WinFsp 安装失败 (exit=$($p.ExitCode))" }
      ok "WinFsp 安装完成"
    }
  } else {
    warn "已跳过依赖安装(WinFsp / rclone)"
  }

  # ---- 3. rclone ----
  $rcloneExe = Join-Path $InstallDir 'rclone.exe'
  $rcloneCmd = Get-Command rclone -ErrorAction SilentlyContinue
  if ($rcloneCmd) {
    ok "rclone 已安装: $($rcloneCmd.Source)"
    $rcloneBin = $rcloneCmd.Source
  } else {
    log "未检测到 rclone,下载中..."
    $rcZip = Join-Path $tmp 'rclone.zip'
    Invoke-WebRequest -Uri "https://downloads.rclone.org/rclone-current-windows-$rcloneZipArch.zip" -OutFile $rcZip -UseBasicParsing
    Expand-Archive -Path $rcZip -DestinationPath (Join-Path $tmp 'rclone') -Force
    $found = Get-ChildItem -Path (Join-Path $tmp 'rclone') -Recurse -Filter 'rclone.exe' | Select-Object -First 1
    if (-not $found) { die "rclone 解压失败" }
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Copy-Item $found.FullName $rcloneExe -Force
    $rcloneBin = $rcloneExe
    ok "rclone 已安装到 $rcloneExe"
  }

  # ---- 4. openlist-mount.exe ----
  New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
  $targetExe = Join-Path $InstallDir 'openlist-mount.exe'

  if ($BinDir -and (Test-Path (Join-Path $BinDir $exeName))) {
    log "从本地 $BinDir 复制..."
    Copy-Item (Join-Path $BinDir $exeName) $targetExe -Force
  } elseif ($Repo) {
    if ($Version -eq 'latest') {
      $url = "https://github.com/$Repo/releases/latest/download/$exeName"
    } else {
      $url = "https://github.com/$Repo/releases/download/$Version/$exeName"
    }
    log "下载: $url"
    Invoke-WebRequest -Uri $url -OutFile $targetExe -UseBasicParsing
  } else {
    die "无法获取二进制: 请设置 -Repo <owner/repo> 或 -BinDir .\dist"
  }

  # 简易校验:PE 可执行
  $magic = [System.IO.File]::ReadAllBytes($targetExe)[0..1]
  if (-not ($magic[0] -eq 0x4D -and $magic[1] -eq 0x5A)) {
    die "下载内容不是有效的 Windows 可执行文件(可能是 404 页面)"
  }
  ok "已安装到 $targetExe"

  # ---- 5. 数据目录 ----
  New-Item -ItemType Directory -Path $DataDir -Force | Out-Null
  ok "数据目录: $DataDir"

  # ---- 6. 停掉旧进程(升级场景) ----
  Get-Process -Name 'openlist-mount' -ErrorAction SilentlyContinue | ForEach-Object {
    log "停止旧进程 PID $($_.Id)"
    Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue
  }
  # 清理该工具残留的 rclone 挂载进程
  Get-CimInstance Win32_Process -Filter "Name='rclone.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -like '*openlist_*' } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }

  # ---- 7. 计划任务:登录自启 ----
  if (-not $NoAutoStart) {
    $action  = New-ScheduledTaskAction -Execute $targetExe -Argument "-addr $Addr -data `"$DataDir`" -rclone `"$rcloneBin`""
    $trigger = New-ScheduledTaskTrigger -AtLogOn
    $setting = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
    Register-ScheduledTask -TaskName 'openlist-mount' -Action $action -Trigger $trigger -Settings $setting -RunLevel Limited -Force | Out-Null
    ok "已注册计划任务 openlist-mount(登录后自动启动托盘)"
  }

  # ---- 8. 启动托盘 ----
  Start-Process -FilePath $targetExe -ArgumentList "-addr", $Addr, "-data", "`"$DataDir`"", "-rclone", "`"$rcloneBin`""
  Start-Sleep -Seconds 2
  $port = ($Addr -split ':')[-1]
  if (-not $port) { $port = '7777' }
  ok "托盘程序已启动,Web 控制台: http://127.0.0.1:$port"

  Write-Host ""
  Write-Host "安装完成!" -ForegroundColor Green
  Write-Host ""
  Write-Host "Web 控制台:  http://127.0.0.1:$port"
  Write-Host "安装目录:    $InstallDir"
  Write-Host "数据目录:    $DataDir  (config.json / rclone.conf / 日志)"
  Write-Host ""
  Write-Host "托盘图标在右下角通知区域,右键可:打开控制台 / 全部启动 / 全部停止 / 退出"
  Write-Host "下一步: 打开 Web 控制台 → 填入 openlist WebDAV 地址(形如 http://<ip>:5244/dav)"
  Write-Host "        → 挂载点填盘符(如 X:) → 保存 → 点启动"
  Write-Host ""
} finally {
  Remove-Item -Path $tmp -Recurse -Force -ErrorAction SilentlyContinue
}