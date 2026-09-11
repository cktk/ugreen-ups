//go:build windows

package main

import (
	"embed"
	"os"
	"syscall"

	"github.com/getlantern/systray"
)

//go:embed assets/icon.ico
var trayIconFS embed.FS

var (
	user32                    = syscall.NewLazyDLL("user32.dll")
	procShowWindow            = user32.NewProc("ShowWindow")
	procGetConsoleWindow      = k32.NewProc("GetConsoleWindow")
	procSetConsoleCtrlHandler = k32.NewProc("SetConsoleCtrlHandler")
)

const (
	ctrlCloseEvent = 2 // CTRL_CLOSE_EVENT：用户点击控制台窗口关闭按钮
	swHide         = 0 // SW_HIDE
)

// runTray 以系统托盘模式运行：启动 Web 监控（单一数据源）与控制台查看器，
// 并在关闭控制台窗口时最小化到托盘继续运行。
func runTray() {
	gTrayMode = true

	addr := *fWeb
	if addr == "" {
		addr = ":8080"
	}

	// Web 仪表盘同时充当唯一监控源，控制台面板改为轮询查看器（避免争用 UPS 句柄）
	go runWeb(addr, *fNoBrowser)
	go runConsole(true)

	// 拦截控制台关闭事件，使“关闭窗口”等价于最小化到托盘
	installConsoleCtrlHandler()

	systray.Run(onTrayReady, onTrayExit)
}

// onTrayReady 设置托盘图标与右键菜单：打开界面 / 退出。
func onTrayReady() {
	icon, _ := trayIconFS.ReadFile("assets/icon.ico")
	systray.SetIcon(icon)
	systray.SetTitle("UGREEN UPS Monitor")
	systray.SetTooltip("UGREEN US3000 UPS 监控中")

	mOpen := systray.AddMenuItem("打开界面", "在浏览器中打开监控仪表盘")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "退出并停止监控")

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				openBrowser(gDashboardURL)
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

// onTrayExit 托盘退出时终止整个进程（Web 监控与查看器随之结束）。
func onTrayExit() {
	os.Exit(0)
}

// trayQuit 供终端命令（q）在托盘模式下退出程序。
func trayQuit() {
	systray.Quit()
}

// consoleCtrlHandler 控制台控制事件回调（供 syscall.NewCallback 使用，参数/返回须为 uintptr）。
// 仅处理“关闭窗口”事件：隐藏控制台而非退出进程，实现最小化到托盘。
// 其余事件（Ctrl+C / Ctrl+Break）交还系统默认处理。
func consoleCtrlHandler(ctrlType uintptr) uintptr {
	if ctrlType == ctrlCloseEvent {
		if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
			procShowWindow.Call(hwnd, swHide)
		}
		return 1 // 已处理，阻止默认终止
	}
	return 0
}

// installConsoleCtrlHandler 安装控制台关闭事件拦截器。
func installConsoleCtrlHandler() {
	cb := syscall.NewCallback(consoleCtrlHandler)
	procSetConsoleCtrlHandler.Call(cb, 1)
}
