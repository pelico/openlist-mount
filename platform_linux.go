//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const platformName = "linux"

// defaultDataDir 返回 Linux 下的默认数据目录。
func defaultDataDir() string { return "/var/lib/openlist-mount" }

// defaultCacheDir 返回 Linux 下的默认缓存目录。
// 注意:这是普通磁盘目录,不是 tmpfs。writes/full 模式下文件会先整份落这里再上传,
// 所以必须靠 --vfs-cache-max-size + 本工具的缓存清理兜底,否则会把这张小盘写满。
func defaultCacheDir() string { return "/var/cache/openlist-mount" }

// probeFuseHealth 快速探测 rclone FUSE 挂载点是否"真的活着"。
// 返回 "ok" / "stale" / "unknown"。
func probeFuseHealth(pid int) string {
	if pid <= 0 {
		return "unknown"
	}
	// 看 /proc/<pid>/cmdline 里有没有 rclone mount
	cmd, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !strings.Contains(string(cmd), "rclone") {
		return "stale"
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return "stale"
	}
	// 在 Unix 上 FindProcess 不会失败,只能通过发 signal=0 探测
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return "stale"
	}
	return "ok"
}

// cleanupStaleMount 尝试多种方法清理僵尸 FUSE 挂载。
func cleanupStaleMount(mountpoint string) error {
	if mountpoint == "" {
		return nil
	}
	// 方法 1:正常 Unmount
	if err := syscall.Unmount(mountpoint, 0); err == nil {
		return nil
	}
	time.Sleep(300 * time.Millisecond)
	// 方法 2:lazy + force
	if err := syscall.Unmount(mountpoint, syscall.MNT_FORCE); err == nil {
		return nil
	}
	time.Sleep(300 * time.Millisecond)
	// 方法 3:fusermount -uF
	if _, err := exec.LookPath("fusermount"); err == nil {
		if err := exec.Command("fusermount", "-u", mountpoint).Run(); err == nil {
			return nil
		}
		if err := exec.Command("fusermount", "-uF", mountpoint).Run(); err == nil {
			return nil
		}
	}
	// 方法 4:umount -l
	if _, err := exec.LookPath("umount"); err == nil {
		if err := exec.Command("umount", "-lf", mountpoint).Run(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("failed to clean stale mount at %s", mountpoint)
}

// isMounted 通过对比 mountpoint 与其父目录的 st_dev 判断是否已挂载。
func isMounted(path string) bool {
	var st1, st2 syscall.Stat_t
	if err := syscall.Stat(path, &st1); err != nil {
		return false
	}
	if err := syscall.Stat(filepath.Dir(path), &st2); err != nil {
		return false
	}
	return st1.Dev != st2.Dev
}

// diskFreePct 返回 path 所在磁盘的剩余百分比 (0-100) 和剩余字节数。
// 出错时返回 (-1, -1)。
func diskFreePct(path string) (float64, int64) {
	total, free, ok := diskUsage(path)
	if !ok || total == 0 {
		return -1, -1
	}
	return float64(free) / float64(total) * 100.0, int64(free)
}

// diskUsage 返回 path 所在磁盘的总字节数与剩余字节数。
func diskUsage(path string) (uint64, uint64, bool) {
	if path == "" {
		return 0, 0, false
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, false
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bavail * uint64(stat.Bsize)
	return total, free, true
}

// fuseHealth 探测 FUSE 是否可用,返回 (是否可用, 缺失提示, 配置建议)。
func fuseHealth() (bool, string, string) {
	available := true
	hint := ""
	if _, err := os.Stat("/dev/fuse"); err != nil {
		available = false
		hint = "/dev/fuse 不存在,挂载会失败"
	}
	confHint := ""
	if b, err := os.ReadFile("/etc/fuse.conf"); err == nil {
		if !strings.Contains(string(b), "user_allow_other") {
			confHint = "建议在 /etc/fuse.conf 加 user_allow_other,配合 --allow-other 使用"
		}
	}
	return available, hint, confHint
}

// terminateProcess 发送 SIGTERM 请求优雅退出。
func terminateProcess(p *os.Process) {
	if p == nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
}

// forceKillProcess 强制结束进程。
func forceKillProcess(p *os.Process) {
	if p == nil {
		return
	}
	_ = p.Kill()
}

// killRcloneForMountpoint 用 pgrep+kill 干掉所有命令行里带这个挂载点的 rclone 进程。
// 用于 Start() 前清残留, 避免 FUSE 僵尸或重复挂载。
func killRcloneForMountpoint(mountpoint, remoteName string) {
	if mountpoint == "" {
		return
	}
	pattern := "rclone mount .* " + mountpoint
	// pgrep -f → 拿到 PID
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil || len(out) == 0 {
		return // 没残留
	}
	killPIDs(strings.Fields(string(out)), false)
	// 等 1s, 还活着就强杀
	time.Sleep(1 * time.Second)
	out2, _ := exec.Command("pgrep", "-f", pattern).Output()
	if len(out2) > 0 {
		killPIDs(strings.Fields(string(out2)), true)
		time.Sleep(500 * time.Millisecond)
	}
}

func killPIDs(pids []string, force bool) {
	for _, pidStr := range pids {
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid <= 1 {
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if force {
			_ = proc.Kill()
		} else {
			_ = proc.Signal(syscall.SIGTERM)
		}
	}
}

// prepareMountpoint Linux 下需要提前创建挂载点目录。
func prepareMountpoint(mountpoint string) error {
	return os.MkdirAll(mountpoint, 0o755)
}

// platformMountArgs 返回 Linux 平台特有的 rclone mount 参数。
func platformMountArgs(cfg *MountConfig) []string {
	var args []string
	if cfg.AllowOther {
		args = append(args, "--allow-other")
	}
	return args
}

// defaultExecPath 返回生成服务文件时使用的默认可执行路径。
func defaultExecPath() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "/usr/local/bin/openlist-mount"
}

// renderServiceUnit 生成 openlist-mount.service unit 内容。
func renderServiceUnit(execPath, addr, dataDir string) string {
	return fmt.Sprintf(`[Unit]
Description=openlist-mount - mount openlist WebDAV via rclone
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s -addr %s -data %s
Restart=on-failure
RestartSec=3s
# 允许 fusermount
AmbientCapabilities=CAP_SYS_ADMIN
CapabilityBoundingSet=CAP_SYS_ADMIN

[Install]
WantedBy=multi-user.target
`, execPath, addr, dataDir)
}

// openBrowser 在 Linux 下用 xdg-open 打开浏览器。
func openBrowser(url string) {
	for _, bin := range []string{"xdg-open", "sensible-browser", "x-www-browser"} {
		if _, err := exec.LookPath(bin); err == nil {
			_ = exec.Command(bin, url).Start()
			return
		}
	}
}

// setupLogging Linux 下日志直接走 stderr,由 systemd/journald 收集。
func setupLogging(dataDir string) {}