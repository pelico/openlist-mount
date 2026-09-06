package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

const version = "0.1.0"

func main() {
	addr := flag.String("addr", ":7777", "HTTP listen address")
	dataDir := flag.String("data", "/var/lib/openlist-mount", "data/config directory")
	rcloneBin := flag.String("rclone", "rclone", "rclone binary path")
	v := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *v {
		fmt.Println("openlist-mount", version)
		return
	}

	srv, err := NewServer(*addr, *dataDir, *rcloneBin)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("openlist-mount %s listening on %s (data=%s)", version, *addr, *dataDir)
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
