//go:build windows

package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	hid "ugreen-ups/hid"
	"ugreen-ups/protocol"
)

//go:embed assets/dashboard.html
var dashboardFS embed.FS

// ---------------------------------------------------------------- 采集器

type historyPoint struct {
	T     int64   `json:"t"`
	InV   float64 `json:"inv"`
	BatV  float64 `json:"batv"`
	Cur   float64 `json:"cur"`
	Load  int     `json:"load"`
	SOC   int     `json:"soc"`
	Power float64 `json:"power"`
	C1    float64 `json:"c1"`
	C2    float64 `json:"c2"`
	C3    float64 `json:"c3"`
	C4    float64 `json:"c4"`
}

type monitor struct {
	mu     sync.Mutex
	dev    *hid.Device
	info   deviceInfo
	latest *protocol.Sample

	history  []historyPoint
	events   []string
	frames   int
	lastErr  string
	lastMode protocol.Mode
	pending  protocol.Mode
	stable   int
	interval time.Duration
}

func newMonitor(dev *hid.Device) *monitor {
	return &monitor{
		dev: dev,
		info: deviceInfo{
			Manufacturer: dev.Manufacturer,
			Product:      dev.Product,
			Serial:       dev.Serial,
			VendorID:     fmt.Sprintf("%04X", dev.VendorID),
			ProductID:    fmt.Sprintf("%04X", dev.ProductID),
			Firmware:     fmt.Sprintf("%d.%02d", dev.Version>>8, dev.Version&0xFF),
		},
		interval: time.Duration(*fInterval) * time.Millisecond,
	}
}

func (m *monitor) loop() {
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	for range tick.C {
		m.readOnce()
	}
}

func (m *monitor) readOnce() {
	raw, err := m.dev.Read(3000)

	m.mu.Lock()
	defer m.mu.Unlock()

	if err != nil {
		m.lastErr = err.Error()
		m.addEvent("读取失败: " + err.Error())
		// 异步重连，避免持锁阻塞
		go func() {
			time.Sleep(500 * time.Millisecond)
			if nd, e := openUPS(); e == nil {
				m.mu.Lock()
				m.dev = nd
				m.lastErr = ""
				m.addEvent("设备已重新连接")
				m.mu.Unlock()
			}
		}()
		return
	}

	s, err := protocol.Parse(raw)
	if err != nil {
		m.lastErr = err.Error()
		return
	}
	m.lastErr = ""
	m.frames++

	// 周期性用 BMS Feature 报告校准 SOC
	if m.frames%30 == 1 {
		if f, e := m.dev.GetFeature(0x06, 0); e == nil && len(f) >= 6 {
			if soc := int(f[1]); soc > 0 && soc <= 100 {
				s.ChargePercent = soc
			}
		}
	}

	// 状态去抖：连续 3 次一致才确认切换
	if s.Mode != m.lastMode {
		if s.Mode == m.pending {
			m.stable++
			if m.stable >= 3 {
				m.addEvent("状态切换为 " + s.Status)
				m.lastMode = s.Mode
				m.pending, m.stable = protocol.ModeUnknown, 0
			}
		} else {
			m.pending, m.stable = s.Mode, 1
		}
	} else {
		m.pending, m.stable = protocol.ModeUnknown, 0
	}

	m.latest = s
	m.history = append(m.history, historyPoint{
		T: s.Time.Unix(), InV: s.InputVoltage, BatV: s.BatteryVoltage,
		Cur: s.InputCurrent, Load: s.LoadPercent, SOC: s.ChargePercent,
		Power: s.InputPower(), C1: s.Cells[0], C2: s.Cells[1],
		C3: s.Cells[2], C4: s.Cells[3],
	})
	if len(m.history) > 1800 { // 约 30 分钟 @1Hz
		m.history = m.history[len(m.history)-1800:]
	}

	// 低电量自动保护：电池供电且电量持续低于阈值时执行关机/睡眠/休眠
	if gLowBattery != nil && gLowBattery.enabled {
		if fire, msg := gLowBattery.feed(s); fire {
			m.addEvent(msg)
			_ = powerAction(gLowBattery.action)
		}
	}
}

func (m *monitor) addEvent(msg string) {
	m.events = append(m.events, time.Now().Format("15:04:05")+" "+msg)
	if len(m.events) > 20 {
		m.events = m.events[len(m.events)-20:]
	}
}

// ---------------------------------------------------------------- HTTP 服务

func runWeb(addr string) {
	dev, err := openUPS()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s连接 UPS 失败: %v%s\n", cRed, err, cReset)
		fmt.Fprintf(os.Stderr, "请确认 UPS 已通过 USB 连接，或运行 -list 查看设备。\n")
		os.Exit(1)
	}

	m := newMonitor(dev)
	m.readOnce()
	go m.loop()

	page, err := dashboardFS.ReadFile("assets/dashboard.html")
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取仪表盘页面失败: %v\n", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	})
	mux.HandleFunc("/api/status", m.handleStatus)
	mux.HandleFunc("/api/history", m.handleHistory)
	mux.HandleFunc("/api/config", m.handleConfig)

	addr = normalizeAddr(addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s监听 %s 失败: %v%s\n", cRed, addr, err, cReset)
		os.Exit(1)
	}

	url := fmt.Sprintf("http://localhost:%d/", portOf(ln.Addr()))
	fmt.Printf("%s UGREEN US3000 UPS 仪表盘已启动 %s\n", cBold+cGreen, cReset)
	fmt.Printf("  设备:   %s %s (序列号 %s, 固件 %s)\n",
		dev.Manufacturer, dev.Product, dev.Serial, m.info.Firmware)
	fmt.Printf("  地址:   %s%s%s\n", cCyan, url, cReset)
	fmt.Printf("  %s按 Ctrl+C 停止服务%s\n\n", cDim, cReset)

	if !*fNoBrowser {
		go func() {
			time.Sleep(400 * time.Millisecond)
			openBrowser(url)
		}()
	}

	if err := http.Serve(ln, mux); err != nil {
		fmt.Fprintf(os.Stderr, "服务异常: %v\n", err)
	}
}

func normalizeAddr(a string) string {
	a = strings.TrimSpace(a)
	if a == "" {
		return ":8080"
	}
	if !strings.Contains(a, ":") {
		return ":" + a
	}
	return a
}

func portOf(addr net.Addr) int {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.Port
	}
	return 8080
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func (m *monitor) handleStatus(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	type payload struct {
		Device   deviceInfo            `json:"device"`
		Sample   *sampleJSON           `json:"sample"`
		Frames   int                   `json:"frames"`
		Events   []string              `json:"events"`
		Error    string                `json:"error,omitempty"`
		ServerAt string                `json:"server_time"`
		Config   map[string]interface{} `json:"config"`
	}
	p := payload{
		Device:   m.info,
		Frames:   m.frames,
		Events:   m.events,
		Error:    m.lastErr,
		ServerAt: time.Now().Format(time.RFC3339),
	}
	en, low, act := gLowBattery.Snapshot()
	p.Config = map[string]interface{}{"enabled": en, "low": low, "action": act}
	if m.latest != nil {
		j := newSampleJSON(m.latest, false)
		p.Sample = &j
	}
	if p.Events == nil {
		p.Events = []string{}
	}
	json.NewEncoder(w).Encode(p)
}

func (m *monitor) handleHistory(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	out := m.history
	if out == nil {
		out = []historyPoint{}
	}
	json.NewEncoder(w).Encode(out)
}

// handleConfig 提供低电量保护的读取(GET)与修改(POST/PUT)。修改即时生效并持久化。
func (m *monitor) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	defer r.Body.Close()

	switch r.Method {
	case http.MethodGet:
		en, low, act := gLowBattery.Snapshot()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": en,
			"low":     low,
			"action":  act,
			"actions": []string{"shutdown", "sleep", "hibernate", "none"},
			"path":    configPath,
		})
	case http.MethodPost, http.MethodPut:
		var req struct {
			Low    int    `json:"low"`
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "无效的 JSON"})
			return
		}
		if req.Low < 0 || req.Low > 100 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "low 必须介于 0–100"})
			return
		}
		switch req.Action {
		case "shutdown", "sleep", "hibernate", "none":
		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "未知 action"})
			return
		}
		en := req.Low > 0
		gLowBattery.Configure(en, req.Low, req.Action)
		if err := saveConfig(AppConfig{Low: req.Low, Action: req.Action}); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "保存配置失败: " + err.Error()})
			return
		}
		en2, low2, act2 := gLowBattery.Snapshot()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": en2,
			"low":     low2,
			"action":  act2,
		})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
