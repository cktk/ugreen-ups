//go:build windows

// 低电量自动保护。
//
// 当 UPS 处于电池供电（OB）且电量持续低于阈值时，自动执行关机 / 睡眠 / 休眠，
// 在市电意外中断、电池即将耗尽前让电脑安全退场。
package main

import (
	"fmt"
	"os/exec"
	"syscall"

	"ugreen-ups/protocol"
)

// lowBatteryGuard 持有低电量触发的状态机。
//
// 设计要点：
//   - 仅在电池供电（OB）模式下触发；市电供电（OL）时即便电量读数偏低也不动作，
//     避免老旧电池在插电状态下被误判而关机。
//   - 需连续 need 帧（默认 3）同时满足“电池模式 + 电量<阈值”才触发，过滤瞬时抖动。
//   - 触发后本会话内只执行一次（triggered 置位），防止睡眠/休眠恢复后反复触发。
type lowBatteryGuard struct {
	enabled   bool
	threshold int
	action    string
	need      int
	stable    int
	triggered bool
}

func newLowBatteryGuard(threshold int, action string) *lowBatteryGuard {
	if threshold <= 0 {
		return &lowBatteryGuard{enabled: false}
	}
	return &lowBatteryGuard{
		enabled:   true,
		threshold: threshold,
		action:    action,
		need:      3,
	}
}

// feed 喂入一帧样本，返回是否应当执行电源动作以及日志消息。
func (g *lowBatteryGuard) feed(s *protocol.Sample) (bool, string) {
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
	default:
		return a
	}
}

// powerAction 执行指定的电源动作。
func powerAction(action string) error {
	switch action {
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
