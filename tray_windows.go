//go:build windows

package main

import (
	_ "embed"
	"log"
	"strings"

	"fyne.io/systray"
)

//go:embed assets/tray.ico
var trayIcon []byte

// runTray 启动后台 HTTP 服务并创建系统托盘图标。
func runTray(srv *Server, addr string) {
	go func() {
		if err := srv.Start(); err != nil {
			log.Printf("http server: %v", err)
		}
	}()
	// systray.Run 会阻塞并独占主线程的消息循环
	systray.Run(func() { trayReady(srv, addr) }, func() {
		srv.Stop()
		log.Printf("openlist-mount exited")
	})
}

// trayReady 在托盘初始化完成后构建菜单。
func trayReady(srv *Server, addr string) {
	systray.SetIcon(trayIcon)
	systray.SetTitle("openlist-mount")
	systray.SetTooltip("openlist-mount - WebDAV 磁盘挂载")

	mOpen := systray.AddMenuItem("打开控制台", "在浏览器中打开 Web 控制台")
	systray.AddSeparator()
	mStartAll := systray.AddMenuItem("全部启动", "启动所有挂载")
	mStopAll := systray.AddMenuItem("全部停止", "停止所有挂载")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "退出 openlist-mount")

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				openBrowser(consoleURL(addr))
			case <-mStartAll.ClickedCh:
				go srv.store.StartAll()
			case <-mStopAll.ClickedCh:
				go srv.store.StopAll()
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

// consoleURL 把监听地址转换成可在浏览器打开的本地 URL。
func consoleURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr + "/"
	}
	return "http://" + addr + "/"
}