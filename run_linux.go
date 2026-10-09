//go:build linux

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

// runUI Linux 下以控制台守护方式运行:监听信号,收到后优雅退出。
func runUI(srv *Server, addr string, console bool) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("openlist-mount %s listening on %s", version, addr)
		if err := srv.Start(); err != nil {
			log.Printf("http server: %v", err)
			sigCh <- syscall.SIGTERM
		}
	}()

	<-sigCh
	log.Printf("shutting down...")
	srv.Stop()
	log.Printf("bye")
}