package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	// CacheDir rclone 本地缓存目录(不填则用平台默认,Linux 为 /var/cache/openlist-mount)。
	// 注意:这是磁盘目录,writes/full 模式下文件会先整份落这里再上传,要留足空间。
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
	// 新增:启动时发现的隐患(如挂载点被本地文件占用),供控制台提示
	Warning string `json:"warning,omitempty"`
	// 新增:日志里最近统计到的上传失败条数与最近一次失败原因
	UploadFailures int    `json:"upload_failures,omitempty"`
	UploadError    string `json:"upload_error,omitempty"`
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
	warn         string        // 启动时发现的隐患
	uploadFails  int           // 最近统计到的上传失败条数
	uploadErr    string        // 最近一次上传失败原因
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
	// 缓存目录:Linux 默认 /var/cache/openlist-mount(磁盘目录,非内存);
	// Windows 默认落在 %LOCALAPPDATA%,由 --vfs-cache-max-size 控上限。
	if c.CacheDir == "" {
		c.CacheDir = defaultCacheDir()
	}
	// 缓存上限:小设备给 500M,配合下面的缓存清理兜底,避免把盘写满
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
	cfg, ok := s.Get(id)
	if !ok {
		return errors.New("not found")
	}
	_ = s.Stop(id) // 忽略未运行的错误
	// 配置已删除 → 该挂载的本地缓存必然是孤儿,直接清干净,避免长期占用磁盘
	if cfg.CacheDir != "" {
		purgeMountCache(cfg.CacheDir, "openlist_"+id)
	}
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
		st.Warning = p.warn
		st.UploadFailures = p.uploadFails
		st.UploadError = p.uploadErr
		// 探测 FUSE 挂载点是否健康
		st.FuseState = probeFuseHealth(p.cmd.Process.Pid)
	}
	return st
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

	remoteName := "openlist_" + cfg.ID

	// 先 kill 掉这个 remote 残留的 rclone 进程 (僵尸挂载/上次 Stop 不彻底)
	killRcloneForMountpoint(cfg.Mountpoint, remoteName)

	// 准备挂载点(Linux 建目录;Windows 盘符则跳过)
	if err := prepareMountpoint(cfg.Mountpoint); err != nil {
		return fmt.Errorf("prepare mountpoint: %w", err)
	}

	// 隐患:挂载点当前不是挂载态、但里面已有本地文件。
	// 说明上次挂载掉过,摄像头/Samba 可能正把数据直接写进本地磁盘,把那个盘塞满。
	warnMsg := ""
	if !isMounted(cfg.Mountpoint) {
		if entries, err := os.ReadDir(cfg.Mountpoint); err == nil && len(entries) > 0 {
			warnMsg = fmt.Sprintf("挂载点 %s 不是挂载态却有 %d 个本地文件:上次挂载可能掉线,数据被写进了本地磁盘", cfg.Mountpoint, len(entries))
			fmt.Fprintf(os.Stderr, "[WARN] %s\n", warnMsg)
		}
	}

	// 清理"别的挂载"残留下来的孤儿缓存(已删除/重建过的挂载)。
	// 注意:只删非当前 remote 的,当前 remote 缓存里可能存着还没上传成功的文件。
	if cfg.CacheDir != "" {
		if freed, n := sweepOrphanCache(cfg.CacheDir, remoteName); n > 0 {
			fmt.Fprintf(os.Stderr, "start %s: swept %d orphan cache dirs (%d bytes)\n", id, n, freed)
		}
	}

	// 日志无限增长会把磁盘吃满,启动前先截断一次(运行期还有每分钟的轮转兜底)
	logFile := filepath.Join(s.dataDir, cfg.ID+".log")
	rotateLog(logFile, 16<<20)

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
		"--log-file", logFile,
	}
	// 平台特有参数(Linux: --allow-other;Windows: --volname)
	args = append(args, platformMountArgs(cfg)...)

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
		warn:         warnMsg,
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

	// 后台 2:日志轮转 + 磁盘水位保护(小设备防爆盘)
	// - 日志:rclone 的 --log-file 会无限增长(实测 6 天涨到 2.5G),只有启动时截断压不住,
	//   这里每分钟检查一次,超过 16MB 就截断。
	// - 水位:真正被写满的是 VFS 缓存目录(writes 模式先落本地)和数据目录(日志/pid)所在的盘,
	//   不是 FUSE 挂载点。任一剩余 < 10% 就停 rclone 并清缓存。
	go func() {
		defer close(proc.monitorDone)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-proc.stopCh:
				return
			case <-ticker.C:
				// 1) 日志轮转,防止把根盘写满(rclone 的 --log-file 永不轮转)
				rotateLog(logFile, 16<<20)

				// 2) 统计上传失败。上传失败时 rclone 会无限重试(实测重试到 try #833),
				//    文件永远留在缓存里、迟早把盘吃满,而用户以前完全看不到这件事。
				if n, sample := scanUploadFailures(logFile); n > 0 {
					s.mu.Lock()
					if p, ok := s.procs[id]; ok {
						p.uploadFails = n
						p.uploadErr = sample
					}
					s.mu.Unlock()
					fmt.Fprintf(os.Stderr, "[ALERT] mount %s: %d 条上传失败记录,最近一次: %s\n", id, n, sample)
				}

				// 3) 逐个候选盘查水位。这里绝不删缓存里的任何文件 ——
				//    没上传成功的文件可能是用户数据的唯一副本,删了就是真丢数据。
				candidates := make([]string, 0, 2)
				if cfg.CacheDir != "" {
					candidates = append(candidates, cfg.CacheDir)
				}
				candidates = append(candidates, s.dataDir)
				for _, path := range candidates {
					pct, free := diskFreePct(path)
					if free < 0 || pct >= 10 {
						continue
					}
					pending := 0
					if cfg.CacheDir != "" {
						pending = countCacheFiles(cfg.CacheDir, remoteName)
					}
					fmt.Fprintf(os.Stderr,
						"[ALERT] mount %s disk %s %.2f%% (free %d bytes) < 10%%, stopping rclone\n",
						id, path, pct, free)
					s.mu.Lock()
					if p, ok := s.procs[id]; ok {
						p.lastError = fmt.Sprintf(
							"磁盘 %s 仅剩 %.1f%%,已紧急停止挂载以免继续写满;缓存里还有 %d 个文件未上传(未删除)",
							path, pct, pending)
					}
					s.mu.Unlock()
					if proc.cmd.Process != nil {
						terminateProcess(proc.cmd.Process)
						time.AfterFunc(5*time.Second, func() {
							if proc.cmd.Process != nil && !proc.cmd.ProcessState.Exited() {
								forceKillProcess(proc.cmd.Process)
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
				terminateProcess(p)
			}
			_ = os.Remove(pidFile)
		}
		return errors.New("not running")
	}
	cfg, hasCfg := s.Get(id)
	mountpoint := ""
	if hasCfg {
		mountpoint = cfg.Mountpoint
	}
	if proc.cmd.Process != nil {
		terminateProcess(proc.cmd.Process)
	}
	// 通知监控 goroutine 退出(关闭 stopCh 触发 select 分支)
	close(proc.stopCh)
	// 用 timer 实现 5s 后强杀
	timer := time.AfterFunc(5*time.Second, func() {
		if proc.cmd.Process != nil {
			forceKillProcess(proc.cmd.Process)
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
	// 清理"别的挂载"残留下来的孤儿缓存;本挂载的缓存不碰(可能有没传上去的文件)
	if hasCfg && cfg.CacheDir != "" {
		if freed, n := sweepOrphanCache(cfg.CacheDir, "openlist_"+id); n > 0 {
			fmt.Fprintf(os.Stderr, "stop %s: swept %d orphan cache dirs (%d bytes)\n", id, n, freed)
		}
	}
	return nil
}

// StartAll 启动所有配置的挂载(供托盘菜单「全部启动」调用)。
func (s *Store) StartAll() {
	s.mu.RLock()
	ids := make([]string, 0, len(s.configs))
	for id := range s.configs {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		if err := s.Start(id); err != nil {
			fmt.Fprintf(os.Stderr, "start %s: %v\n", id, err)
		}
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

// ---- 本地缓存清理 ----
//
// writes/full 模式下 rclone 会把文件先整份落到 --cache-dir,传完再靠
// --vfs-cache-max-age 删掉。一旦进程被强杀、或写入速度长期高于上传速度,
// 缓存就会残留、堆积,最后把这张小盘塞满。下面几个函数是兜底清理。

// mountCachePaths 返回某挂载在 rclone 缓存目录下的数据/元数据路径。
// rclone 布局: <cache-dir>/vfs/<remote>/ 与 <cache-dir>/vfsMeta/<remote>/
func mountCachePaths(cacheDir, remoteName string) []string {
	if cacheDir == "" || remoteName == "" {
		return nil
	}
	return []string{
		filepath.Join(cacheDir, "vfs", remoteName),
		filepath.Join(cacheDir, "vfsMeta", remoteName),
	}
}

// purgeMountCache 彻底删除某挂载的本地缓存(用于删除配置 / 手动一键清空)。
// 会丢弃未上传成功的文件,只能由用户显式触发。
func purgeMountCache(cacheDir, remoteName string) {
	for _, p := range mountCachePaths(cacheDir, remoteName) {
		_ = os.RemoveAll(p)
	}
}

// sweepOrphanCache 删除缓存目录里属于"别的挂载"的残留(挂载被删除或重建后留下的),
// 返回释放字节数与删除的顶层目录数。
//
// 只看 cacheDir 下 vfs/ 和 vfsMeta/ 的顶层目录,且只删名字以 openlist_ 开头、
// 但不是当前 remote 的那些。当前 remote 的缓存一律不碰 —— 里面可能有
// "还没上传成功"的文件,那是用户数据的唯一副本(曾被按修改时间当垃圾删过)。
func sweepOrphanCache(cacheDir, currentRemote string) (int64, int) {
	if cacheDir == "" {
		return 0, 0
	}
	var freed int64
	var n int
	for _, root := range mountCacheRoots(cacheDir) {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() || name == currentRemote || !strings.HasPrefix(name, "openlist_") {
				continue
			}
			p := filepath.Join(root, name)
			freed += dirSize(p)
			if err := os.RemoveAll(p); err == nil {
				n++
			}
		}
	}
	return freed, n
}

// mountCacheRoots 返回缓存目录下 rclone 存放数据的两个顶层目录。
func mountCacheRoots(cacheDir string) []string {
	return []string{filepath.Join(cacheDir, "vfs"), filepath.Join(cacheDir, "vfsMeta")}
}

// countCacheFiles 统计某挂载缓存目录下的文件数,用于提示还有多少文件卡在缓存里。
func countCacheFiles(cacheDir, remoteName string) int {
	n := 0
	for _, root := range mountCachePaths(cacheDir, remoteName) {
		_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			n++
			return nil
		})
	}
	return n
}

// scanUploadFailures 读 rclone 日志尾部,统计"上传失败"的条数并取出最近一次失败原因。
//
// 上传失败时 rclone 会无限重试(实测重试到 try #833),文件一直卡在缓存里直到把盘吃满,
// 但控制台以前什么都不显示 —— 摄像头录像实际上没存上去都不知道。
func scanUploadFailures(logFile string) (int, string) {
	const tailBytes = 128 << 10
	f, err := os.Open(logFile)
	if err != nil {
		return 0, ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, ""
	}
	if off := fi.Size() - tailBytes; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return 0, ""
		}
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return 0, ""
	}

	var count int
	var sample string
	for _, line := range strings.Split(string(buf), "\n") {
		if !strings.Contains(line, "failed to upload") && !strings.Contains(line, "Failed to copy") {
			continue
		}
		count++
		if i := strings.Index(line, "Failed to copy: "); i >= 0 {
			sample = strings.TrimSpace(line[i+len("Failed to copy: "):])
		}
	}
	return count, sample
}

// dirSize 递归统计目录占用字节数;目录不存在返回 0。
func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, e := d.Info(); e == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// diskUsageNearest 返回 path 所在分区的容量。path 还不存在时(如尚未挂载过、
// 缓存目录没建)向上找最近的已存在父目录,保证控制台能提前显示那张盘的容量。
func diskUsageNearest(path string) (uint64, uint64, bool) {
	if path == "" {
		return 0, 0, false
	}
	p := path
	for i := 0; i < 32 && p != ""; i++ {
		if total, free, ok := diskUsage(p); ok {
			return total, free, true
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return 0, 0, false
}

// rotateLog 日志超过 maxBytes 时清空重来,避免长期运行把磁盘吃满。
func rotateLog(path string, maxBytes int64) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= maxBytes {
		return
	}
	_ = os.Truncate(path, 0)
}

// CacheSize 返回某挂载当前占用的本地缓存字节数。
func (s *Store) CacheSize(id string) int64 {
	cfg, ok := s.Get(id)
	if !ok || cfg.CacheDir == "" {
		return 0
	}
	var total int64
	for _, p := range mountCachePaths(cfg.CacheDir, "openlist_"+id) {
		total += dirSize(p)
	}
	return total
}

// CleanCache 清空某挂载的本地缓存并返回释放的字节数。
// 供"缓存盘爆满/停止后残留"时手动清空;若挂载仍在运行请先停止。
func (s *Store) CleanCache(id string) (int64, error) {
	cfg, ok := s.Get(id)
	if !ok {
		return 0, errors.New("not found")
	}
	if cfg.CacheDir == "" {
		return 0, nil
	}
	freed := s.CacheSize(id)
	purgeMountCache(cfg.CacheDir, "openlist_"+id)
	return freed, nil
}
