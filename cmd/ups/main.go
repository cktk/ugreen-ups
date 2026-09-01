//go:build windows

// ups-monitor — 绿联 UGREEN US3000 UPS 实时监控工具。
//
// 通过 USB HID 直接读取设备遥测帧，无需安装任何驱动或第三方软件。
// 支持三种用法：终端实时面板（默认）、单次/JSON 输出、Web 仪表盘。
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	hid "ugreen-ups/hid"
	"ugreen-ups/protocol"
)

// ---------------------------------------------------------------- 控制台（Windows）

var (
	k32                    = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleOutputCP = k32.NewProc("SetConsoleOutputCP")
	procGetConsoleMode     = k32.NewProc("GetConsoleMode")
	procSetConsoleMode     = k32.NewProc("SetConsoleMode")
	procGetStdHandle       = k32.NewProc("GetStdHandle")
)

const (
	stdOutputHandle                 = ^uintptr(10) // STD_OUTPUT_HANDLE == -11
	enableVirtualTerminalProcessing = 0x0004
	cpUTF8                          = 65001
)

// initConsole 启用 UTF-8 输出与 ANSI 转义序列支持。
func initConsole() {
	procSetConsoleOutputCP.Call(uintptr(cpUTF8))
	h, _, _ := procGetStdHandle.Call(uintptr(stdOutputHandle))
	if h == 0 {
		return
	}
	var mode uint32
	if ret, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); ret != 0 {
		procSetConsoleMode.Call(h, uintptr(mode|enableVirtualTerminalProcessing))
	}
}

// ---------------------------------------------------------------- ANSI 配色

const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cRed    = "\033[31m"
	cCyan   = "\033[36m"
	cBlue   = "\033[34m"
	cWhite  = "\033[97m"
)

// ---------------------------------------------------------------- 主程序

var (
	fOnce     = flag.Bool("once", false, "仅读取一帧后退出")
	fJSON     = flag.Bool("json", false, "以 JSON 格式输出")
	fWeb      = flag.String("web", "", "启动 Web 仪表盘，例如 -web :8080")
	fList     = flag.Bool("list", false, "列出系统中所有 HID 设备")
	fCSV      = flag.String("csv", "", "同时将数据追加写入 CSV 文件")
	fRaw      = flag.Bool("raw", false, "额外显示原始帧十六进制")
	fInterval = flag.Int("i", 1000, "采样间隔（毫秒）")
	fQuiet    = flag.Bool("q", false, "静默模式，不打印表头")

	fNoBrowser = flag.Bool("no-browser", false, "Web 模式下不自动打开浏览器")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"UGREEN US3000 UPS 监控工具\n\n用法:\n  %s [选项]\n\n选项:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n示例:\n"+
			"  %s                 终端实时面板\n"+
			"  %s -once -json     读取一次并以 JSON 输出\n"+
			"  %s -web :8080      启动 Web 仪表盘\n"+
			"  %s -csv ups.csv    实时面板并记录 CSV\n", os.Args[0], os.Args[0], os.Args[0], os.Args[0])
	}
	flag.Parse()

	initConsole()

	if *fList {
		listDevices()
		return
	}
	if *fWeb != "" {
		runWeb(*fWeb)
		return
	}
	if *fOnce {
		runOnce()
		return
	}
	runConsole()
}

// openUPS 连接 UPS 的私有遥测接口。
func openUPS() (*hid.Device, error) {
	return hid.OpenVendor()
}

func listDevices() {
	infos, err := hid.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "枚举失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("系统 HID 设备 (%d 个):\n\n", len(infos))
	fmt.Printf("  %-6s %-6s %-8s %-8s %-6s %s\n", "VID", "PID", "UsagePage", "Usage", "输入", "路径")
	for _, i := range infos {
		mark := "  "
		if i.VendorID == hid.UgreenVID {
			mark = "* "
		}
		fmt.Printf("%s%04X   %04X   0x%04X     0x%04X    %-6d %s\n",
			mark, i.VendorID, i.ProductID, i.UsagePage, i.Usage, i.InputLen, i.Path)
	}
	fmt.Println("\n(* = 绿联 UPS 相关接口)")
}

func runOnce() {
	dev, err := openUPS()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s连接 UPS 失败: %v%s\n", cRed, err, cReset)
		os.Exit(1)
	}
	defer dev.Close()

	raw, err := dev.Read(5000)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s读取失败: %v%s\n", cRed, err, cReset)
		os.Exit(1)
	}
	s, err := protocol.Parse(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s解析失败: %v%s\n", cRed, err, cReset)
		os.Exit(1)
	}
	// 补充设备信息
	if f, err := dev.GetFeature(0x06, 0); err == nil && len(f) >= 6 {
		soc := int(f[1])
		if soc > 0 && soc <= 100 {
			s.ChargePercent = soc // BMS 上报值优先
		}
	}
	printJSON(dev, s)
}

type jsonOut struct {
	Device deviceInfo `json:"device"`
	Sample sampleJSON `json:"sample"`
}

type deviceInfo struct {
	Manufacturer string `json:"manufacturer"`
	Product      string `json:"product"`
	Serial       string `json:"serial"`
	VendorID     string `json:"vendor_id"`
	ProductID    string `json:"product_id"`
	Firmware     string `json:"firmware"`
}

type sampleJSON struct {
	Time           string    `json:"time"`
	Status         string    `json:"status"`
	StatusCode     string    `json:"status_code"`
	Online         bool      `json:"online"`
	Charging       bool      `json:"charging"`
	InputVoltage   float64   `json:"input_voltage_v"`
	OutputVoltage  float64   `json:"output_voltage_v"`
	InputCurrent   float64   `json:"input_current_a"`
	InputPower     float64   `json:"input_power_w"`
	BatteryVoltage float64   `json:"battery_voltage_v"`
	ChargePercent  int       `json:"battery_charge_percent"`
	ChargeCurrent  float64   `json:"charge_current_ma"`
	LoadPercent    int       `json:"load_percent"`
	LoadValid      bool      `json:"load_valid"`
	RuntimeSec     int       `json:"runtime_seconds"`
	RuntimeSource  string    `json:"runtime_source"`
	Cells          []float64 `json:"cell_voltages_v"`
	CellDeltaMv    float64   `json:"cell_delta_mv"`
	Health         string    `json:"battery_health"`
	RawHex         string    `json:"raw_hex,omitempty"`
}

func newSampleJSON(s *protocol.Sample, withRaw bool) sampleJSON {
	rt, src := s.RuntimeEstimate()
	cells := make([]float64, len(s.Cells))
	for i, v := range s.Cells {
		cells[i] = v
	}
	j := sampleJSON{
		Time:           s.Time.Format(time.RFC3339),
		Status:         s.Status,
		StatusCode:     s.StatusShort(),
		Online:         s.Online,
		Charging:       s.Charging,
		InputVoltage:   r3(s.InputVoltage),
		OutputVoltage:  r3(s.OutputVoltage),
		InputCurrent:   r3(s.InputCurrent),
		InputPower:     r2(s.InputPower()),
		BatteryVoltage: r3(s.BatteryVoltage),
		ChargePercent:  s.ChargePercent,
		ChargeCurrent:  s.ChargeCurrent,
		LoadPercent:    s.LoadPercent,
		LoadValid:      s.LoadValid,
		RuntimeSec:     rt,
		RuntimeSource:  src,
		Cells:          cells,
		CellDeltaMv:    r2(s.CellDelta()),
		Health:         s.Health(),
	}
	if withRaw {
		j.RawHex = s.RawHex()
	}
	return j
}

func printJSON(dev *hid.Device, s *protocol.Sample) {
	out := jsonOut{
		Device: deviceInfo{
			Manufacturer: dev.Manufacturer,
			Product:      dev.Product,
			Serial:       dev.Serial,
			VendorID:     fmt.Sprintf("%04X", dev.VendorID),
			ProductID:    fmt.Sprintf("%04X", dev.ProductID),
			Firmware:     fmt.Sprintf("%d.%02d", dev.Version>>8, dev.Version&0xFF),
		},
		Sample: newSampleJSON(s, *fRaw),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(out)
}

func r3(v float64) float64 { return float64(int(v*1000+0.5)) / 1000 }
func r2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }

// ---------------------------------------------------------------- 终端实时面板

func runConsole() {
	dev, err := openUPS()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s连接 UPS 失败: %v%s\n", cRed, err, cReset)
		fmt.Fprintf(os.Stderr, "请确认 UPS 已通过 USB 连接，或运行 -list 查看设备。\n")
		os.Exit(1)
	}
	defer dev.Close()

	var cw *csv.Writer
	var cf *os.File
	if *fCSV != "" {
		cf, err = os.OpenFile(*fCSV, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法打开 CSV: %v\n", err)
		} else {
			defer cf.Close()
			cw = csv.NewWriter(cf)
			defer cw.Flush()
			// 若文件为空则写入表头
			if st, e := cf.Stat(); e == nil && st.Size() == 0 {
				cw.Write([]string{"时间", "状态", "输入电压V", "输出/输入电流A", "功率W",
					"电池电压V", "电量%", "充电电流mA", "负载%", "电芯1V", "电芯2V",
					"电芯3V", "电芯4V", "压差mV", "剩余秒"})
			}
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	var (
		frames   int
		lastMode = protocol.ModeUnknown
		stable   int
		pending  = protocol.ModeUnknown
		events   []string
		rate     time.Time
		rateN    int
		fps      float64
	)
	rate = time.Now()

	tick := time.NewTicker(time.Duration(*fInterval) * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-sig:
			fmt.Printf("\n%s已停止监控。%s\n", cDim, cReset)
			return
		case <-tick.C:
		}

		raw, err := dev.Read(3000)
		if err != nil {
			// 设备可能断开，尝试重连
			dev.Close()
			time.Sleep(500 * time.Millisecond)
			if nd, e := openUPS(); e == nil {
				dev = nd
				events = append(events, fmt.Sprintf("%s 设备已重新连接", time.Now().Format("15:04:05")))
			} else {
				events = append(events, fmt.Sprintf("%s 读取失败: %v", time.Now().Format("15:04:05"), err))
			}
			if len(events) > 6 {
				events = events[len(events)-6:]
			}
			continue
		}

		s, err := protocol.Parse(raw)
		if err != nil {
			events = append(events, fmt.Sprintf("%s 解析失败: %v", time.Now().Format("15:04:05"), err))
			continue
		}
		frames++
		rateN++
		if elapsed := time.Since(rate).Seconds(); elapsed >= 5 {
			fps = float64(rateN) / elapsed
			rate, rateN = time.Now(), 0
		}

		// 状态去抖：连续 3 次一致才确认切换
		if s.Mode != lastMode {
			if s.Mode == pending {
				stable++
				if stable >= 3 {
					events = append(events, fmt.Sprintf("%s 状态切换: %s",
						time.Now().Format("15:04:05"), s.Status))
					lastMode = s.Mode
					stable = 0
					pending = protocol.ModeUnknown
				}
			} else {
				pending = s.Mode
				stable = 1
			}
		} else {
			pending = protocol.ModeUnknown
			stable = 0
		}
		if len(events) > 6 {
			events = events[len(events)-6:]
		}

		if cw != nil {
			rt, _ := s.RuntimeEstimate()
			cw.Write([]string{
				s.Time.Format("2006-01-02 15:04:05"), s.StatusShort(),
				fmt.Sprintf("%.3f", s.InputVoltage), fmt.Sprintf("%.3f", s.InputCurrent),
				fmt.Sprintf("%.2f", s.InputPower()), fmt.Sprintf("%.3f", s.BatteryVoltage),
				fmt.Sprintf("%d", s.ChargePercent), fmt.Sprintf("%.0f", s.ChargeCurrent),
				fmt.Sprintf("%d", s.LoadPercent),
				fmt.Sprintf("%.3f", s.Cells[0]), fmt.Sprintf("%.3f", s.Cells[1]),
				fmt.Sprintf("%.3f", s.Cells[2]), fmt.Sprintf("%.3f", s.Cells[3]),
				fmt.Sprintf("%.1f", s.CellDelta()), fmt.Sprintf("%d", rt),
			})
			cw.Flush()
		}

		if *fJSON {
			b, _ := json.Marshal(newSampleJSON(s, *fRaw))
			fmt.Println(string(b))
			continue
		}

		render(dev, s, frames, fps, events)
	}
}

// ---------------------------------------------------------------- 渲染

func render(dev *hid.Device, s *protocol.Sample, frames int, fps float64, events []string) {
	var b strings.Builder
	b.WriteString("\033[H\033[2J") // 归位并清屏

	// 标题
	b.WriteString(cBold + cCyan)
	b.WriteString("  UGREEN US3000 UPS 实时监控")
	b.WriteString(cReset + cDim)
	fmt.Fprintf(&b, "                      %s %s  固件 %d.%02d  序列号 %s\n",
		dev.Product, fmt.Sprintf("VID:%04X PID:%04X", dev.VendorID, dev.ProductID),
		dev.Version>>8, dev.Version&0xFF, dev.Serial)
	b.WriteString(cReset)
	b.WriteString(cDim + "  " + strings.Repeat("─", 76) + "\n" + cReset)

	// 状态行
	statusColor := cGreen
	statusIcon := "●"
	if s.Mode == protocol.ModeBattery {
		statusColor = cRed
		statusIcon = "▲"
	} else if s.Charging {
		statusColor = cYellow
		statusIcon = "◆"
	} else if s.Mode == protocol.ModeUnknown {
		statusColor = cDim
		statusIcon = "?"
	}
	fmt.Fprintf(&b, "  %s%s %s%s   %s[%s]%s\n",
		statusColor, statusIcon, pad(s.Status, 34), cReset, cDim, s.StatusShort(), cReset)

	fmt.Fprintf(&b, "  %s采样时间%s %s    %s第 %d 帧%s",
		cDim, cReset, s.Time.Format("2006-01-02 15:04:05"), cDim, frames, cReset)
	if fps > 0 {
		fmt.Fprintf(&b, "    %s%.1f 帧/秒%s", cDim, fps, cReset)
	}
	b.WriteString("\n\n")

	// 电力
	b.WriteString(section("电力"))
	loadStr := "—"
	if s.LoadValid {
		loadStr = fmt.Sprintf("%d %%", s.LoadPercent)
	}
	outStr := fmt.Sprintf("%.3f V", s.OutputVoltage)
	if s.Mode != protocol.ModeBattery {
		outStr = cDim + "仅电池供电时上报" + cReset
	}
	kv2(&b, "输入电压", fmt.Sprintf("%.3f V", s.InputVoltage),
		"输入电流", fmt.Sprintf("%.3f A", s.InputCurrent))
	kv2(&b, "输入功率", fmt.Sprintf("%.2f W", s.InputPower()), "负载", loadStr)
	kv2(&b, "输出电压", outStr, "副测点电压", fmt.Sprintf("%.3f V", s.InputVoltage1))

	// 电池
	b.WriteString(section("电池"))
	bar := barString(s.ChargePercent, 100, 20)
	fmt.Fprintf(&b, "  %s%s%s %s%d%%%s  %s%s%s\n",
		cDim, pad("电量", 12), cReset, cBold+chargeColor(s.ChargePercent), s.ChargePercent, cReset,
		chargeColor(s.ChargePercent), bar, cReset)
	kv2(&b, "电池电压", fmt.Sprintf("%.3f V", s.BatteryVoltage),
		"电芯合计", fmt.Sprintf("%.3f V", s.CellSum()))
	chgStr := fmt.Sprintf("%.0f mA", s.ChargeCurrent)
	if !s.Charging && s.ChargeCurrent == 0 {
		chgStr = cDim + "0 mA（未充电）" + cReset
	}
	kv2(&b, "充电电流", chgStr, "标称容量", "43.0 Wh")
	rt, _ := s.RuntimeEstimate()
	rtStr := cDim + "市电正常" + cReset
	if rt > 0 {
		rtStr = fmtDuration(rt)
		if s.Mode == protocol.ModeBattery {
			rtStr += "（设备估算）"
		}
	}
	kv2(&b, "预计续航", rtStr, "电池类型", "4S 锂离子")

	// 电芯
	b.WriteString(section("电芯电压 (4S)"))
	for i, v := range s.Cells {
		col := cGreen
		if v < 3.3 {
			col = cRed
		} else if v < 3.6 || v > 4.25 {
			col = cYellow
		}
		fmt.Fprintf(&b, "  %s%s%s %s%.3f V%s  %s%s%s\n",
			cDim, pad(fmt.Sprintf("电芯 %d", i+1), 12), cReset,
			col, v, cReset, col, barString(int(v*1000), 4200, 16), cReset)
	}
	delta := s.CellDelta()
	dcol := cGreen
	if delta > 150 {
		dcol = cRed
	} else if delta > 80 {
		dcol = cYellow
	}
	kv2(&b, "压差", fmt.Sprintf("%s%.0f mV%s", dcol, delta, cReset),
		"均衡评估", s.Health())

	// 事件
	if len(events) > 0 {
		b.WriteString(section("事件"))
		for _, e := range events {
			fmt.Fprintf(&b, "  %s· %s%s\n", cDim, e, cReset)
		}
	}

	if *fRaw {
		b.WriteString(section("原始帧"))
		fmt.Fprintf(&b, "  %s%s%s\n", cDim, s.RawHex(), cReset)
	}

	b.WriteString("\n  " + cDim + "按 Ctrl+C 退出" + cReset + "\n")
	fmt.Print(b.String())
}

func section(name string) string {
	fill := 76 - displayWidth(name)
	if fill < 4 {
		fill = 4
	}
	return fmt.Sprintf("\n  %s%s%s  %s\n", cBold+cBlue, name, cReset,
		cDim+strings.Repeat("─", fill)+cReset)
}

// kv2 输出一行两列：键 + 值 + 键 + 值，按实际显示宽度对齐。
func kv2(b *strings.Builder, k1, v1, k2, v2 string) {
	const valueWidth = 22
	b.WriteString("  " + cDim + pad(k1, 12) + cReset + v1)
	if w := displayWidth(v1); w < valueWidth {
		b.WriteString(strings.Repeat(" ", valueWidth-w))
	}
	b.WriteString(cDim + pad(k2, 12) + cReset + v2 + "\n")
}

// displayWidth 计算字符串的终端显示宽度：
// 忽略 ANSI 转义序列（不占列），中日韩等宽字符按 2 列计。
func displayWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		// CSI 序列：ESC [ ... 终止符(0x40–0x7E)
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7E) {
				j++
			}
			if j < len(s) {
				j++ // 跳过终止符
			}
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r > 0x2E80 {
			w += 2 // CJK 全角
		} else {
			w++
		}
		i += size
	}
	return w
}

// pad 按显示宽度在右侧补空格
func pad(s string, width int) string {
	if w := displayWidth(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

func barString(v, max, width int) string {
	if max <= 0 {
		max = 100
	}
	filled := v * width / max
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("█", filled) + cDim + strings.Repeat("░", width-filled) + cReset
}

func chargeColor(p int) string {
	switch {
	case p >= 60:
		return cGreen
	case p >= 30:
		return cYellow
	default:
		return cRed
	}
}

func fmtDuration(sec int) string {
	if sec < 0 {
		return "—"
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	if h > 0 {
		return fmt.Sprintf("%d 小时 %d 分", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%d 分 %d 秒", m, s)
	}
	return fmt.Sprintf("%d 秒", s)
}
