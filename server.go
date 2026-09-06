package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed web/*
var webFS embed.FS

// Server HTTP 服务,封装 Store + systemd 工具。
type Server struct {
	addr    string
	store   *Store
	httpSrv *http.Server
}

func NewServer(addr, dataDir, rcloneBin string) (*Server, error) {
	store, err := NewStore(dataDir, rcloneBin)
	if err != nil {
		return nil, err
	}
	store.RestoreAll()
	return &Server{addr: addr, store: store}, nil
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleStatic)
	mux.HandleFunc("GET /api/mounts", s.handleList)
	mux.HandleFunc("POST /api/mounts", s.handleUpsert)
	mux.HandleFunc("DELETE /api/mounts/{id}", s.handleDelete)
	mux.HandleFunc("POST /api/mounts/{id}/start", s.handleStart)
	mux.HandleFunc("POST /api/mounts/{id}/stop", s.handleStop)
	mux.HandleFunc("GET /api/mounts/{id}/status", s.handleStatus)
	mux.HandleFunc("GET /api/mounts/{id}/log", s.handleLog)
	mux.HandleFunc("POST /api/systemd", s.handleSystemd)
	mux.HandleFunc("GET /api/rclone/check", s.handleRcloneCheck)

	s.httpSrv = &http.Server{Addr: s.addr, Handler: mux}
	return s.httpSrv.ListenAndServe()
}

func (s *Server) Stop() {
	s.store.StopAll()
	if s.httpSrv != nil {
		_ = s.httpSrv.Close()
	}
}

// ---- static files ----
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	// 根路径返回 index.html,其它去 web/ 找
	p := r.URL.Path
	if p == "/" {
		p = "/index.html"
	}
	p = strings.TrimPrefix(p, "/")
	data, err := webFS.ReadFile("web/" + p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", contentType(p))
	_, _ = w.Write(data)
}

func contentType(p string) string {
	switch {
	case strings.HasSuffix(p, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(p, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(p, ".js"):
		return "application/javascript; charset=utf-8"
	default:
		return "text/plain; charset=utf-8"
	}
}

// ---- API ----
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	cfgs := s.store.List()
	statuses := make([]map[string]interface{}, 0, len(cfgs))
	for _, c := range cfgs {
		st := s.store.Status(c.ID)
		statuses = append(statuses, map[string]interface{}{
			"config": c,
			"status": st,
		})
	}
	writeJSON(w, statuses)
}

func (s *Server) handleUpsert(w http.ResponseWriter, r *http.Request) {
	var c MountConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if c.ID == "" {
		c.ID = newID()
	}
	if err := s.store.Upsert(&c); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, c)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.Delete(id); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, map[string]string{"status": "deleted"})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.Start(id); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, s.store.Status(id))
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_ = s.store.Stop(id) // 即使未运行也返回 ok
	writeJSON(w, s.store.Status(id))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	writeJSON(w, s.store.Status(id))
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg, ok := s.store.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	b, err := readFileTail(filepath.Join(s.store.dataDir, id+".log"), 64*1024)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
	_ = cfg
}

// 注意:filepath 已在 mount.go 中引入,这里直接复用


func (s *Server) handleSystemd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExecPath string `json:"exec_path"`
		Addr     string `json:"addr"`
		DataDir  string `json:"data_dir"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ExecPath == "" {
		// 默认用当前可执行路径
		req.ExecPath = "/usr/local/bin/openlist-mount"
	}
	if req.Addr == "" {
		req.Addr = s.addr
	}
	if req.DataDir == "" {
		req.DataDir = s.store.dataDir
	}
	unit := renderSystemdUnit(req.ExecPath, req.Addr, req.DataDir)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(unit))
}

func (s *Server) handleRcloneCheck(w http.ResponseWriter, r *http.Request) {
	ver, err := rcloneVersion(s.store.rcloneBin)
	if err != nil {
		writeJSON(w, map[string]interface{}{"installed": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"installed": true, "version": ver})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- helpers ----
var idMu sync.Mutex
var idCounter uint64

func newID() string {
	idMu.Lock()
	defer idMu.Unlock()
	idCounter++
	return fmt.Sprintf("m%d", idCounter)
}

func readFileTail(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size <= int64(max) {
		return os.ReadFile(path)
	}
	if _, err := f.Seek(size-int64(max), 0); err != nil {
		return nil, err
	}
	buf := make([]byte, max)
	if _, err := f.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// rcloneVersion 取 rclone 版本号。
func rcloneVersion(bin string) (string, error) {
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "rclone v") {
			return strings.TrimPrefix(line, "rclone "), nil
		}
	}
	return strings.TrimSpace(string(out)), nil
}
