//go:build windows

// 低电量自动保护。
//
// 当 UPS 处于电池供电（OB）且电量持续低于阈值时，自动执行关机 / 睡眠 / 休眠，
// 在市电意外中断、电池即将耗尽前让电脑安全退场。
//
// 配置可由启动参数、终端交互菜单或网页设置动态修改，并持久化到
// 与可执行文件同目录的 ups-monitor.json。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"ugreen-ups/protocol"
)

// ---------------------------------------------------------------- 配置持久化

// AppConfig 是低电量保护的持久化配置。
type AppConfig struct {
	Low    int    `json:"low"`    // 电量阈值，0 表示禁用
	Action string `json:"action"` // shutdown / sleep / hibernate / none
}

// configPath 为配置文件路径（与可执行文件同目录）。
var configPath string

func init() {
	if exe, err := os.Executable(); err == nil {
		configPath = filepath.Join(filepath.Dir(exe), "ups-monitor.json")
	} else {
		configPath = "ups-monitor.json"
	}
}

func loadConfig() (AppConfig, bool) {
	var c AppConfig
	data, err := os.ReadFile(configPath)
	if err != nil {
		return c, false
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, false
	}
	return c, true
}

func saveConfig(c AppConfig) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

// ---------------------------------------------------------------- 保护状态机

// lowBatteryGuard 持有低电量触发的状态机，支持运行时动态修改。
//
// 设计要点：
//   - 仅在电池供电（OB）模式下触发；市电供电（OL）时即便电量读数偏低也不动作，
//     避免老旧电池在插电状态下被误判而关机。
//   - 需连续 need 帧（默认 3）同时满足“电池模式 + 电量<阈值”才触发，过滤瞬时抖动。
//   - 触发后本会话内只执行一次（triggered 置位），防止睡眠/休眠恢复后反复触发。
//   - 所有字段受 mu 保护，可从终端菜单与网页设置并发修改。
type lowBatteryGuard struct {
	mu        sync.Mutex
	enabled   bool
	threshold int
	action    string
	need      int
	stable    int
	triggered bool
}

func newLowBatteryGuard(low int, action string) *lowBatteryGuard {
	g := &lowBatteryGuard{need: 3, action: action, threshold: low}
	g.enabled = low > 0
	return g
}

// Configure 动态更新配置（用于启动参数、终端菜单与网页设置）。
func (g *lowBatteryGuard) Configure(enabled bool, low int, action string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.enabled = enabled && low > 0
	g.threshold = low
	g.action = action
	g.stable = 0
	if g.enabled {
		g.triggered = false // 重新武装
	}
}

// Snapshot 返回当前配置快照（线程安全）。
func (g *lowBatteryGuard) Snapshot() (bool, int, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enabled, g.threshold, g.action
}

// feed 喂入一帧样本，返回是否应当执行电源动作以及日志消息。
func (g *lowBatteryGuard) feed(s *protocol.Sample) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.enabled || g.triggered {
		return false, ""
	}
	if s.Mode != protocol.ModeBattery || s.ChargePercent >= g.threshold {
		g.stable = 0
		return false, ""
	}
	g.stable++
	if g.stable < g.need {
		return false, ""
	}
	g.triggered = true
	return true, fmt.Sprintf("电量 %d%% 低于阈值 %d%%，执行%s",
		s.ChargePercent, g.threshold, actionName(g.action))
}

func actionName(a string) string {
	switch a {
	case "shutdown":
		return "关机"
	case "sleep":
		return "睡眠"
	case "hibernate":
		return "休眠"
	case "none":
		return "仅提示(不执行)"
	default:
		return a
	}
}

// powerAction 执行指定的电源动作。
func powerAction(action string) error {
	switch action {
	case "none":
		return nil
	case "shutdown":
		return exec.Command("shutdown.exe", "/s", "/t", "0", "/f").Run()
	case "hibernate":
		return exec.Command("shutdown.exe", "/h").Run()
	case "sleep":
		if err := suspend(false); err != nil {
			// 退路：部分环境下直接调用 API 失败，改用 rundll32 触发睡眠
			if e2 := exec.Command("rundll32.exe", "powrprof.dll,SetSuspendState", "0,1,0").Run(); e2 != nil {
				return fmt.Errorf("睡眠失败: %v (rundll32: %v)", err, e2)
			}
		}
		return nil
	default:
		return fmt.Errorf("未知动作: %s", action)
	}
}

var powrprof = syscall.NewLazyDLL("powrprof.dll")
var procSetSuspendState = powrprof.NewProc("SetSuspendState")

// suspend 调用 Windows SetSuspendState。hibernate=true 为休眠，false 为睡眠。
func suspend(hibernate bool) error {
	h := uintptr(0)
	if hibernate {
		h = 1
	}
	r, _, err := procSetSuspendState.Call(h, 1, 0)
	if r == 0 {
		return fmt.Errorf("SetSuspendState 返回 0: %v", err)
	}
	return nil
}
