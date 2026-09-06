package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MountConfig 描述单个 openlist 挂载项。
type MountConfig struct {
	ID         string `json:"id"`
	Name       string `json:"name"`       // 友好显示名
	URL        string `json:"url"`        // openlist WebDAV 根地址,如 http://192.168.1.2:5244/dav
	Username   string `json:"username"`
	Password   string `json:"password"`  // 明文,仅在内存与 config.json 中,本地落盘注意权限
	Mountpoint string `json:"mountpoint"` // /mnt/openlist
	AllowOther bool   `json:"allow_other"`
	DirCache   string `json:"dir_cache"`  // 默认 24h
	AttrTime   string `json:"attr_time"`  // 默认 1h
	AutoStart  bool   `json:"auto_start"` // 工具启动时是否自动挂载
}

// MountStatus 运行态。
type MountStatus struct {
	ID       string `json:"id"`
	Running  bool   `json:"running"`
	PID      int    `json:"pid"`
	StartedAt time.Time `json:"started_at,omitempty"`
	LastError string  `json:"last_error,omitempty"`
}

// Store 负责配置持久化 + rclone 进程生命周期。
type Store struct {
	mu        sync.RWMutex
	dataDir   string
	rcloneBin string
	configs   map[string]*MountConfig
	procs     map[string]*runningProc // 运行中的 rclone 进程
}

type runningProc struct {
	cmd        *exec.Cmd
	pidFile    string
	startedAt  time.Time
	lastError  string
	stopCh     chan struct{}
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

func (s *Store) configPath() string { return filepath.Join(s.dataDir, "config.json") }
func (s *Store) pidDir() string     { return filepath.Join(s.dataDir, "pids") }
func (s *Store) rcloneConfPath() string { return filepath.Join(s.dataDir, "rclone.conf") }

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
		// 复制避免外部修改
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
func (s *Store) Upsert(c *MountConfig) error {
	if c.ID == "" {
		return errors.New("id required")
	}
	if c.URL == "" || c.Mountpoint == "" {
		return errors.New("url and mountpoint required")
	}
	if c.DirCache == "" {
		c.DirCache = "24h"
	}
	if c.AttrTime == "" {
		c.AttrTime = "1h"
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

	// 准备挂载点
	if err := os.MkdirAll(cfg.Mountpoint, 0o755); err != nil {
		return fmt.Errorf("mkdir mountpoint: %w", err)
	}

	// 若已挂载(比如上次残留),先尝试 umount
	if isMounted(cfg.Mountpoint) {
		_ = syscall.Unmount(cfg.Mountpoint, 0)
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
	args := []string{
		"mount", remoteName + ":", cfg.Mountpoint,
		"--config", s.rcloneConfPath(),
		"--vfs-cache-mode", "off", // 不缓存文件内容,直接走网盘
		"--dir-cache-time", cfg.DirCache, // 目录列表缓存(避免频繁 list 被限速)
		"--attr-timeout", cfg.AttrTime, // 属性缓存
		"--buffer-size", "32M",
		"--log-level", "INFO",
		"--log-file", filepath.Join(s.dataDir, cfg.ID+".log"),
	}
	if cfg.AllowOther {
		args = append(args, "--allow-other")
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
		cmd:       cmd,
		pidFile:   pidFile,
		startedAt: time.Now(),
		stopCh:    make(chan struct{}),
	}

	s.mu.Lock()
	s.procs[id] = proc
	s.mu.Unlock()

	// 后台监控,捕获退出
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		if proc, ok := s.procs[id]; ok {
			if err != nil {
				proc.lastError = err.Error()
			}
		}
		s.mu.Unlock()
		_ = os.Remove(pidFile)
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
	// 用 timer 实现 5s 后强杀
	timer := time.AfterFunc(5*time.Second, func() {
		if proc.cmd.Process != nil {
			_ = proc.cmd.Process.Kill()
		}
	})
	defer timer.Stop()
	_ = proc.cmd.Wait()
	_ = os.Remove(proc.pidFile)
	s.mu.Lock()
	delete(s.procs, id)
	s.mu.Unlock()
	// 残留挂载强制卸载
	if mountpoint != "" && isMounted(mountpoint) {
		_ = syscall.Unmount(mountpoint, syscall.MNT_FORCE)
	}
	return nil
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
