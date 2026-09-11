//go:build windows

// 原生 GUI 界面（基于纯 Go 的 github.com/lxn/walk，无需 cgo / C 编译器）。
//
// 设计要点：
//   - 程序以 GUI 子系统编译（无控制台窗口），直接绘制 Go 原生窗口，不再是 cmd 控制台。
//   - 顶部为 Canvas 自绘的渐变横幅（标题 / 状态 / 电量），其余为原生分组控件。
//   - 关闭窗口（X）仅隐藏到托盘，不退出进程；右键托盘图标可“打开 Web 页面”或“打开 GUI”。
//   - Web 仪表盘端口每次启动随机（127.0.0.1:0，由系统分配），避免占用固定 8080。
//   - 单一监控源仍是 Web 监控（runWeb）；GUI 通过 /api/status 与 /api/config 读写。
//   - GUI 内可直接配置“低电量自动保护”（启用 / 阈值 / 动作 / 开机自启），保存即生效并持久化。
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	hid "ugreen-ups/hid"
	"ugreen-ups/protocol"
)

//go:embed assets/icon.ico
var trayIconFS embed.FS

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
	logFile         *os.File
	logPath         string // 当前日志文件路径（滚动时用）
	logSize         int64  // 当前日志文件已写入字节数
)

// ---------------------------------------------------------------- 日志 / 崩溃提示

// 程序设计为可连续运行数年，日志必须自己管住，否则会无限增长把磁盘写满。
// 策略：单文件超过 logMaxBytes 就滚动，历史文件编号 .1/.2/.3，最多保留 logKeepFiles 份。
// 日志只记录事件与心跳（不逐帧落盘）：心跳每 logBeatInterval 一条，
// 按此频率每份文件约覆盖 2 个月，四份加起来足够回溯大半年的运行情况。
const (
	logMaxBytes  = 2 << 20 // 2 MiB
	logKeepFiles = 3

	logBeatInterval = 5 * time.Minute // 心跳日志间隔
)

// logInit 打开日志文件（与可执行文件同目录，失败则退回临时目录）。
// GUI 子系统无控制台，所有诊断信息走文件 + 弹窗。
func logInit() {
	path := "ups-monitor.log"
	if exe, err := os.Executable(); err == nil {
		path = filepath.Join(filepath.Dir(exe), "ups-monitor.log")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		f, err = os.CreateTemp("", "ups-monitor-*.log")
		if err != nil {
			return
		}
		path = f.Name()
	}
	logFile, logPath = f, path
	if st, err := f.Stat(); err == nil {
		logSize = st.Size()
	}
	if logSize >= logMaxBytes {
		// 上次退出时就已写满（例如长时间断连刷屏），立刻滚一次再继续
		rotateLog("启动时发现日志已超限")
	}
}

func logf(format string, a ...any) {
	if logFile == nil {
		return
	}
	n, err := fmt.Fprintf(logFile,
		time.Now().Format("2006-01-02 15:04:05.000 ")+format+"\n", a...)
	if err != nil {
		return
	}
	_ = logFile.Sync()
	logSize += int64(n)
	if logSize >= logMaxBytes {
		rotateLog(fmt.Sprintf("达到 %d MiB 上限", logMaxBytes>>20))
	}
}

// rotateLog 归档当前日志并新建空文件：log → log.1 → log.2 → … 超出 logKeepFiles 的丢弃。
// 只保留固定份数，因此磁盘占用有上限，长期运行不会撑爆。
func rotateLog(reason string) {
	if logFile == nil || logPath == "" {
		return
	}
	_ = logFile.Close()
	logFile = nil

	// 从最旧一份开始往后挪，避免相互覆盖
	for i := logKeepFiles - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", logPath, i), fmt.Sprintf("%s.%d", logPath, i+1))
	}
	_ = os.Rename(logPath, logPath+".1")

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	logFile = f
	logSize = 0
	n, _ := fmt.Fprintf(f, "%s=== 日志已滚动（%s，保留 %d 份历史 .1-.%d）===\n",
		time.Now().Format("2006-01-02 15:04:05.000 "), reason, logKeepFiles, logKeepFiles)
	_ = f.Sync()
	logSize += int64(n)
}

// msgBox 用原生 Win32 弹窗显示错误（不依赖 walk，即使 GUI 初始化失败也能提示用户）。
func msgBox(title, text string) {
	t, err1 := syscall.UTF16PtrFromString(title)
	x, err2 := syscall.UTF16PtrFromString(text)
	if err1 != nil || err2 != nil {
		return
	}
	const mbIconError = 0x00000010
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(t)), mbIconError)
}

// guard 包裹 goroutine，捕获 panic 并记录 + 提示，避免静默崩溃。
func guard(name string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			logf("PANIC in %s: %v\n%s", name, r, debug.Stack())
			msgBox("UGREEN UPS Monitor 崩溃", fmt.Sprintf("%s: %v", name, r))
			os.Exit(1)
		}
	}()
	f()
}

func fatal(err error) {
	logf("FATAL: %v", err)
	msgBox("UGREEN UPS Monitor 启动失败", fmt.Sprint(err))
	os.Exit(1)
}

// ---------------------------------------------------------------- 字体

type uiFonts struct {
	normal, bold, title, big          *walk.Font
	banner, bannerSub, socHuge, small *walk.Font
}

func mkFont(family string, size int, style walk.FontStyle) *walk.Font {
	f, err := walk.NewFont(family, size, style)
	if err != nil {
		return nil
	}
	return f
}

// loadFonts 优先使用中文界面字体，失败则退回 Segoe UI；全部失败时各字段为 nil，
// 绘制与 setFont 处均有 nil 保护，界面仍可用（仅无自定义字号）。
func loadFonts() uiFonts {
	for _, fam := range []string{"Microsoft YaHei UI", "Microsoft YaHei", "Segoe UI", "Tahoma"} {
		if f := mkFont(fam, 9, 0); f != nil {
			return uiFonts{
				normal:    f,
				bold:      mkFont(fam, 9, walk.FontBold),
				title:     mkFont(fam, 12, walk.FontBold),
				big:       mkFont(fam, 15, walk.FontBold),
				banner:    mkFont(fam, 14, walk.FontBold),
				bannerSub: mkFont(fam, 8, 0),
				socHuge:   mkFont(fam, 24, walk.FontBold),
				small:     mkFont(fam, 8, 0),
			}
		}
	}
	return uiFonts{}
}

// nz 返回非空字体；全部创建失败时返回 nil，调用方需自行判空。
func nz(fs ...*walk.Font) *walk.Font {
	for _, f := range fs {
		if f != nil {
			return f
		}
	}
	return nil
}

// ---------------------------------------------------------------- 配色

var (
	colBanner1 = walk.RGB(0x15, 0x53, 0xB4) // 横幅渐变起始（深蓝）
	colBanner2 = walk.RGB(0x2E, 0x9B, 0xE8) // 横幅渐变结束（亮蓝）
	colWhite   = walk.RGB(0xFF, 0xFF, 0xFF)
	colSubTxt  = walk.RGB(0xE2, 0xEF, 0xFF)
	colDimTxt  = walk.RGB(0xBB, 0xD6, 0xF2)
)

// ---------------------------------------------------------------- 布局常量与控件助手

const (
	keyColWidth = 88  // 表单“键”列宽度
	valColWidth = 126 // 表单“值”列宽度
	rootMargin  = 12  // 窗口内边距
	rootSpacing = 10  // 顶层控件间距
	groupPad    = 10  // 分组框左右内边距
)

func setF(w interface{ SetFont(*walk.Font) }, f *walk.Font) {
	if f != nil {
		w.SetFont(f)
	}
}

// lbl 创建一个标签。
func lbl(parent walk.Container, text string, f *walk.Font) *walk.Label {
	l, err := walk.NewLabel(parent)
	if err != nil {
		fatal(err)
	}
	_ = l.SetText(text)
	setF(l, f)
	return l
}

// row 创建一行水平容器（内部间距 8）。
// 调用方放完控件后需调用 endRow，否则该行不会被横向撑满。
func row(parent walk.Container) *walk.Composite {
	c, err := walk.NewComposite(parent)
	if err != nil {
		fatal(err)
	}
	lay := walk.NewHBoxLayout()
	_ = lay.SetSpacing(8)
	_ = lay.SetMargins(walk.Margins{})
	_ = c.SetLayout(lay)
	return c
}

// endRow 在行的末尾追加一个弹性占位（必须最后创建，顺序决定位置）。
//
// 作用有两个：
//  1. 让该行获得 GrowableHorz 标志，从而使外层的纵向布局把它横向撑满 ——
//     否则 walk 会把不“可增长”的子项按默认方式居中摆放，导致各分组宽度参差不齐。
//  2. 多余宽度全部被行尾的占位吸收，控件保持各自宽度并整体左对齐。
func endRow(r walk.Container) {
	if _, err := walk.NewHSpacer(r); err != nil {
		fatal(err)
	}
}

// leftAlignButton 把复选框/单选按钮的文字改为左对齐。
// Windows 的 BS_AUTOCHECKBOX / BS_AUTORADIOBUTTON 在多出空白时会居中绘制文字，
// walk 未暴露该样式位，直接改样式。
func leftAlignButton(b interface {
	Handle() win.HWND
	Invalidate() error
}) {
	h := b.Handle()
	if h == 0 {
		return
	}
	style := win.GetWindowLong(h, win.GWL_STYLE)
	win.SetWindowLong(h, win.GWL_STYLE, style|win.BS_LEFT)
	win.SetWindowPos(h, 0, 0, 0, 0, 0,
		win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_FRAMECHANGED)
	_ = b.Invalidate()
}

// fixedWidth 把控件宽度固定为 96dpi 下的 w（walk 的 SetMinMaxSize 中 max 为 0 表示不限制）。
func fixedWidth(w interface {
	SetMinMaxSize(min, max walk.Size) error
}, w96 int) {
	_ = w.SetMinMaxSize(walk.Size{Width: w96}, walk.Size{Width: w96})
}

// group 创建一个垂直布局的分组框（带左右内边距，避免控件贴着边框）。
func group(parent walk.Container, title string, f uiFonts) *walk.GroupBox {
	gb, err := walk.NewGroupBox(parent)
	if err != nil {
		fatal(err)
	}
	_ = gb.SetTitle(title)
	setF(gb, nz(f.bold, f.normal))
	lay := walk.NewVBoxLayout()
	_ = lay.SetSpacing(6)
	_ = lay.SetMargins(walk.Margins{HNear: groupPad, VNear: 6, HFar: groupPad, VFar: 8})
	_ = gb.SetLayout(lay)
	return gb
}

// kv 在 parent 中创建一行“键 值 [键 值]”，键列固定宽度以保证竖直对齐。
// 返回各值标签；k2 为空时只创建一列（第二个返回值为 nil）。
func kv(parent walk.Container, k1, k2 string, f uiFonts) (*walk.Label, *walk.Label) {
	r := row(parent)
	var l1 *walk.Label
	l1 = lbl(r, k1, nz(f.bold, f.normal))
	_ = l1.SetMinMaxSize(walk.Size{Width: keyColWidth}, walk.Size{})
	v1 := lbl(r, "—", f.normal)
	_ = v1.SetMinMaxSize(walk.Size{Width: valColWidth}, walk.Size{})
	if k2 == "" {
		endRow(r)
		return v1, nil
	}
	l2 := lbl(r, k2, nz(f.bold, f.normal))
	_ = l2.SetMinMaxSize(walk.Size{Width: keyColWidth}, walk.Size{})
	v2 := lbl(r, "—", f.normal)
	endRow(r)
	return v1, v2
}

// ---------------------------------------------------------------- 顶部渐变横幅

// bannerWidget 顶部自绘横幅：左侧标题/状态/接口信息，右侧大号电量。
// 用 Canvas 绘制而非默认控件，避免纯系统控件的观感过于平淡。
type bannerWidget struct {
	cw     *walk.CustomWidget
	title  string
	status string
	detail string
	soc    string
	hint   string
}

// 横幅内各元素在 96dpi 下的逻辑坐标与高度。
const (
	bannerHeight = 88
	bannerRightW = 132
)

func newBanner(parent walk.Container, f uiFonts) *bannerWidget {
	b := &bannerWidget{
		title:  "UGREEN UPS Monitor",
		status: "正在连接 UPS…",
		detail: "USB HID",
		soc:    "--",
		hint:   "剩余电量",
	}
	cw, err := walk.NewCustomWidget(parent, 0, func(cv *walk.Canvas, _ walk.Rectangle) error {
		b.paint(cv, f)
		return nil
	})
	if err != nil {
		fatal(err)
	}
	_ = cw.SetMinMaxSize(walk.Size{Width: 120, Height: bannerHeight}, walk.Size{Height: bannerHeight})
	b.cw = cw
	return b
}

// set 更新横幅文案并重绘（必须在 UI 线程调用）。
func (b *bannerWidget) set(title, status, detail, soc, hint string) {
	b.title, b.status, b.detail, b.soc, b.hint = title, status, detail, soc, hint
	if b.cw != nil {
		b.cw.Invalidate()
	}
}

// drawText 安全绘制：walk 的 Canvas 在字体为 nil 时会解引用空指针，必须先判空。
func drawText(cv *walk.Canvas, text string, f *walk.Font, col walk.Color,
	r walk.Rectangle, flags walk.DrawTextFormat) {
	if f == nil || text == "" {
		return
	}
	_ = cv.DrawTextPixels(text, f, col, r, flags)
}

func (b *bannerWidget) paint(cv *walk.Canvas, f uiFonts) {
	// 注意：不能用 cv.BoundsPixels()——绘制时的 HDC 是窗口 DC，
	// GetDeviceCaps(HORZRES) 返回的是整块屏幕的尺寸，会导致右对齐元素被画到窗口外。
	bounds := b.cw.ClientBoundsPixels()
	if bounds.Width < 60 || bounds.Height < 40 {
		return
	}
	// 渐变底色
	_ = cv.GradientFillRectanglePixels(colBanner1, colBanner2, walk.Horizontal, bounds)

	// 坐标系换算：布局值按 96dpi 计，实际绘制用像素，需按 DPI 放大。
	dpi := cv.DPI()
	sc := func(v int) int { return v * dpi / 96 }

	rightW := sc(bannerRightW)
	leftX := sc(16)
	leftW := bounds.Width - rightW - leftX - sc(12)
	if leftW < sc(80) {
		leftW = sc(80)
	}
	rightX := bounds.Width - rightW - sc(12)

	const fl = walk.TextSingleLine | walk.TextVCenter | walk.TextLeft
	const fr = walk.TextSingleLine | walk.TextVCenter | walk.TextRight

	drawText(cv, b.title, nz(f.banner, f.title, f.bold), colWhite,
		walk.Rectangle{X: leftX, Y: sc(8), Width: leftW, Height: sc(26)}, fl)
	drawText(cv, b.status, nz(f.normal), colSubTxt,
		walk.Rectangle{X: leftX, Y: sc(37), Width: leftW, Height: sc(20)}, fl)
	drawText(cv, b.detail, nz(f.bannerSub, f.small, f.normal), colDimTxt,
		walk.Rectangle{X: leftX, Y: sc(58), Width: leftW, Height: sc(20)}, fl)

	drawText(cv, b.soc, nz(f.socHuge, f.big, f.bold), colWhite,
		walk.Rectangle{X: rightX, Y: sc(6), Width: rightW, Height: sc(46)}, fr)
	drawText(cv, b.hint, nz(f.bannerSub, f.small, f.normal), colDimTxt,
		walk.Rectangle{X: rightX, Y: sc(54), Width: rightW, Height: sc(20)}, fr)
}

// ---------------------------------------------------------------- 电芯电压图表

// 图表在 96dpi 下的分段高度（自上而下）：
//
//	0            .. barValueH              柱顶电压数值标注
//	barValueH    .. barBottom              柱体
//	barBottom    .. barAxisH               电芯编号
//	+gap                                  分隔
//	trendTop     .. cellChartHeight        压差历史折线
const (
	cellChartHeight = 118
	barValueH       = 14
	barBodyH        = 46
	barAxisH        = 15
	cellChartGap    = 7
	trendLabelW     = 40
)

// cellChart 自绘“电芯电压柱状图 + 压差历史折线”。
//
//   - 柱状图：4 节电芯电压，柱高按当前 4 节的最小/最大值自适应放大（至少 20mV 量程），
//     柱顶连折线以直观呈现均衡程度，并画出平均电压虚线。
//   - 折线图：从 /api/history 取回的压差（最大−最小）序列，展示均衡性的变化趋势。
//
// 颜色取自系统主题色（COLOR_WINDOWTEXT / COLOR_GRAYTEXT），浅色与深色主题下都可读。
type cellChart struct {
	cw    *walk.CustomWidget
	cells [4]float64
	ok    bool
	trend []float64 // 压差序列，单位 mV
}

func newCellChart(parent walk.Container, f uiFonts) *cellChart {
	c := &cellChart{}
	cw, err := walk.NewCustomWidget(parent, 0, func(cv *walk.Canvas, _ walk.Rectangle) error {
		c.paint(cv, f)
		return nil
	})
	if err != nil {
		fatal(err)
	}
	_ = cw.SetMinMaxSize(walk.Size{Width: 200, Height: cellChartHeight}, walk.Size{Height: cellChartHeight})
	c.cw = cw
	return c
}

func (c *cellChart) setCells(cells [4]float64) {
	c.cells, c.ok = cells, true
	if c.cw != nil {
		c.cw.Invalidate()
	}
}

func (c *cellChart) setTrend(v []float64) {
	c.trend = v
	if c.cw != nil {
		c.cw.Invalidate()
	}
}

// clear 回到“等待设备数据”占位状态。
func (c *cellChart) clear() {
	c.ok = false
	if c.cw != nil {
		c.cw.Invalidate()
	}
}

// invalidate 标记需要重绘（必须在 UI 线程调用）。
func (c *cellChart) invalidate() {
	if c.cw != nil {
		c.cw.Invalidate()
	}
}

// gfx 收集一帧绘制期间创建的 GDI 对象，绘制结束时统一释放，避免句柄泄漏。
type gfx struct {
	brushes []*walk.SolidColorBrush
	pens    []*walk.GeometricPen
}

func (g *gfx) brush(c walk.Color) *walk.SolidColorBrush {
	b, err := walk.NewSolidColorBrush(c)
	if err != nil {
		return nil
	}
	g.brushes = append(g.brushes, b)
	return b
}

// pen 返回一支实线几何笔；width96 以 1/96 英寸为单位，由 walk 按 DPI 自动放大。
func (g *gfx) pen(c walk.Color, width96 int) *walk.GeometricPen {
	br := g.brush(c)
	if br == nil {
		return nil
	}
	p, err := walk.NewGeometricPen(walk.PenSolid, width96, br)
	if err != nil {
		return nil
	}
	g.pens = append(g.pens, p)
	return p
}

func (g *gfx) release() {
	for _, p := range g.pens {
		p.Dispose()
	}
	for _, b := range g.brushes {
		b.Dispose()
	}
}

func (c *cellChart) paint(cv *walk.Canvas, f uiFonts) {
	bounds := c.cw.ClientBoundsPixels()
	if bounds.Width < 120 || bounds.Height < 60 {
		return
	}
	dpi := cv.DPI()
	sc := func(v int) int { return v * dpi / 96 }

	colText := walk.Color(win.GetSysColor(win.COLOR_WINDOWTEXT))
	colDim := walk.Color(win.GetSysColor(win.COLOR_GRAYTEXT))
	colBar := walk.RGB(0x4A, 0x90, 0xD9)
	colBarMin := walk.RGB(0xE0, 0x9A, 0x3C)
	colLine := walk.RGB(0xE8, 0x71, 0x3C)
	colTrend := walk.RGB(0x7B, 0x61, 0xD6)

	smallF := nz(f.small, f.normal)

	if !c.ok {
		drawText(cv, "等待设备数据…", nz(f.normal), colDim,
			walk.Rectangle{X: sc(8), Y: 0, Width: bounds.Width - sc(16), Height: bounds.Height},
			walk.TextSingleLine|walk.TextVCenter|walk.TextLeft)
		return
	}

	g := &gfx{}
	defer g.release()

	// ---------------- 柱状图：4 节电芯电压 ----------------
	minV, maxV := c.cells[0], c.cells[0]
	for _, v := range c.cells {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	mid := (minV + maxV) / 2
	span := maxV - minV
	if span < 0.020 { // 量程下限 20mV：4 节完全一致时柱子也不会贴顶
		span = 0.020
	}
	yMin, yMax := mid-span/2, mid+span/2

	left := sc(6)
	right := bounds.Width - sc(6)
	segW := (right - left) / len(c.cells)
	if segW <= 0 {
		return
	}
	barBottom := sc(barValueH + barBodyH)
	barMaxH := sc(barBodyH)
	barW := segW / 2
	if barW > sc(44) {
		barW = sc(44)
	}

	barBrush := g.brush(colBar)
	minBrush := g.brush(colBarMin)

	// 平均电压参考线（手工虚线，只依赖 PS_SOLID，最稳）
	if ap := g.pen(colDim, 1); ap != nil {
		avgY := barBottom - int((mid-yMin)/(yMax-yMin)*float64(barMaxH))
		for x := left; x < right; x += sc(10) {
			end := x + sc(6)
			if end > right {
				end = right
			}
			_ = cv.DrawLinePixels(ap, walk.Point{X: x, Y: avgY}, walk.Point{X: end, Y: avgY})
		}
	}

	tops := make([]walk.Point, 0, len(c.cells))
	for i, v := range c.cells {
		cx := left + i*segW + segW/2
		h := int((v - yMin) / (yMax - yMin) * float64(barMaxH))
		if h < sc(2) {
			h = sc(2)
		}
		br := barBrush
		if v == minV && maxV-minV > 0.0005 { // 最低的一节用暖色标出，一眼看出短板
			br = minBrush
		}
		if br != nil {
			_ = cv.FillRectanglePixels(br, walk.Rectangle{
				X: cx - barW/2, Y: barBottom - h, Width: barW, Height: h,
			})
		}

		top := walk.Point{X: cx, Y: barBottom - h}
		tops = append(tops, top)

		drawText(cv, fmt.Sprintf("%.3f", v), smallF, colText,
			walk.Rectangle{X: cx - segW/2, Y: top.Y - sc(barValueH), Width: segW, Height: sc(barValueH)},
			walk.TextSingleLine|walk.TextVCenter|walk.TextCenter)
		drawText(cv, fmt.Sprintf("#%d", i+1), smallF, colDim,
			walk.Rectangle{X: cx - segW/2, Y: barBottom + sc(1), Width: segW, Height: sc(barAxisH)},
			walk.TextSingleLine|walk.TextVCenter|walk.TextCenter)
	}

	// 柱顶折线：越平直说明 4 节越均衡
	if lp := g.pen(colLine, 2); lp != nil && len(tops) > 1 {
		_ = cv.DrawPolylinePixels(lp, tops)
	}

	// ---------------- 折线图：压差随时间的变化 ----------------
	trendTop := sc(barValueH + barBodyH + barAxisH + cellChartGap)
	trendBot := bounds.Height - sc(1)
	if trendBot-trendTop < sc(28) {
		return
	}

	drawText(cv, "压差", smallF, colDim,
		walk.Rectangle{X: sc(2), Y: trendTop, Width: sc(trendLabelW), Height: trendBot - trendTop},
		walk.TextSingleLine|walk.TextVCenter|walk.TextLeft)

	infoW := sc(70)
	tx0 := sc(trendLabelW) + sc(2)
	tx1 := bounds.Width - infoW - sc(4)
	ty0 := trendTop + sc(2)
	ty1 := trendBot - sc(3)

	if bp := g.pen(colDim, 1); bp != nil && tx1 > tx0 {
		_ = cv.DrawLinePixels(bp, walk.Point{X: tx0, Y: ty1}, walk.Point{X: tx1, Y: ty1})
	}

	if len(c.trend) < 2 || tx1 <= tx0 || ty1 <= ty0 {
		drawText(cv, "正在积累趋势数据…", smallF, colDim,
			walk.Rectangle{X: tx0, Y: ty0, Width: tx1 - tx0, Height: ty1 - ty0},
			walk.TextSingleLine|walk.TextVCenter|walk.TextLeft)
		return
	}

	tMax, tMin := c.trend[0], c.trend[0]
	for _, d := range c.trend {
		if d > tMax {
			tMax = d
		}
		if d < tMin {
			tMin = d
		}
	}
	rng := tMax - tMin
	if rng < 4 { // 量程下限 4mV：完全平直时折线不会贴边
		rng = 4
	}
	lo, hi := tMin-rng*0.15, tMax+rng*0.15

	if hp := g.pen(colTrend, 2); hp != nil {
		n := len(c.trend)
		pts := make([]walk.Point, 0, n)
		for i, d := range c.trend {
			pts = append(pts, walk.Point{
				X: tx0 + (tx1-tx0)*i/(n-1),
				Y: ty1 - int((d-lo)/(hi-lo)*float64(ty1-ty0)),
			})
		}
		_ = cv.DrawPolylinePixels(hp, pts)
	}

	// 右侧三行：当前 / 历史最大 / 历史最小
	lineH := (ty1 - ty0) / 3
	const rightAlign = walk.TextSingleLine | walk.TextVCenter | walk.TextRight
	rows := []struct {
		text string
		col  walk.Color
	}{
		{fmt.Sprintf("当前 %.0f mV", c.trend[len(c.trend)-1]), colTrend},
		{fmt.Sprintf("最大 %.0f mV", tMax), colText},
		{fmt.Sprintf("最小 %.0f mV", tMin), colText},
	}
	for i, r := range rows {
		drawText(cv, r.text, smallF, r.col,
			walk.Rectangle{X: bounds.Width - infoW, Y: ty0 + i*lineH, Width: infoW - sc(4), Height: lineH},
			rightAlign)
	}
}

// ---------------------------------------------------------------- 配置读写（经 Web 接口）

var (
	actionKeys   = []string{"shutdown", "sleep", "hibernate", "none"}
	actionLabels = []string{"关机", "睡眠", "休眠", "仅提示"}
)

// actionIndex 返回动作在 actionKeys 中的下标（未知动作按第一个处理）。
func actionIndex(a string) int {
	for i, k := range actionKeys {
		if k == a {
			return i
		}
	}
	return 0
}

// checkedActionIndex 返回当前选中的触发动作下标；异常情况下回退到第一项。
func checkedActionIndex(rs []*walk.RadioButton) int {
	for i, rb := range rs {
		if rb != nil && rb.Checked() {
			return i
		}
	}
	return 0
}

// setCheckedAction 按 actionKeys 下标选中对应单选项。
func setCheckedAction(rs []*walk.RadioButton, idx int) {
	if idx < 0 || idx >= len(rs) {
		return
	}
	for i, rb := range rs {
		if rb != nil {
			rb.SetChecked(i == idx)
		}
	}
}

// waitWebReady 等待 Web 仪表盘完成端口绑定。
// Web 服务与 GUI 同时启动，而监听端口是系统随机分配的：在 gWebPort 被写入实际端口之前
// 发起的请求会打到旧的默认端口上，因此首次请求前必须等一下。
func waitWebReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if gWebBound {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// fetchCellTrend 取最近 n 个采样点，换算成压差序列（单位 mV）。
// 返回结构复用 web.go 中的 historyPoint（同 package）。
func fetchCellTrend(n int) ([]float64, error) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/api/history?limit=%d", gWebPort, n))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var pts []historyPoint
	if err := json.NewDecoder(resp.Body).Decode(&pts); err != nil {
		return nil, err
	}
	out := make([]float64, 0, len(pts))
	for _, p := range pts {
		cs := [4]float64{p.C1, p.C2, p.C3, p.C4}
		mn, mx := cs[0], cs[0]
		for _, v := range cs {
			if v < mn {
				mn = v
			}
			if v > mx {
				mx = v
			}
		}
		if mn <= 0 { // 无效样本跳过
			continue
		}
		out = append(out, (mx-mn)*1000)
	}
	return out, nil
}

type cfgResp struct {
	Enabled   bool   `json:"enabled"`
	Low       int    `json:"low"`
	Action    string `json:"action"`
	Autostart bool   `json:"autostart"`
	Path      string `json:"path"`
}

// fetchConfig 读取当前低电量保护配置。
func fetchConfig() (cfgResp, error) {
	var c cfgResp
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/api/config", gWebPort))
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return c, err
	}
	return c, nil
}

// postConfig 保存配置（服务端即时生效并持久化到 ups-monitor.json，同时同步注册表自启）。
func postConfig(low int, action string, autostart bool) error {
	body, err := json.Marshal(map[string]interface{}{
		"low": low, "action": action, "autostart": autostart,
	})
	if err != nil {
		return err
	}
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/api/config", gWebPort),
		"application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// ---------------------------------------------------------------- 运行时控件引用

type guiRefs struct {
	banner  *bannerWidget
	dev     *walk.Label
	socBar  *walk.ProgressBar
	socTxt  *walk.Label
	v       map[string]*walk.Label // 各指标值标签（按中文键索引）
	rtBasis *walk.Label            // 本地续航估算的依据说明（小灰字）
	chart   *cellChart             // 电芯电压柱状图 + 压差历史折线
	cellSum *walk.Label            // 电芯合计量值（图表下方补充说明）
	footer  *walk.Label

	// 底部一行保存统计片段，由 setStats() 填；footerURL 由 update() 填
	footerURL string
	runLbl    *walk.Label

	// 低电量保护设置控件
	cfgMsg   *walk.Label
	cbEnable *walk.CheckBox
	neLow    *walk.NumberEdit
	radios   []*walk.RadioButton // 触发动作单选组（按 actionKeys 顺序）
	cbAuto   *walk.CheckBox
}

// loadAppIcon 加载窗口/托盘图标：优先用内嵌 ICO 写出的临时文件，
// 失败则退回 rsrc 嵌进可执行文件的图标资源（ID=1）。
func loadAppIcon() (*walk.Icon, error) {
	if data, err := trayIconFS.ReadFile("assets/icon.ico"); err == nil {
		if f, err := os.CreateTemp("", "ups-monitor-*.ico"); err == nil {
			if _, err := f.Write(data); err == nil {
				f.Close()
				if ic, err := walk.NewIconFromFile(f.Name()); err == nil {
					return ic, nil
				} else {
					logf("loadAppIcon: NewIconFromFile 失败: %v", err)
				}
			} else {
				f.Close()
			}
		}
	} else {
		logf("loadAppIcon: 读取内嵌图标失败: %v", err)
	}
	if ic, err := walk.NewIconFromResourceId(1); err == nil {
		return ic, nil
	} else {
		logf("loadAppIcon: 资源图标回退失败: %v", err)
	}
	return nil, fmt.Errorf("无法加载图标")
}

// ---------------------------------------------------------------- 主界面

// runGUI 启动原生 GUI 窗口 + 系统托盘，并作为默认运行模式。
func runGUI() {
	logf("runGUI: start, args=%v", os.Args)

	// 单一监控源：Web 仪表盘（不自动开浏览器，GUI 即主界面）。
	// 端口缺省 127.0.0.1:0 —— 仅监听本机并由系统随机分配端口，避免占用固定 8080、也不对外暴露。
	addr := *fWeb
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	// 在启动监听前注册：把系统实际分配的端口写进日志（GUI 无控制台，靠日志排查）。
	onWebReady = func(u string) { logf("runGUI: Web 仪表盘已启动 %s", u) }
	go guard("runWeb", func() { runWeb(addr, true) })
	logf("runGUI: web monitor launching on %s", addr)

	icon, iconErr := loadAppIcon()
	if iconErr != nil {
		logf("runGUI: load icon failed (ignored): %v", iconErr)
	}

	mw, err := walk.NewMainWindow()
	if err != nil {
		fatal(err)
	}
	logf("runGUI: main window created")
	if icon != nil {
		_ = mw.SetIcon(icon)
	}
	mw.SetTitle("UGREEN US3000 UPS Monitor")
	// walk 的 MainWindow 默认自带一条空状态栏，白占高度又没有信息，直接隐藏。
	mw.StatusBar().SetVisible(false)
	// 逻辑尺寸：内容约 675 逻辑高，留出余量以免不同主题/DPI 下底部被裁；
	// 同时偏矮一些，1080p @150%（仅 720 逻辑高）的笔记本也能完整显示。
	_ = mw.SetSize(walk.Size{Width: 560, Height: 710})

	f := loadFonts()
	logf("runGUI: fonts normal=%v bold=%v banner=%v socHuge=%v small=%v",
		f.normal != nil, f.bold != nil, f.banner != nil, f.socHuge != nil, f.small != nil)
	setF(mw, nz(f.normal))

	// 表单自身必须有布局：walk 的 startLayout 会对表单调用 CreateLayoutItemsForContainer，
	// 表单无布局时 ContainerBase.CreateLayoutItem 会解引用 nil layout 而 panic。
	rootLay := walk.NewVBoxLayout()
	_ = rootLay.SetSpacing(rootSpacing)
	_ = rootLay.SetMargins(walk.Margins{HNear: rootMargin, VNear: rootMargin, HFar: rootMargin, VFar: rootMargin})
	if err := mw.SetLayout(rootLay); err != nil {
		fatal(err)
	}

	// ---- 顶部渐变横幅（自绘：标题 / 运行状态 / 接口信息 / 大号电量）----
	bn := newBanner(mw, f)

	// 横幅下方的一行小字：固件与序列号
	devLbl := lbl(mw, "等待设备数据…", nz(f.small, f.normal))

	// ---- 电池 ----
	battGb := group(mw, "电池", f)
	socRow := row(battGb)
	socKey := lbl(socRow, "电量", nz(f.bold, f.normal))
	_ = socKey.SetMinMaxSize(walk.Size{Width: keyColWidth}, walk.Size{})
	socBar, err := walk.NewProgressBar(socRow)
	if err != nil {
		fatal(err)
	}
	socBar.SetRange(0, 100)
	_ = socBar.SetMinMaxSize(walk.Size{Width: 230, Height: 20}, walk.Size{})
	socTxt := lbl(socRow, "—", nz(f.big, f.bold))
	fixedWidth(socTxt, 68)

	vals := map[string]*walk.Label{}
	vBattV, vCellSum := kv(battGb, "电池电压", "电芯合计", f)
	vChg, vHealth := kv(battGb, "充电电流", "均衡评估", f)
	// 剩余续航分两个来源展示：设备上报（仅电池供电时固件才给值）+ 本地估算
	vRtDev, vRtLocal := kv(battGb, "设备续航", "本地估算", f)
	rtBasisLbl := lbl(battGb, "—", nz(f.small, f.normal))
	endRow(battGb) // 让分组横向撑满，否则上面那行小字会被居中
	vals["电池电压"], vals["电芯合计"] = vBattV, vCellSum
	vals["充电电流"], vals["均衡评估"] = vChg, vHealth
	vals["设备续航"], vals["本地估算"] = vRtDev, vRtLocal

	// ---- 电力 ----
	powerGb := group(mw, "电力", f)
	vInV, vInA := kv(powerGb, "输入电压", "输入电流", f)
	vInW, vLoad := kv(powerGb, "输入功率", "负载", f)
	vals["输入电压"], vals["输入电流"] = vInV, vInA
	vals["输入功率"], vals["负载"] = vInW, vLoad

	// ---- 电芯 ----
	cellGb := group(mw, "电芯电压（4S 串联） · 柱状图 + 压差折线", f)
	chart := newCellChart(cellGb, f)
	cellSumLbl := lbl(cellGb, "—", nz(f.normal))
	endRow(cellGb) // 让该分组也横向撑满（自绘控件本身不“可增长”）

	// ---- 低电量自动保护（可编辑）----
	cfgGb := group(mw, "低电量自动保护 · 设置", f)

	cbRow := row(cfgGb)
	cbEnable, err := walk.NewCheckBox(cbRow)
	if err != nil {
		fatal(err)
	}
	_ = cbEnable.SetText("启用低电量保护（仅电池供电、电量连续低于阈值时触发）")
	setF(cbEnable, nz(f.bold, f.normal))
	leftAlignButton(cbEnable)
	endRow(cbRow)

	lowRow := row(cfgGb)
	lowKey := lbl(lowRow, "电量阈值", nz(f.bold, f.normal))
	_ = lowKey.SetMinMaxSize(walk.Size{Width: keyColWidth}, walk.Size{})
	neLow, err := walk.NewNumberEdit(lowRow)
	if err != nil {
		fatal(err)
	}
	_ = neLow.SetRange(0, 100)
	_ = neLow.SetDecimals(0)
	_ = neLow.SetValue(20)
	fixedWidth(neLow, 72)
	setF(neLow, nz(f.normal))
	_ = lbl(lowRow, "%（建议 20–30）", nz(f.small, f.normal))
	endRow(lowRow)

	// 触发动作：用单选按钮替代下拉框 —— 下拉列表在部分远程/高 DPI 环境下弹出即收起，
	// 这里改成常驻可见的单选项，一眼看全所有可选动作，也不再需要展开-选择两步操作。
	actRow := row(cfgGb)
	actKey := lbl(actRow, "触发动作", nz(f.bold, f.normal))
	_ = actKey.SetMinMaxSize(walk.Size{Width: keyColWidth}, walk.Size{})
	// 连续创建 → walk 自动把它们并入同一个 RadioButtonGroup（互斥）
	radios := make([]*walk.RadioButton, 0, len(actionLabels))
	for i, name := range actionLabels {
		rb, err := walk.NewRadioButton(actRow)
		if err != nil {
			fatal(err)
		}
		_ = rb.SetText(name)
		setF(rb, nz(f.normal))
		rb.SetChecked(i == 0)
		radios = append(radios, rb)
	}
	endRow(actRow)

	actHintRow := row(cfgGb)
	actHintPad := lbl(actHintRow, "", nz(f.small, f.normal))
	_ = actHintPad.SetMinMaxSize(walk.Size{Width: keyColWidth}, walk.Size{})
	_ = lbl(actHintRow, "关机 / 睡眠 / 休眠：调用系统电源操作；“仅提示”只写日志、不执行任何操作。", nz(f.small, f.normal))
	endRow(actHintRow)

	autoRow := row(cfgGb)
	cbAuto, err := walk.NewCheckBox(autoRow)
	if err != nil {
		fatal(err)
	}
	_ = cbAuto.SetText("开机自启（随 Windows 启动监控）")
	setF(cbAuto, nz(f.normal))
	leftAlignButton(cbAuto)
	endRow(autoRow)

	btnRow := row(cfgGb)
	btnSave, err := walk.NewPushButton(btnRow)
	if err != nil {
		fatal(err)
	}
	_ = btnSave.SetText("保存设置")
	_ = btnSave.SetMinMaxSize(walk.Size{Width: 96, Height: 30}, walk.Size{Width: 96, Height: 30})
	setF(btnSave, nz(f.bold, f.normal))
	btnWeb, err := walk.NewPushButton(btnRow)
	if err != nil {
		fatal(err)
	}
	_ = btnWeb.SetText("打开 Web 页面")
	_ = btnWeb.SetMinMaxSize(walk.Size{Width: 118, Height: 30}, walk.Size{Width: 118, Height: 30})
	setF(btnWeb, nz(f.normal))
	endRow(btnRow)

	cfgMsg := lbl(cfgGb, "正在读取当前配置…", nz(f.small, f.normal))
	pathLbl := lbl(cfgGb, "配置文件："+configPath, nz(f.small, f.normal))

	// ---- 底部提示 ----
	footerLbl := lbl(mw, "关闭窗口将最小化到托盘 · 右键托盘图标：打开 Web 页面 / 打开 GUI / 退出", nz(f.small, f.normal))
	// 长期运行统计：运行时长 / 累计帧数 / 日志滚动策略，随轮询刷新
	runLbl := lbl(mw, "—", nz(f.small, f.normal))

	refs := &guiRefs{
		banner: bn, dev: devLbl, socBar: socBar, socTxt: socTxt,
		v: vals, rtBasis: rtBasisLbl, chart: chart, cellSum: cellSumLbl, footer: footerLbl, runLbl: runLbl,
		cfgMsg: cfgMsg, cbEnable: cbEnable, neLow: neLow, radios: radios, cbAuto: cbAuto,
	}
	_ = pathLbl

	// 未启用保护时，阈值与动作无意义 —— 视觉上同步置灰，避免误操作
	syncEnable := func() {
		on := cbEnable.Checked()
		neLow.SetEnabled(on)
		for _, rb := range radios {
			rb.SetEnabled(on)
		}
	}
	cbEnable.CheckedChanged().Attach(syncEnable)

	// 保存设置：低电量保护 + 开机自启，经 /api/config 即时生效并持久化
	btnSave.Clicked().Attach(func() {
		low := int(neLow.Value())
		if low < 0 {
			low = 0
		}
		if low > 100 {
			low = 100
		}
		idx := checkedActionIndex(radios)
		if idx < 0 || idx >= len(actionKeys) {
			idx = 0
		}
		action := actionKeys[idx]
		auto := cbAuto.Checked()
		if !cbEnable.Checked() {
			low = 0 // 0 表示禁用保护
		}
		if err := postConfig(low, action, auto); err != nil {
			cfgMsg.SetText("保存失败：" + err.Error())
			logf("postConfig 失败: %v", err)
			return
		}
		if low == 0 {
			cfgMsg.SetText("✔ 已保存：低电量保护已禁用")
		} else {
			cfgMsg.SetText(fmt.Sprintf("✔ 已保存：电量低于 %d%% 时%s", low, actionLabels[idx]))
		}
		logf("配置已保存: low=%d action=%s autostart=%v", low, action, auto)
	})

	btnWeb.Clicked().Attach(func() { openBrowser(gDashboardURL) })

	// 关闭窗口 → 最小化到托盘，不退出进程
	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		*canceled = true
		mw.Hide()
	})

	// 系统托盘：右键“打开 Web 页面” / “打开 GUI” / “退出”
	// 某些无桌面的环境（如服务器会话）无法创建托盘图标，此时降级为仅窗口+Web，不致命退出。
	ni, niErr := walk.NewNotifyIcon(mw)
	if niErr != nil {
		logf("警告: 无法创建系统托盘图标（环境可能无桌面）: %v", niErr)
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
		logf("托盘图标已创建（菜单：打开 Web 页面 / 打开 GUI / 退出）")
	}

	// 启动后加载一次当前配置到界面（等待 Web 就绪，带重试）
	go guard("loadConfig", func() {
		for i := 0; i < 40; i++ {
			cfg, err := fetchConfig()
			if err != nil {
				time.Sleep(400 * time.Millisecond)
				continue
			}
			c := cfg
			mw.Synchronize(func() {
				refs.cbEnable.SetChecked(c.Enabled)
				low := c.Low
				if low <= 0 {
					low = 20 // 未启用时给个默认阈值，便于直接勾选启用
				}
				_ = refs.neLow.SetValue(float64(low))
				setCheckedAction(refs.radios, actionIndex(c.Action))
				refs.cbAuto.SetChecked(c.Autostart)
				if c.Enabled {
					refs.cfgMsg.SetText("已加载当前配置")
				} else {
					refs.cfgMsg.SetText("已加载当前配置：保护未启用")
				}
				syncEnable()
			})
			logf("loadConfig: enabled=%v low=%d action=%s autostart=%v", c.Enabled, c.Low, c.Action, c.Autostart)
			return
		}
		logf("loadConfig: 读取配置超时（Web 未就绪）")
	})

	// 定时轮询监控数据并更新界面
	// 顺带在日志里留下"长期运行"痕迹：状态切换即时记录，其余每 logBeatInterval 一条心跳。
	// 逐帧落盘会让日志暴涨且毫无信息量，这里只记事件 —— 配合滚动策略，跑几年也不怕。
	go guard("poll", func() {
		tick := time.NewTicker(time.Duration(*fInterval) * time.Millisecond)
		defer tick.Stop()
		var lastStatus string
		lastBeat := time.Now()
		for range tick.C {
			s, di, _, frames, perr := pollStatus()
			snapshot, devInfo, pollErr, n := s, di, perr, frames

			if snapshot != nil {
				if snapshot.Status != lastStatus {
					if lastStatus != "" {
						logf("状态切换: %s → %s（电量 %d%%，负载 %d%%）",
							lastStatus, snapshot.Status, snapshot.ChargePercent, snapshot.LoadPercent)
					}
					lastStatus = snapshot.Status
				}
				if time.Since(lastBeat) >= logBeatInterval {
					lastBeat = time.Now()
					rt := snapshot.Runtime()
					logf("心跳: %s 电量=%d%% 负载=%d%% 输入=%.2fV/%.3fA 电池=%.3fV/%.0fmA 设备续航=%s 本地估算=%s 累计=%s 帧",
						snapshot.Status, snapshot.ChargePercent, snapshot.LoadPercent,
						snapshot.InputVoltage, snapshot.InputCurrent,
						snapshot.BatteryVoltage, snapshot.ChargeCurrent,
						fmtDuration(rt.DeviceSec), fmtDuration(rt.LocalSec), fmtFrames(n))
				}
			}

			mw.Synchronize(func() {
				refs.update(snapshot, devInfo, pollErr)
				refs.setStats(n)
			})
		}
	})

	// 压差历史趋势：低频单独轮询 /api/history（取最近 N 点算压差序列），
	// 供图表下半部分的折线使用；与状态轮询解耦，互不影响刷新节奏。
	go guard("trend", func() {
		const trendPoints = 150 // 约最近 2.5 分钟（采样 1Hz）
		waitWebReady(20 * time.Second)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		loggedErr := false
		for {
			if v, err := fetchCellTrend(trendPoints); err == nil && len(v) > 0 {
				tr := v
				mw.Synchronize(func() {
					if refs.chart != nil {
						refs.chart.setTrend(tr)
					}
				})
			} else if err != nil && !loggedErr {
				loggedErr = true // 只在首次失败时记录，避免日志刷屏
				logf("fetchCellTrend 失败（后续不再重复记录）: %v", err)
			}
			<-tick.C
		}
	})

	logf("runGUI: entering message loop")
	mw.Show()
	mw.Run()
	logf("runGUI: message loop exited (this should only happen on quit)")
}

// update 将最新样本刷新到 GUI 控件（必须在 UI 线程调用，外部用 Synchronize 包住）。
func (r *guiRefs) update(s *protocol.Sample, di deviceInfo, connErr error) {
	if gDashboardURL != "" && r.footerURL != gDashboardURL {
		r.footerURL = gDashboardURL
		r.renderFooter()
	}

	if connErr != nil || s == nil {
		r.banner.set("UGREEN UPS Monitor", "设备未连接 · 正在重试…", "USB HID", "--", "剩余电量")
		r.dev.SetText("请检查 UPS 的 USB 数据线是否已连接：" + di.Product)
		r.socTxt.SetText("—")
		r.socBar.SetValue(0)
		if r.chart != nil {
			r.chart.clear()
		}
		r.setCellSum("—")
		if r.rtBasis != nil {
			_ = r.rtBasis.SetText("—")
		}
		for _, k := range []string{"电池电压", "电芯合计", "充电电流", "均衡评估", "设备续航", "本地估算",
			"输入电压", "输入电流", "输入功率", "负载"} {
			if l, ok := r.v[k]; ok {
				_ = l.SetText("—")
			}
		}
		return
	}

	set := func(k, v string) {
		if l, ok := r.v[k]; ok {
			_ = l.SetText(v)
		}
	}

	title := di.Product
	if title == "" {
		title = "UGREEN US3000 UPS"
	} else if !strings.Contains(strings.ToUpper(title), "UGREEN") {
		title = "UGREEN " + title
	}
	r.banner.set(title, s.Status,
		fmt.Sprintf("USB HID  VID:%s  PID:%s", di.VendorID, di.ProductID),
		fmt.Sprintf("%d%%", s.ChargePercent), "剩余电量")
	r.dev.SetText(fmt.Sprintf("固件 %s  ·  硬件 %s  ·  USB %s  ·  序列号 %s",
		di.Firmware, di.Hardware, di.USBVersion, di.Serial))

	r.socTxt.SetText(fmt.Sprintf("%d%%", s.ChargePercent))
	r.socBar.SetValue(s.ChargePercent)

	set("电池电压", fmt.Sprintf("%.3f V", s.BatteryVoltage))
	set("电芯合计", fmt.Sprintf("%.3f V", s.CellSum()))
	set("充电电流", fmt.Sprintf("%.0f mA", s.ChargeCurrent))
	set("均衡评估", s.Health())

	// 剩余续航：设备上报值与本地估算值并列展示，二者口径不同、不互相替代
	rt := s.Runtime()
	if rt.DeviceSec > 0 {
		set("设备续航", fmtDuration(rt.DeviceSec))
	} else {
		set("设备续航", "—")
	}
	if rt.LocalSec > 0 {
		set("本地估算", fmtDuration(rt.LocalSec))
	} else {
		set("本地估算", "—")
	}
	basis := rt.Basis
	if rt.DeviceSec <= 0 {
		basis += "　·　设备续航需电池供电时才由固件给出"
	}
	if r.rtBasis != nil {
		_ = r.rtBasis.SetText("估算依据：" + basis)
	}

	set("输入电压", fmt.Sprintf("%.3f V", s.InputVoltage))
	set("输入电流", fmt.Sprintf("%.3f A", s.InputCurrent))
	set("输入功率", fmt.Sprintf("%.2f W", s.InputPower()))
	if s.LoadValid {
		set("负载", fmt.Sprintf("%d %%", s.LoadPercent))
	} else {
		set("负载", "—")
	}

	if r.chart != nil {
		r.chart.setCells([4]float64{s.Cells[0], s.Cells[1], s.Cells[2], s.Cells[3]})
	}
	r.setCellSum(fmt.Sprintf("4 节串联合计 %.3f V  ·  当前压差 %.0f mV  ·  均衡评估：%s",
		s.CellSum(), s.CellDelta(), s.Health()))
}

// setCellSum 更新电芯图表下方的一行补充说明。
func (r *guiRefs) setCellSum(text string) {
	if r.cellSum != nil {
		_ = r.cellSum.SetText(text)
	}
}

// setStats 刷新底部长期运行统计（已运行时长 + 累计帧数）。
// 程序按年运行是常态，光看帧数没概念，配上运行时长才读得懂；
// 帧数超过 1 亿（约 3.2 年 @1Hz）后改用「亿」表示，避免一长串数字挤爆底栏。
func (r *guiRefs) setStats(frames int) {
	if r.runLbl == nil {
		return
	}
	_ = r.runLbl.SetText(fmt.Sprintf("已运行 %s  ·  累计 %s 帧  ·  日志按 %d MiB × %d 份滚动",
		fmtDuration(int(time.Since(runStart).Seconds())), fmtFrames(frames),
		logMaxBytes>>20, logKeepFiles))
}

// renderFooter 由 URL 片段与固定提示拼出底部整行文本。
func (r *guiRefs) renderFooter() {
	if r.footer == nil {
		return
	}
	if r.footerURL == "" {
		return
	}
	_ = r.footer.SetText("Web 仪表盘：" + r.footerURL + "  ·  关闭窗口将最小化到托盘")
}

// ---------------------------------------------------------------- 设备列表窗口

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
	_ = mw.SetSize(walk.Size{Width: 760, Height: 480})
	if err := mw.SetLayout(walk.NewVBoxLayout()); err != nil {
		fatal(err)
	}
	te, err := walk.NewTextEdit(mw)
	if err != nil {
		fatal(err)
	}
	te.SetReadOnly(true)
	te.SetText(out)
	mw.Run()
}

// pollStatus / statusResp / viewerSample 的定义见 main.go（同一 package，GUI 与控制台查看器共用）。
