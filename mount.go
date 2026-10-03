package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MountConfig 描述单个 openlist 挂载项。
type MountConfig struct {
	ID          string `json:"id"`
	Name        string `json:"name"`             // 友好显示名
	URL         string `json:"url"`              // openlist WebDAV 根地址,如 http://192.168.1.2:5244/dav
	Username    string `json:"username"`
	Password    string `json:"password"`         // 明文,仅在内存与 config.json 中,本地落盘注意权限
	Mountpoint  string `json:"mountpoint"`       // /mnt/openlist
	AllowOther  bool   `json:"allow_other"`
	DirCache    string `json:"dir_cache"`        // 默认 24h
	AttrTime    string `json:"attr_time"`        // 默认 1h
	VfsCacheMode string `json:"vfs_cache_mode"`  // off / minimal / writes / full,默认 off
	AutoStart   bool   `json:"auto_start"`       // 工具启动时是否自动挂载

	// ---- 新增:缓存精细化控制 ----
	// CacheDir rclone 本地缓存目录(不填则用 rclone 默认位置)。
	// 小设备建议指向 tmpfs,如 /tmp/rclone-cache。
	CacheDir string `json:"cache_dir"`
	// VfsCacheMaxSize 单个挂载本地缓存上限,如 "10G"、"500M"。空表示不限制(不推荐)。
	VfsCacheMaxSize string `json:"vfs_cache_max_size"`
	// VfsCacheMaxAge 缓存文件最久保留时间,如 "24h"、"7d"。空表示不自动过期。
	VfsCacheMaxAge string `json:"vfs_cache_max_age"`
	// VfsCachePollInterval 缓存扫描周期,默认 1m,小设备建议 5m 或更长。
	VfsCachePollInterval string `json:"vfs_cache_poll_interval"`

	// ---- 新增:内存 / 并发控制 (防小设备 OOM) ----
	// BufferSize 每个传输的内存缓冲,默认 "32M",小设备建议 "8M" 或 "16M"。
	BufferSize string `json:"buffer_size"`
	// Transfers 并发传输数,默认 4,小设备建议 1 或 2。
	Transfers int `json:"transfers"`
	// MaxReadAhead FUSE 预读缓冲,默认 128K,摄像头场景 64K 足够。
	MaxReadAhead string `json:"max_read_ahead"`
}

// MountStatus 运行态。
type MountStatus struct {
	ID        string    `json:"id"`
	Running   bool      `json:"running"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	// 新增:最近一次探测的 FUSE 健康度("ok" 或 "stale")
	FuseState string `json:"fuse_state,omitempty"`
}

// Store 负责配置持久化 + rclone 进程生命周期。
type Store struct {
	mu        sync.RWMutex
	dataDir   string
	rcloneBin string
	configs   map[string]*MountConfig
	procs     map[string]*runningProc // 运行中的 rclone 进程
}

// runningProc 代表一个运行中的 rclone mount 进程 + 它的保护协程。
type runningProc struct {
	cmd          *exec.Cmd
	pidFile      string
	startedAt    time.Time
	lastError    string
	stopCh       chan struct{}
	monitorDone  chan struct{} // 用于等待监控 goroutine 退出
}

func NewStore(dataDir, rcloneBin string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data dir: %w", err)
	}
	s := &Store{
		dataDir:   dataDir,
		rcloneBin: rcloneBin,
		configs:   map[string]*MountConfig{},
		procs:     map[string]*runningProc{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) configPath() string      { return filepath.Join(s.dataDir, "config.json") }
func (s *Store) pidDir() string          { return filepath.Join(s.dataDir, "pids") }
func (s *Store) rcloneConfPath() string  { return filepath.Join(s.dataDir, "rclone.conf") }

// load 从 config.json 载入配置。
func (s *Store) load() error {
	b, err := os.ReadFile(s.configPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read config: %w", err)
	}
	var list []*MountConfig
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	for _, c := range list {
		s.configs[c.ID] = c
	}
	return nil
}

// save 持久化所有配置。
func (s *Store) save() error {
	s.mu.RLock()
	list := make([]*MountConfig, 0, len(s.configs))
	for _, c := range s.configs {
		cc := *c
		list = append(list, &cc)
	}
	s.mu.RUnlock()
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	// 0600 防止他人读到明文密码
	return os.WriteFile(s.configPath(), b, 0o600)
}

// List 返回所有配置副本。
func (s *Store) List() []*MountConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*MountConfig, 0, len(s.configs))
	for _, c := range s.configs {
		cc := *c
		out = append(out, &cc)
	}
	return out
}

// Get 取单个配置。
func (s *Store) Get(id string) (*MountConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.configs[id]
	if !ok {
		return nil, false
	}
	cc := *c
	return &cc, true
}

// Upsert 新增或更新配置。
// 默认值基于极限资源场景设计:玩客云 8G 盘剩 2-3G + 1G RAM + 多路摄像头。
func (s *Store) Upsert(c *MountConfig) error {
	if c.ID == "" {
		return errors.New("id required")
	}
	if c.URL == "" || c.Mountpoint == "" {
		return errors.New("url and mountpoint required")
	}
	// 目录/属性缓存:小设备用长缓存减少 FUSE 开销
	if c.DirCache == "" {
		c.DirCache = "1h"
	}
	if c.AttrTime == "" {
		c.AttrTime = "30s"
	}
	// 缓存模式:默认 writes(摄像头/Samba 写入场景必需)
	switch c.VfsCacheMode {
	case "", "off", "minimal", "writes", "full":
		if c.VfsCacheMode == "" {
			c.VfsCacheMode = "writes"
		}
	default:
		c.VfsCacheMode = "writes"
	}

	// ---- 极限资源默认值 ----
	// 缓存目录:tmpfs 内存中转,磁盘零写入(玩客云磁盘不够)
	if c.CacheDir == "" {
		c.CacheDir = "/tmp/rclone-cache"
	}
	// 缓存上限:tmpfs 吃内存,500M 是玩客云能承受的上限(1G RAM 里挤)
	if c.VfsCacheMaxSize == "" {
		c.VfsCacheMaxSize = "500M"
	}
	// 缓存最久:5 分钟上传完立即删,不留
	if c.VfsCacheMaxAge == "" {
		c.VfsCacheMaxAge = "5m"
	}
	// 扫描周期:1 分钟扫一次,赶紧清过期文件
	if c.VfsCachePollInterval == "" {
		c.VfsCachePollInterval = "1m"
	}

	// ---- 内存/并发:极小值,玩客云 1G RAM 里挤 ----
	// 并发:1 个一个串行传,省内存省 OpenList 后端压力
	if c.Transfers <= 0 {
		c.Transfers = 1
	}
	// 单传输缓冲:8M 够了,摄像头 MP4 是顺序写
	if c.BufferSize == "" {
		c.BufferSize = "8M"
	}
	// FUSE 预读:0,摄像头是顺序写,不需要预读
	if c.MaxReadAhead == "" {
		c.MaxReadAhead = "0"
	}

	s.mu.Lock()
	s.configs[c.ID] = c
	s.mu.Unlock()
	if err := s.save(); err != nil {
		return err
	}
	// 即时刷新 rclone.conf,方便用户/排错时直接看
	_ = s.regenRcloneConf()
	return nil
}

// Delete 删除配置(若正在运行,先停止)。
func (s *Store) Delete(id string) error {
	if _, ok := s.Get(id); !ok {
		return errors.New("not found")
	}
	_ = s.Stop(id) // 忽略未运行的错误
	// 先从内存中删除,再 regen rclone.conf
	s.mu.Lock()
	delete(s.configs, id)
	s.mu.Unlock()
	if err := s.regenRcloneConf(); err != nil {
		return err
	}
	return s.save()
}

// Status 返回运行态。
func (s *Store) Status(id string) MountStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := MountStatus{ID: id}
	if p, ok := s.procs[id]; ok {
		st.Running = p.cmd.ProcessState == nil || !p.cmd.ProcessState.Exited()
		st.PID = p.cmd.Process.Pid
		st.StartedAt = p.startedAt
		st.LastError = p.lastError
		// 探测 FUSE 挂载点是否健康
		st.FuseState = probeFuseHealth(p.cmd.Process.Pid)
	}
	return st
}

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
	// 另一种方式:进程是否在运行
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

// Start 启动 rclone mount 子进程。
func (s *Store) Start(id string) error {
	cfg, ok := s.Get(id)
	if !ok {
		return errors.New("not found")
	}

	s.mu.Lock()
	if p, exists := s.procs[id]; exists {
		if p.cmd.ProcessState == nil || !p.cmd.ProcessState.Exited() {
			s.mu.Unlock()
			return errors.New("already running")
		}
	}
	s.mu.Unlock()

	// 先 kill 掉这个挂载点残留的 rclone 进程 (僵尸挂载/上次 Stop 不彻底)
	s.killRcloneForMountpoint(cfg.Mountpoint)

	// 准备挂载点
	if err := os.MkdirAll(cfg.Mountpoint, 0o755); err != nil {
		return fmt.Errorf("mkdir mountpoint: %w", err)
	}

	// 多策略清理残留挂载(包括 FUSE 僵尸)
	if isMounted(cfg.Mountpoint) {
		if err := cleanupStaleMount(cfg.Mountpoint); err != nil {
			// 即使清理失败也继续:可能是已卸载但 stat 还在
			fmt.Fprintf(os.Stderr, "clean stale mount: %v\n", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 确保 rclone.conf 包含该 section
	if err := s.regenRcloneConf(); err != nil {
		return fmt.Errorf("regen rclone.conf: %w", err)
	}

	if err := os.MkdirAll(s.pidDir(), 0o755); err != nil {
		return err
	}

	remoteName := "openlist_" + cfg.ID
	vfsMode := cfg.VfsCacheMode
	if vfsMode == "" {
		vfsMode = "off"
	}
	args := []string{
		"mount", remoteName + ":", cfg.Mountpoint,
		"--config", s.rcloneConfPath(),
		"--allow-non-empty",
		"--vfs-cache-mode", vfsMode,
		"--dir-cache-time", cfg.DirCache,
		"--attr-timeout", cfg.AttrTime,
		"--buffer-size", cfg.BufferSize,
		"--transfers", fmt.Sprintf("%d", cfg.Transfers),
		"--max-read-ahead", cfg.MaxReadAhead,
		"--log-level", "INFO",
		"--log-file", filepath.Join(s.dataDir, cfg.ID+".log"),
	}
	if cfg.AllowOther {
		args = append(args, "--allow-other")
	}

	// 缓存目录(用户指定则用,否则 rclone 自己决定)
	if cfg.CacheDir != "" {
		if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
			return fmt.Errorf("mkdir cache dir %s: %w", cfg.CacheDir, err)
		}
		args = append(args, "--cache-dir", cfg.CacheDir)
	}

	// 只有用了缓存模式才加这些参数
	if vfsMode != "off" {
		if cfg.VfsCacheMaxSize != "" {
			args = append(args, "--vfs-cache-max-size", cfg.VfsCacheMaxSize)
		}
		if cfg.VfsCacheMaxAge != "" {
			args = append(args, "--vfs-cache-max-age", cfg.VfsCacheMaxAge)
		}
		if cfg.VfsCachePollInterval != "" {
			args = append(args, "--vfs-cache-poll-interval", cfg.VfsCachePollInterval)
		}
	}

	cmd := exec.Command(s.rcloneBin, args...)
	cmd.Env = os.Environ()
	// rclone 自己 daemonize 行为不友好,这里直接前台跑,Go 进程做守护
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("rclone start: %w", err)
	}

	pidFile := filepath.Join(s.pidDir(), cfg.ID+".pid")
	_ = os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0o644)

	proc := &runningProc{
		cmd:          cmd,
		pidFile:      pidFile,
		startedAt:    time.Now(),
		stopCh:       make(chan struct{}),
		monitorDone:  make(chan struct{}),
	}

	s.mu.Lock()
	s.procs[id] = proc
	s.mu.Unlock()

	// 后台 1:捕获 rclone 退出
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		if p, ok := s.procs[id]; ok {
			if err != nil {
				p.lastError = err.Error()
			}
		}
		s.mu.Unlock()
		_ = os.Remove(pidFile)
	}()

	// 后台 2:磁盘水位保护(小设备防爆盘)
	// 每 60 秒检查挂载点所在磁盘,剩余 < 5% 时给 rclone 发 SIGTERM
	go func() {
		defer close(proc.monitorDone)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-proc.stopCh:
				return
			case <-ticker.C:
				// 优先检查挂载点所在磁盘(Samba 实际写入的地方)
				// 其次检查 data 目录(tmpfs cache-dir 不检查,tmpfs 剩余=内存剩余)
				path := cfg.Mountpoint
				if path == "" {
					path = s.dataDir
				}
				pct, free := diskFreePct(path)
				if free >= 0 && pct < 10 {
					fmt.Fprintf(os.Stderr,
						"[ALERT] mount %s disk %.2f%% (free %d bytes) < 10%%, force-stopping rclone\n",
						id, pct, free)
					if proc.cmd.Process != nil {
						_ = proc.cmd.Process.Signal(syscall.SIGTERM)
						time.AfterFunc(5*time.Second, func() {
							if proc.cmd.Process != nil && !proc.cmd.ProcessState.Exited() {
								_ = proc.cmd.Process.Kill()
							}
						})
					}
					return
				}
			}
		}
	}()

	return nil
}

// Stop 终止 rclone 进程。返回错误表示确实未运行或停止失败。
func (s *Store) Stop(id string) error {
	s.mu.Lock()
	proc, ok := s.procs[id]
	s.mu.Unlock()
	if !ok {
		// 也尝试从 pidfile 恢复(可能是上次进程残留)
		pidFile := filepath.Join(s.pidDir(), id+".pid")
		if pid, _ := readPIDFile(pidFile); pid > 0 {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Signal(syscall.SIGTERM)
			}
			_ = os.Remove(pidFile)
		}
		return errors.New("not running")
	}
	mountpoint := ""
	if cfg, ok := s.Get(id); ok {
		mountpoint = cfg.Mountpoint
	}
	if proc.cmd.Process != nil {
		_ = proc.cmd.Process.Signal(syscall.SIGTERM)
	}
	// 通知监控 goroutine 退出(关闭 stopCh 触发 select 分支)
	close(proc.stopCh)
	// 用 timer 实现 5s 后强杀
	timer := time.AfterFunc(5*time.Second, func() {
		if proc.cmd.Process != nil {
			_ = proc.cmd.Process.Kill()
		}
	})
	defer timer.Stop()
	_ = proc.cmd.Wait()
	// 等监控 goroutine 也退出(最多 1s)
	select {
	case <-proc.monitorDone:
	case <-time.After(1 * time.Second):
	}
	_ = os.Remove(proc.pidFile)
	s.mu.Lock()
	delete(s.procs, id)
	s.mu.Unlock()
	// 残留挂载多策略卸载
	if mountpoint != "" {
		_ = cleanupStaleMount(mountpoint)
	}
	return nil
}

// killRcloneForMountpoint 用 pgrep+kill 干掉所有命令行里带这个挂载点的 rclone 进程。
// 用于 Start() 前清残留, 避免 FUSE 僵尸或重复挂载。
func (s *Store) killRcloneForMountpoint(mountpoint string) {
	if mountpoint == "" {
		return
	}
	// pgrep -f "rclone mount <mountpoint>" → 拿到 PID
	out, err := exec.Command("pgrep", "-f", "rclone mount .* "+mountpoint).Output()
	if err != nil || len(out) == 0 {
		return // 没残留
	}
	pids := strings.Fields(string(out))
	for _, pidStr := range pids {
		if pid, err := strconv.Atoi(pidStr); err == nil && pid > 1 {
			proc, err := os.FindProcess(pid)
			if err == nil {
				_ = proc.Signal(syscall.SIGTERM)
			}
		}
	}
	// 等 1s, 还活着就强杀
	time.Sleep(1 * time.Second)
	out2, _ := exec.Command("pgrep", "-f", "rclone mount .* "+mountpoint).Output()
	if len(out2) > 0 {
		pids2 := strings.Fields(string(out2))
		for _, pidStr := range pids2 {
			if pid, err := strconv.Atoi(pidStr); err == nil && pid > 1 {
				proc, err := os.FindProcess(pid)
				if err == nil {
					_ = proc.Kill()
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// RestoreAll 启动时根据 AutoStart 字段恢复运行态。
func (s *Store) RestoreAll() {
	s.mu.RLock()
	ids := make([]string, 0)
	for id, c := range s.configs {
		if c.AutoStart {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()
	for _, id := range ids {
		if err := s.Start(id); err != nil {
			fmt.Fprintf(os.Stderr, "restore %s: %v\n", id, err)
		}
	}
}

// StopAll 关停所有运行中挂载。
func (s *Store) StopAll() {
	s.mu.RLock()
	ids := make([]string, 0, len(s.procs))
	for id := range s.procs {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		_ = s.Stop(id)
	}
}

// regenRcloneConf 根据所有配置重新生成 rclone.conf。
func (s *Store) regenRcloneConf() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sb strings.Builder
	for _, c := range s.configs {
		secName := "openlist_" + c.ID
		sb.WriteString("[" + secName + "]\n")
		sb.WriteString("type = webdav\n")
		sb.WriteString("url = " + c.URL + "\n")
		sb.WriteString("vendor = other\n")
		sb.WriteString("user = " + c.Username + "\n")
		pass := c.Password
		if obscured, err := s.obscure(c.Password); err == nil && obscured != "" {
			pass = obscured
		}
		sb.WriteString("pass = " + pass + "\n")
		sb.WriteString("\n")
	}
	return os.WriteFile(s.rcloneConfPath(), []byte(sb.String()), 0o600)
}

// obscure 调用 rclone obscure 加密密码。失败时返回 err,调用方 fallback 用明文。
func (s *Store) obscure(plain string) (string, error) {
	cmd := exec.Command(s.rcloneBin, "obscure", plain)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

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

func readPIDFile(p string) (int, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err != nil {
		return 0, err
	}
	return pid, nil
}

// diskFreePct 返回 path 所在磁盘的剩余百分比 (0-100) 和剩余字节数。
// 出错时返回 (-1, -1)。
func diskFreePct(path string) (float64, int64) {
	if path == "" {
		return -1, -1
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return -1, -1
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bavail * uint64(stat.Bsize)
	if total == 0 {
		return -1, -1
	}
	return float64(free) / float64(total) * 100.0, int64(free)
}
