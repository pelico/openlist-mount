package main

import (
	"flag"
	"fmt"
	"log"
)

const version = "0.1.0"

func main() {
	addr := flag.String("addr", ":7777", "HTTP listen address")
	dataDir := flag.String("data", defaultDataDir(), "data/config directory")
	rcloneBin := flag.String("rclone", "rclone", "rclone binary path")
	console := flag.Bool("console", false, "run in console mode (no tray) [Windows only]")
	v := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *v {
		fmt.Println("openlist-mount", version)
		return
	}

	setupLogging(*dataDir)

	srv, err := NewServer(*addr, *dataDir, *rcloneBin)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	log.Printf("openlist-mount %s (%s) starting on %s (data=%s)", version, platformName, *addr, *dataDir)
	runUI(srv, *addr, *console)
}