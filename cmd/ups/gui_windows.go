//go:build windows

// 原生 GUI 界面（基于纯 Go 的 github.com/lxn/walk，无需 cgo / C 编译器）。
//
// 设计要点：
//   - 程序以 GUI 子系统编译（无控制台窗口），直接绘制 Go 原生窗口，不再是 cmd 控制台。
//   - 关闭窗口（X）仅隐藏到托盘，不退出进程；右键托盘图标可“打开 Web 页面”或“打开 GUI”。
//   - Web 仪表盘端口每次启动随机（:0，由系统分配），避免占用固定 8080。
//   - 单一监控源仍是 Web 监控（runWeb）；GUI 通过轮询 /api/status 取数并渲染。
package main

import (
	"embed"
	"fmt"
	"os"
	"time"

	"github.com/lxn/walk"
	hid "ugreen-ups/hid"
	"ugreen-ups/protocol"
)

//go:embed assets/icon.ico
var trayIconFS embed.FS

// guiRefs 缓存 GUI 中需要动态更新的控件引用。
type guiRefs struct {
	status *walk.Label
	dev    *walk.Label
	socBar *walk.ProgressBar
	socTxt *walk.Label
	power  map[string]*walk.Label
	batt   map[string]*walk.Label
	cells  *walk.Label
	prot   *walk.Label
	footer *walk.Label
}

// loadAppIcon 将内嵌的 ICO 写出到临时文件并加载为 walk 图标。
func loadAppIcon() (*walk.Icon, error) {
	data, err := trayIconFS.ReadFile("assets/icon.ico")
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "ups-monitor-*.ico")
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	return walk.NewIconFromFile(f.Name())
}

// newGroup 创建一个带标题的分组框，并在其中为每组键值建立一行标签。
func newGroup(parent walk.Container, title string, keys []string) (*walk.GroupBox, map[string]*walk.Label) {
	gb, err := walk.NewGroupBox(parent)
	if err != nil {
		fatal(err)
	}
	gb.SetTitle(title)
	gb.SetLayout(walk.NewVBoxLayout())
	labels := make(map[string]*walk.Label, len(keys))
	for _, k := range keys {
		row, err := walk.NewComposite(gb)
		if err != nil {
			fatal(err)
		}
		row.SetLayout(walk.NewHBoxLayout())
		kLbl, err := walk.NewLabel(row)
		if err != nil {
			fatal(err)
		}
		kLbl.SetText(k)
		vLbl, err := walk.NewLabel(row)
		if err != nil {
			fatal(err)
		}
		vLbl.SetText("—")
		labels[k] = vLbl
	}
	return gb, labels
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "致命错误:", err)
	os.Exit(1)
}

// runGUI 启动原生 GUI 窗口 + 系统托盘，并作为默认运行模式。
func runGUI() {
	// 单一监控源：Web 仪表盘（不自动开浏览器，GUI 即主界面）。
	// 端口缺省 127.0.0.1:0 —— 仅监听本机并由系统随机分配端口，避免占用固定 8080、也不对外暴露。
	addr := *fWeb
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	go runWeb(addr, true)

	icon, iconErr := loadAppIcon()
	if iconErr != nil {
		fmt.Fprintln(os.Stderr, "加载图标失败（忽略）:", iconErr)
	}

	mw, err := walk.NewMainWindow()
	if err != nil {
		fatal(err)
	}
	if icon != nil {
		_ = mw.SetIcon(icon)
	}
	mw.SetTitle("UGREEN US3000 UPS 监控")

	root, err := walk.NewComposite(mw)
	if err != nil {
		fatal(err)
	}
	root.SetLayout(walk.NewVBoxLayout())

	status, err := walk.NewLabel(root)
	if err != nil {
		fatal(err)
	}
	status.SetText("正在连接 UPS…")

	dev, err := walk.NewLabel(root)
	if err != nil {
		fatal(err)
	}
	dev.SetText("UGREEN US3000")

	socRow, err := walk.NewComposite(root)
	if err != nil {
		fatal(err)
	}
	socRow.SetLayout(walk.NewHBoxLayout())
	socLbl, err := walk.NewLabel(socRow)
	if err != nil {
		fatal(err)
	}
	socLbl.SetText("电量")
	socBar, err := walk.NewProgressBar(socRow)
	if err != nil {
		fatal(err)
	}
	socBar.SetRange(0, 100)
	socTxt, err := walk.NewLabel(socRow)
	if err != nil {
		fatal(err)
	}
	socTxt.SetText("—")

	_, power := newGroup(root, "电力",
		[]string{"输入电压", "输入电流", "输入功率", "负载"})
	_, batt := newGroup(root, "电池",
		[]string{"电池电压", "电芯合计", "充电电流", "预计续航", "均衡"})

	cells, err := walk.NewLabel(root)
	if err != nil {
		fatal(err)
	}
	cells.SetText("电芯: —")

	prot, err := walk.NewLabel(root)
	if err != nil {
		fatal(err)
	}
	prot.SetText("低电量自动保护: —")

	footer, err := walk.NewLabel(root)
	if err != nil {
		fatal(err)
	}
	footer.SetText("关闭窗口将最小化到托盘 · 右键托盘图标可“打开 Web 页面”或“打开 GUI”")

	refs := &guiRefs{
		status: status, dev: dev, socBar: socBar, socTxt: socTxt,
		power: power, batt: batt, cells: cells, prot: prot, footer: footer,
	}

	// 关闭窗口 → 最小化到托盘，不退出进程
	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		*canceled = true
		mw.Hide()
	})

	// 系统托盘：右键“打开 Web 页面” / “打开 GUI” / “退出”
	// 某些无桌面的环境（如服务器会话）无法创建托盘图标，此时降级为仅窗口+Web，不致命退出。
	ni, niErr := walk.NewNotifyIcon(mw)
	if niErr != nil {
		fmt.Fprintln(os.Stderr, "警告: 无法创建系统托盘图标（环境可能无桌面）:", niErr)
	} else {
		if icon != nil {
			_ = ni.SetIcon(icon)
		}
		_ = ni.SetToolTip("UGREEN US3000 UPS 监控中")

		aWeb := walk.NewAction()
		_ = aWeb.SetText("打开 Web 页面")
		aWeb.Triggered().Attach(func() { openBrowser(gDashboardURL) })

		aGUI := walk.NewAction()
		_ = aGUI.SetText("打开 GUI")
		aGUI.Triggered().Attach(func() { mw.Show() })

		aQuit := walk.NewAction()
		_ = aQuit.SetText("退出")
		aQuit.Triggered().Attach(func() {
			_ = ni.Dispose()
			walk.App().Exit(0)
		})

		_ = ni.ContextMenu().Actions().Add(aWeb)
		_ = ni.ContextMenu().Actions().Add(aGUI)
		_ = ni.ContextMenu().Actions().Add(aQuit)
		_ = ni.SetVisible(true)
	}

	// 定时轮询监控数据并更新界面
	go func() {
		tick := time.NewTicker(time.Duration(*fInterval) * time.Millisecond)
		defer tick.Stop()
		for range tick.C {
			s, di, evs, perr := pollStatus()
			snapshot := s
			devInfo := di
			pollErr := perr
			_ = evs
			mw.Synchronize(func() {
				refs.update(snapshot, devInfo, pollErr)
			})
		}
	}()

	mw.Show()
	mw.Run()
}

// update 将最新样本刷新到 GUI 控件（必须在 UI 线程调用，外部用 Synchronize 包住）。
func (r *guiRefs) update(s *protocol.Sample, di deviceInfo, connErr error) {
	if connErr != nil || s == nil {
		r.status.SetText("设备未连接 · 正在重试…")
		r.dev.SetText(di.Product)
		r.socTxt.SetText("—")
		r.socBar.SetValue(0)
		return
	}
	r.status.SetText(s.Status)
	r.dev.SetText(fmt.Sprintf("%s  VID:%s PID:%s  固件 %s  序列号 %s",
		di.Product, di.VendorID, di.ProductID, di.Firmware, di.Serial))
	r.socTxt.SetText(fmt.Sprintf("%d%%", s.ChargePercent))
	r.socBar.SetValue(s.ChargePercent)

	r.power["输入电压"].SetText(fmt.Sprintf("%.3f V", s.InputVoltage))
	r.power["输入电流"].SetText(fmt.Sprintf("%.3f A", s.InputCurrent))
	r.power["输入功率"].SetText(fmt.Sprintf("%.2f W", s.InputPower()))
	if s.LoadValid {
		r.power["负载"].SetText(fmt.Sprintf("%d %%", s.LoadPercent))
	} else {
		r.power["负载"].SetText("—")
	}

	r.batt["电池电压"].SetText(fmt.Sprintf("%.3f V", s.BatteryVoltage))
	r.batt["电芯合计"].SetText(fmt.Sprintf("%.3f V", s.CellSum()))
	r.batt["充电电流"].SetText(fmt.Sprintf("%.0f mA", s.ChargeCurrent))
	if rt, _ := s.RuntimeEstimate(); rt > 0 {
		r.batt["预计续航"].SetText(fmtDuration(rt))
	} else {
		r.batt["预计续航"].SetText("—")
	}
	r.batt["均衡"].SetText(s.Health())

	r.cells.SetText(fmt.Sprintf("电芯: %.3f / %.3f / %.3f / %.3f V（压差 %.0f mV）",
		s.Cells[0], s.Cells[1], s.Cells[2], s.Cells[3], s.CellDelta()))

	if gLowBattery != nil {
		en, low, act := gLowBattery.Snapshot()
		if en {
			r.prot.SetText(fmt.Sprintf("已启用（电量 < %d%% 时 %s）", low, actionName(act)))
		} else {
			r.prot.SetText(fmt.Sprintf("已禁用（电量 < %d%% 时 %s）", low, actionName(act)))
		}
	}
}

// runListGUI 以 GUI 窗口列出系统 HID 设备（GUI 子系统下无控制台，故用窗口展示）。
func runListGUI() {
	infos, err := hid.List()
	out := "系统 HID 设备:\r\n\r\n"
	if err != nil {
		out += "枚举失败: " + err.Error()
	} else {
		for _, i := range infos {
			mark := "  "
			if i.VendorID == hid.UgreenVID {
				mark = "* "
			}
			out += fmt.Sprintf("%sVID:%04X PID:%04X UsagePage:0x%04X Usage:0x%04X 输入:%d %s\r\n",
				mark, i.VendorID, i.ProductID, i.UsagePage, i.Usage, i.InputLen, i.Path)
		}
		out += "\r\n(* = 绿联 UPS 相关接口)"
	}

	mw, err := walk.NewMainWindow()
	if err != nil {
		fatal(err)
	}
	mw.SetTitle("HID 设备列表")
	root, err := walk.NewComposite(mw)
	if err != nil {
		fatal(err)
	}
	root.SetLayout(walk.NewVBoxLayout())
	te, err := walk.NewTextEdit(root)
	if err != nil {
		fatal(err)
	}
	te.SetReadOnly(true)
	te.SetText(out)
	mw.Run()
}

// pollStatus / statusResp / viewerSample 的定义见 main.go（同一 package，GUI 与控制台查看器共用）。
