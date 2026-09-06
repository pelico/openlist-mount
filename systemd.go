package main

import "fmt"

// renderSystemdUnit 生成 openlist-mount.service unit 内容。
func renderSystemdUnit(execPath, addr, dataDir string) string {
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
