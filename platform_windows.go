//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const platformName = "windows"

// stillActive 是 GetExitCodeProcess 表示进程仍在运行的哨兵值 (STATUS_PENDING)。
const stillActive = 259

var (
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procGetLogicalDrives    = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW       = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// defaultDataDir 返回 Windows 下的默认数据目录。
// 优先 %LOCALAPPDATA%:托盘以登录用户身份运行,而 %ProgramData% 对普通用户可能是只读的。
func defaultDataDir() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "openlist-mount")
	}
	if d := os.Getenv("ProgramData"); d != "" {
		return filepath.Join(d, "openlist-mount")
	}
	return "openlist-mount-data"
}

// defaultCacheDir 返回 Windows 下的默认缓存目录(%LOCALAPPDATA%\openlist-mount\cache)。
// Windows 没有 tmpfs,缓存落在本地磁盘,由 --vfs-cache-max-size 控制上限。
func defaultCacheDir() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "openlist-mount", "cache")
	}
	return filepath.Join(os.TempDir(), "openlist-mount-cache")
}

// probeFuseHealth 通过进程句柄探测 rclone 是否仍存活。
func probeFuseHealth(pid int) string {
	if pid <= 0 {
		return "unknown"
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "stale"
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return "stale"
	}
	if code == stillActive {
		return "ok"
	}
	return "stale"
}

// cleanupStaleMount 在 Windows 上卸载 = 结束 rclone 进程,
// 进程清理已由 killRcloneForMountpoint 完成,这里无需额外操作。
func cleanupStaleMount(mountpoint string) error {
	return nil
}

// isMounted 判断挂载点(盘符或目录)当前是否存在。
// Windows 上 rclone+WinFsp 通常挂成盘符,故以盘符是否存在为准。
func isMounted(path string) bool {
	if drive := driveLetterOf(path); drive != "" {
		mask, _, _ := procGetLogicalDrives.Call()
		bit := uint32(drive[0] - 'A')
		return mask&(1<<bit) != 0
	}
	// 目录挂载:能 stat 且非空视为存在
	if _, err := os.Stat(path); err == nil {
		return false // 目录总是存在,无法据此判断挂载态
	}
	return false
}

// diskUsage 返回 path 所在卷的总字节数与剩余字节数。
func diskUsage(path string) (uint64, uint64, bool) {
	root := volumeRoot(path)
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, 0, false
	}
	var freeAvail, total, totalFree uint64
	r1, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r1 == 0 || total == 0 {
		return 0, 0, false
	}
	return total, totalFree, true
}

// diskFreePct 返回 path 所在磁盘的剩余百分比 (0-100) 和剩余字节数。
func diskFreePct(path string) (float64, int64) {
	total, free, ok := diskUsage(path)
	if !ok || total == 0 {
		return -1, -1
	}
	return float64(free) / float64(total) * 100.0, int64(free)
}

// fuseHealth 探测 WinFsp 是否已安装(Windows 上等价于 FUSE 的角色)。
func fuseHealth() (bool, string, string) {
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "WinFsp", "bin", "winfsp-x64.dll"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "WinFsp", "bin", "winfsp-x64.dll"),
		filepath.Join(os.Getenv("ProgramFiles"), "WinFsp", "bin", "winfsp-a64.dll"),
	}
	for _, p := range candidates {
		if p == "" || strings.HasPrefix(p, "WinFsp") {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return true, "", ""
		}
	}
	return false, "未检测到 WinFsp,请先安装:https://winfsp.dev/rel/",
		"Windows 平台需要 WinFsp 驱动才能把远程挂载成本地盘符"
}

// terminateProcess Windows 没有 SIGTERM,直接 TerminateProcess。
func terminateProcess(p *os.Process) {
	if p == nil {
		return
	}
	_ = p.Kill()
}

// forceKillProcess 强制结束进程。
func forceKillProcess(p *os.Process) {
	if p == nil {
		return
	}
	_ = p.Kill()
}

// killRcloneForMountpoint 用 PowerShell 按命令行匹配,结束挂载该 remote 的 rclone 进程。
// 精确到 remoteName,避免误杀用户其它 rclone 任务。
func killRcloneForMountpoint(mountpoint, remoteName string) {
	if remoteName == "" {
		return
	}
	script := fmt.Sprintf(
		`Get-CimInstance Win32_Process -Filter "Name='rclone.exe'" | `+
			`Where-Object { $_.CommandLine -like '*%s*' } | `+
			`ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`,
		remoteName)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run()
	time.Sleep(500 * time.Millisecond)
}

// prepareMountpoint Windows 下若挂载点是盘符则无需创建目录。
func prepareMountpoint(mountpoint string) error {
	if driveLetterOf(mountpoint) != "" {
		return nil
	}
	return os.MkdirAll(mountpoint, 0o755)
}

// platformMountArgs 返回 Windows 平台特有的 rclone mount 参数。
func platformMountArgs(cfg *MountConfig) []string {
	name := cfg.Name
	if name == "" {
		name = "openlist"
	}
	return []string{"--volname", name}
}

// defaultExecPath 返回生成服务配置时使用的默认可执行路径。
func defaultExecPath() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "openlist-mount.exe"
}

// renderServiceUnit 生成 Windows 开机自启(计划任务)的 PowerShell 片段。
func renderServiceUnit(execPath, addr, dataDir string) string {
	return "# ── Windows 开机自启:注册计划任务(登录后自动启动托盘) ──\n" +
		"$exe = '" + execPath + "'\n" +
		"$args = '-addr " + addr + " -data \"" + dataDir + "\"'\n" +
		"$action  = New-ScheduledTaskAction -Execute $exe -Argument $args\n" +
		"$trigger = New-ScheduledTaskTrigger -AtLogOn\n" +
		"$setting = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries\n" +
		"Register-ScheduledTask -TaskName 'openlist-mount' -Action $action -Trigger $trigger -Settings $setting -RunLevel Limited -Force\n"
}

// openBrowser 在 Windows 下用 start 打开默认浏览器。
func openBrowser(url string) {
	_ = exec.Command("cmd", "/c", "start", "", url).Start()
}

// setupLogging Windows 托盘模式没有控制台,日志写入数据目录下的 openlist-mount.log。
func setupLogging(dataDir string) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "openlist-mount.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(f)
}

// driveLetterOf 从路径中提取盘符(如 "X:"),非盘符路径返回空串。
func driveLetterOf(path string) string {
	if len(path) >= 2 && path[1] == ':' {
		c := path[0]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return strings.ToUpper(string(c)) + ":"
		}
	}
	return ""
}

// volumeRoot 返回路径所在卷的根(如 "C:\"),用于磁盘空间查询。
func volumeRoot(path string) string {
	if d := driveLetterOf(path); d != "" {
		return d + `\`
	}
	if wd, err := os.Getwd(); err == nil {
		if d := driveLetterOf(wd); d != "" {
			return d + `\`
		}
	}
	return `C:\`
}