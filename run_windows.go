//go:build windows

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

// runUI Windows 下默认以托盘方式运行;传 --console 时退回控制台模式(便于排错)。
func runUI(srv *Server, addr string, console bool) {
	if console {
		runConsole(srv, addr)
		return
	}
	runTray(srv, addr)
}

// runConsole 控制台模式:监听 Ctrl+C / 结束信号,收到后优雅退出。
func runConsole(srv *Server, addr string) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("openlist-mount %s listening on %s", version, addr)
		if err := srv.Start(); err != nil {
			log.Printf("http server: %v", err)
			sigCh <- os.Interrupt
		}
	}()

	<-sigCh
	log.Printf("shutting down...")
	srv.Stop()
	log.Printf("bye")
}