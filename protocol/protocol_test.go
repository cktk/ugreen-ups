package protocol

import (
	"encoding/hex"
	"math"
	"testing"
)

// onlineFrames 是固件 V3.3 的实测抓包（市电供电 · 电池已充满，SOC 97%）。
//
// 每帧开头都是 `71 03 03 01 02 02 16 26`：
//
//	[0]=0x71 Report ID
//	[1]=0x03 [2]=0x03  → 固件 V3.3
//	[3]=0x01           → 硬件 V1
//	[4]=0x02 [5]=0x02  → 私有协议 2.2
//	[6]=0x16           → 保留
//	[7]=0x26           → 市电供电、电池已充满
//
// 注意 [1]/[2] 与 [3]/[4]/[5] 一一对应那些"看似随机"的帧头字节，
// 这正是把固件版本从 1.00（USB bcdDevice）纠正为 V3.3 的依据。
var onlineFrames = []string{
	"71030301020216267000000060000000300c2fc205bc400b043f2329130000001340270fde0fe70fe30fe5616421310000000000000000010003070000000000",
	"7103030102021626700000006000000030222fda050a400b03ac23290f0000001340270fde0fe70fe30fe5616421310000000000000000010003070000000000",
	"71030301020216267000000060000000307730320261400b01b62329080000001340280fde0fe80fe30fe5616421310000000000000000010003070000000000",
	"71030301020216267000000060000000307b30360229400c01a02329060000001140290fde0fe70fe30fe5616421310000000000000000010003070000000000",
	"71030301020216267000000060000000307c30360227400b019b2329060000001340270fde0fe70fe30fe5616421310000000000000000010003070000000000",
	"71030301020216267000000060000000307830310256400b01b32329070000001340260fde0fe70fe40fe5616421310000000000000000010003070000000000",
	"710303010202162670000000600000003075302f0265400b01bd2329070000001340270fde0fe70fe30fe5616421310000000000000000010003070000000000",
	"71030301020216267000000060000000307f3039022b400b01892329070000001340280fde0fe70fe30fe5616421310000000000000000010003070000000000",
}

func mustFrame(t *testing.T, h string) []byte {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("测试数据不是合法十六进制: %v", err)
	}
	if len(b) != FrameLen {
		t.Fatalf("测试帧长度 = %d，期望 %d", len(b), FrameLen)
	}
	return b
}

func mustParse(t *testing.T, h string) *Sample {
	t.Helper()
	s, err := Parse(mustFrame(t, h))
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	return s
}

// putBE 按大端写入 u16，供构造合成帧使用。
func putBE(b []byte, off, v int) {
	b[off] = byte(v >> 8)
	b[off+1] = byte(v)
}

// makeFrame 构造一帧用于测试：默认填上实测的帧头，再按需覆盖字段。
func makeFrame(status byte) []byte {
	b := make([]byte, FrameLen)
	copy(b, []byte{ReportID, 3, 3, 1, 2, 2, 0x16, status})
	return b
}

// ---------------------------------------------------------------- 固件版本

// 这是本次修复的核心：固件版本要从遥测帧头读，而不是拿 USB 的 bcdDevice 顶替。
func TestFirmwareVersionFromFrame(t *testing.T) {
	for i, h := range onlineFrames {
		s := mustParse(t, h)
		if got := s.Version.Firmware(); got != "V3.3" {
			t.Errorf("第 %d 帧固件版本 = %q，期望 %q", i+1, got, "V3.3")
		}
		if s.Version.FWMajor != 3 || s.Version.FWMinor != 3 {
			t.Errorf("第 %d 帧固件主/次版本 = %d/%d，期望 3/3",
				i+1, s.Version.FWMajor, s.Version.FWMinor)
		}
		if s.Version.HWVersion != 1 {
			t.Errorf("第 %d 帧硬件版本 = %d，期望 1", i+1, s.Version.HWVersion)
		}
		if got := s.Version.Protocol(); got != "2.2" {
			t.Errorf("第 %d 帧协议版本 = %q，期望 %q", i+1, got, "2.2")
		}
		if !s.Version.Known {
			t.Errorf("第 %d 帧 Version.Known 应为 true", i+1)
		}
	}
}

func TestVersionZeroValueIsNotMisread(t *testing.T) {
	var v Version
	if got := v.Firmware(); got != "—" {
		t.Errorf("零值 Version.Firmware() = %q，期望 %q", got, "—")
	}
	if got := v.Protocol(); got != "—" {
		t.Errorf("零值 Version.Protocol() = %q，期望 %q", got, "—")
	}
}

// ---------------------------------------------------------------- 字段解码

// 用第 1 帧校对已知字段，防止改动协议解析时悄悄跑偏。
func TestParseOnlineFrame(t *testing.T) {
	s := mustParse(t, onlineFrames[0])

	if s.Mode != ModeOnline || !s.Online || s.Charging {
		t.Fatalf("模式 = %v online=%v charging=%v，期望市电已充满", s.Mode, s.Online, s.Charging)
	}
	if !almostEqual(s.BatteryVoltage, 16.395) {
		t.Errorf("电池电压 = %.3f，期望 16.395", s.BatteryVoltage)
	}
	if !almostEqual(s.InputVoltage, 12.226) {
		t.Errorf("输入电压 = %.3f，期望 12.226", s.InputVoltage)
	}
	want := [4]float64{4.062, 4.071, 4.067, 4.069}
	for i, w := range want {
		if !almostEqual(s.Cells[i], w) {
			t.Errorf("电芯 %d = %.3f，期望 %.3f", i+1, s.Cells[i], w)
		}
	}
	if got := s.CellDelta(); math.Abs(got-9) > 0.5 {
		t.Errorf("压差 = %.1f mV，期望 9 mV", got)
	}
	if s.ChargePercent != 97 {
		t.Errorf("电量 = %d%%，期望 97%%", s.ChargePercent)
	}
	if s.LoadPercent != 0 || !s.LoadValid {
		t.Errorf("负载 = %d%% valid=%v，期望 0%%/true", s.LoadPercent, s.LoadValid)
	}
}

// ---------------------------------------------------------------- 剩余运行时间

// 市电供电时设备不给续航值（[16-17] 被输入电压复用），只能用本地估算。
func TestRuntimeOnlineUsesLocalEstimateOnly(t *testing.T) {
	s := mustParse(t, onlineFrames[0])
	r := s.Runtime()

	if r.DeviceSec != -1 {
		t.Errorf("市电模式下设备续航应为 -1，实际 %d", r.DeviceSec)
	}
	wantW := s.InputVoltage * s.InputCurrent // 未充电，无需扣充电功率
	if math.Abs(s.LoadPowerW()-wantW) > 1e-9 {
		t.Errorf("负载功率 = %.4f W，期望 %.4f W", s.LoadPowerW(), wantW)
	}
	wantSec := int(PackCapacityWh * float64(s.ChargePercent) / 100.0 / wantW * 3600.0)
	if r.LocalSec != wantSec {
		t.Errorf("本地估算 = %d s，期望 %d s", r.LocalSec, wantSec)
	}
	if r.Usable() != r.LocalSec {
		t.Errorf("Usable() = %d，市电模式下应回退到本地估算 %d", r.Usable(), r.LocalSec)
	}
	if r.Source() != "本地估算" {
		t.Errorf("Source() = %q，期望 %q", r.Source(), "本地估算")
	}
}

// 电池供电时设备上报值应被优先采用，同时本地估算照常给出，两者并存。
func TestRuntimeBatteryPrefersDeviceValue(t *testing.T) {
	f := makeFrame(StatusOB)
	putBE(f, 16, 1800)  // 设备上报剩余 1800 s
	putBE(f, 18, 12000) // 稳压输出 12.0 V
	putBE(f, 22, 16395) // 电池组 16.395 V
	putBE(f, 24, 2000)  // 放电电流 2.0 A → 24 W
	f[31] = 40          // 负载 40%
	f[43] = 50          // 电量 50%

	s := mustParse(t, hex.EncodeToString(f))
	r := s.Runtime()

	if r.DeviceSec != 1800 {
		t.Errorf("设备上报续航 = %d s，期望 1800 s", r.DeviceSec)
	}
	wantSec := int(PackCapacityWh * 0.5 / 24.0 * 3600.0)
	if r.LocalSec != wantSec {
		t.Errorf("本地估算 = %d s，期望 %d s", r.LocalSec, wantSec)
	}
	if r.Usable() != 1800 {
		t.Errorf("Usable() = %d，应优先设备上报的 1800", r.Usable())
	}
	if r.Source() != "设备上报" {
		t.Errorf("Source() = %q，期望 %q", r.Source(), "设备上报")
	}
}

// 充电时的输入功率混着给电池充电的部分，估算续航必须把它扣掉，
// 否则会系统性低估（这个 bug 在旧实现里存在）。
func TestLoadPowerSubtractsChargingPower(t *testing.T) {
	f := makeFrame(StatusOLCHRG)
	putBE(f, 18, 12400) // 输入 12.4 V
	putBE(f, 22, 16400) // 电池组 16.4 V
	putBE(f, 24, 3000)  // 输入电流 3.0 A → 37.2 W
	putBE(f, 29, 2000)  // 充电电流 2000 mA → 16.4V × 2A = 32.8 W
	f[43] = 50

	s := mustParse(t, hex.EncodeToString(f))
	if !s.Charging {
		t.Fatal("应为充电中状态")
	}
	want := 12.4*3.0 - 16.4*2.0 // = 4.4 W
	if math.Abs(s.LoadPowerW()-want) > 1e-6 {
		t.Errorf("负载功率 = %.4f W，期望 %.4f W（输入功率需扣除充电功率）", s.LoadPowerW(), want)
	}
}

// 充电功率大于输入功率（数据抖动/低速充电）时不能算出负功率。
func TestLoadPowerNeverNegative(t *testing.T) {
	f := makeFrame(StatusOLCHRG)
	putBE(f, 18, 12400)
	putBE(f, 22, 16400)
	putBE(f, 24, 500)  // 输入仅 6.2 W
	putBE(f, 29, 2000) // 充电 32.8 W（明显大于输入）
	f[43] = 50

	s := mustParse(t, hex.EncodeToString(f))
	if got := s.LoadPowerW(); got < 0 {
		t.Errorf("负载功率 = %.4f W，不应为负", got)
	}
	if got := s.Runtime().LocalSec; got != -1 {
		t.Errorf("负载过低时本地估算应为 -1，实际 %d", got)
	}
}

// 负载过低、电量为 0 等边界下不应给出误导性的正数结果。
func TestRuntimeUnavailableCases(t *testing.T) {
	f := makeFrame(StatusOL)
	f[43] = 0 // 电量为 0
	s := mustParse(t, hex.EncodeToString(f))
	if got := s.Runtime().LocalSec; got != -1 {
		t.Errorf("电量为 0 时本地估算应为 -1，实际 %d", got)
	}

	f2 := makeFrame(StatusOL)
	putBE(f2, 18, 12400)
	putBE(f2, 24, 0) // 无电流
	f2[43] = 90
	s2 := mustParse(t, hex.EncodeToString(f2))
	if got := s2.Runtime().LocalSec; got != -1 {
		t.Errorf("负载过低时本地估算应为 -1，实际 %d", got)
	}
	if got, _ := s2.RuntimeEstimate(); got != -1 {
		t.Errorf("RuntimeEstimate() 秒数应为 -1，实际 %d", got)
	}
}

// ---------------------------------------------------------------- 健壮性

func TestParseRejectsBadInput(t *testing.T) {
	if _, err := Parse(make([]byte, MinLen-1)); err == nil {
		t.Error("长度不足应报错")
	}
	b := make([]byte, FrameLen)
	b[0] = 0x01 // 错误的 Report ID
	if _, err := Parse(b); err == nil {
		t.Error("Report ID 不匹配应报错")
	}
}

// 未知状态字节不应导致解析失败——仍要能给出可用读数与帧头版本。
func TestParseUnknownStatusStillReportsVersion(t *testing.T) {
	f := makeFrame(0x99)
	putBE(f, 18, 12400)
	putBE(f, 24, 1000)
	f[43] = 80

	s := mustParse(t, hex.EncodeToString(f))
	if s.Mode != ModeUnknown {
		t.Errorf("模式 = %v，期望 ModeUnknown", s.Mode)
	}
	if got := s.Version.Firmware(); got != "V3.3" {
		t.Errorf("未知状态下固件版本 = %q，期望 V3.3", got)
	}
}

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
