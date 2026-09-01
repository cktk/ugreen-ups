// Package protocol 解析绿联 UGREEN US3000 UPS 的私有 HID 遥测帧。
//
// 设备使用厂商自定义 UsagePage 0xFF00、Report ID 0x71 的 64 字节输入报告，
// 约每 1 秒主动上报一次。字节映射依据固件 V3.3 的实测与公开逆向资料整理，
// 偏移均包含开头的 Report ID 字节（即 data[0] == 0x71）。
//
// 该设备为直流 UPS：输入来自外置电源适配器（12V/19V/20V DC），输出 12V DC。
package protocol

import (
	"encoding/hex"
	"fmt"
	"math"
	"time"
)

const (
	ReportID       = 0x71
	FrameLen       = 64
	MinLen         = 47 // 有效字段至少到 byte[46]
	CellCount      = 4
	PackCapacityWh = 43.0 // 3000mAh x 14.4V（4S 锂电，见拆解数据）
)

// 状态字节 data[7] 的取值
const (
	StatusOL     byte = 0x26 // 市电供电，电池已充满
	StatusOLCHRG byte = 0x36 // 市电供电，电池充电中
	StatusOB     byte = 0x21 // 电池供电（市电中断）
)

// Mode 表示 UPS 工作模式
type Mode int

const (
	ModeUnknown Mode = iota
	ModeOnline       // OL：市电供电
	ModeBattery      // OB：电池供电
)

// Sample 是一帧遥测数据的解析结果
type Sample struct {
	Time time.Time

	RawStatus byte   // 原始状态字节
	Mode      Mode   // 工作模式
	Online    bool   // 市电是否正常
	Charging  bool   // 是否充电中
	Status    string // 人类可读状态

	InputVoltage1 float64 // [16-17] DC 输入第一测量点 (V)
	InputVoltage  float64 // [18-19] DC 输入 (V)；OB 模式下为输出电压
	OutputVoltage float64 // OB 模式下的稳压输出 (V)，OL 模式为 0

	BatteryVoltage float64            // [22-23] 电池组总电压 (V)
	Cells          [CellCount]float64 // [35-42] 4 节电芯电压 (V)

	InputCurrent   float64 // [24-25] 输入电流 (A)；OB 模式下为电池放电电流
	BatteryCurrent float64 // OB 模式下的电池电流 (A)
	ChargeCurrent  float64 // [29-30] 充电电流 (mA)，仅 OL CHRG 有效

	LoadPercent int  // 负载百分比
	LoadValid   bool // 负载值是否可信（OL 模式需 [29]==0）

	ChargePercent int // [43] 电池电量百分比
	RuntimeSec    int // OB 模式下 [16-17] 的剩余时间估计（秒），<0 表示不可用

	Raw []byte
}

// beU16 大端无符号 16 位
func beU16(b []byte, i int) uint16 {
	return uint16(b[i])<<8 | uint16(b[i+1])
}

// Parse 解析一帧 64 字节输入报告。
func Parse(data []byte) (*Sample, error) {
	if len(data) < MinLen {
		return nil, fmt.Errorf("帧长度不足：需要至少 %d 字节，实际 %d", MinLen, len(data))
	}
	if data[0] != ReportID {
		return nil, fmt.Errorf("报告 ID 不匹配：期望 0x%02X，实际 0x%02X", ReportID, data[0])
	}

	s := &Sample{
		Time:           time.Now(),
		RawStatus:      data[7],
		InputVoltage1:  float64(beU16(data, 16)) / 1000.0,
		BatteryVoltage: float64(beU16(data, 22)) / 1000.0,
		ChargeCurrent:  float64(beU16(data, 29)),
		ChargePercent:  int(data[43]),
		RuntimeSec:     -1,
		Raw:            data,
	}
	for i := 0; i < CellCount; i++ {
		s.Cells[i] = float64(beU16(data, 35+i*2)) / 1000.0
	}

	switch data[7] {
	case StatusOL:
		s.Mode, s.Online, s.Charging = ModeOnline, true, false
		s.Status = "市电供电 · 电池已充满"
		s.InputVoltage = float64(beU16(data, 18)) / 1000.0
		s.InputCurrent = float64(beU16(data, 24)) / 1000.0
		// OL 模式：[30] 仅在 [29]==0 时才是负载百分比
		if data[29] == 0 {
			s.LoadPercent = int(data[30])
			s.LoadValid = true
		}
	case StatusOLCHRG:
		s.Mode, s.Online, s.Charging = ModeOnline, true, true
		s.Status = "市电供电 · 电池充电中"
		s.InputVoltage = float64(beU16(data, 18)) / 1000.0
		s.InputCurrent = float64(beU16(data, 24)) / 1000.0
		if data[29] == 0 && data[30] <= 100 {
			s.LoadPercent = int(data[30])
			s.LoadValid = true
		}
	case StatusOB:
		s.Mode, s.Online, s.Charging = ModeBattery, false, false
		s.Status = "电池供电 · 市电中断"
		s.OutputVoltage = float64(beU16(data, 18)) / 1000.0
		s.InputVoltage = s.OutputVoltage
		s.BatteryCurrent = float64(beU16(data, 24)) / 1000.0
		s.InputCurrent = s.BatteryCurrent
		s.RuntimeSec = int(beU16(data, 16))
		s.LoadPercent = int(data[31])
		s.LoadValid = true
	default:
		s.Mode = ModeUnknown
		s.Status = fmt.Sprintf("未知状态 (0x%02X)", data[7])
		// 未知模式下按最通用的方式解析，保证仍有可用读数
		s.InputVoltage = float64(beU16(data, 18)) / 1000.0
		s.InputCurrent = float64(beU16(data, 24)) / 1000.0
		if data[29] == 0 && data[30] <= 100 {
			s.LoadPercent = int(data[30])
			s.LoadValid = true
		}
	}
	return s, nil
}

// InputPower 输入/输出功率估算（W）
func (s *Sample) InputPower() float64 {
	v := s.InputVoltage
	if s.Mode == ModeBattery {
		v = s.OutputVoltage
	}
	return v * s.InputCurrent
}

// CellDelta 电芯压差（mV）
func (s *Sample) CellDelta() float64 {
	min, max := math.MaxFloat64, -math.MaxFloat64
	for _, v := range s.Cells {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return (max - min) * 1000.0
}

// CellSum 电芯电压之和（V）
func (s *Sample) CellSum() float64 {
	var sum float64
	for _, v := range s.Cells {
		sum += v
	}
	return sum
}

// RuntimeEstimate 基于电量与当前功率估算的剩余运行时间（秒）。
// 电池供电模式下优先采用设备上报值；市电模式下按当前功率折算。
func (s *Sample) RuntimeEstimate() (int, string) {
	if s.Mode == ModeBattery {
		if s.RuntimeSec > 0 {
			return s.RuntimeSec, "设备上报"
		}
		p := s.OutputVoltage * s.BatteryCurrent
		if p > 1 {
			return int(PackCapacityWh * float64(s.ChargePercent) / 100.0 / p * 3600.0), "按当前功率估算"
		}
		return -1, "功率过低，无法估算"
	}
	p := s.InputVoltage * s.InputCurrent
	if p > 1 {
		return int(PackCapacityWh * float64(s.ChargePercent) / 100.0 / p * 3600.0), "按当前功率估算"
	}
	return -1, "功率过低，无法估算"
}

// Health 返回电池健康相关评估
func (s *Sample) Health() string {
	d := s.CellDelta()
	switch {
	case d <= 30:
		return "优秀（电芯高度均衡）"
	case d <= 80:
		return "良好"
	case d <= 150:
		return "一般（压差偏大）"
	default:
		return "需关注（压差过大，建议检查电池）"
	}
}

// StatusShort 返回紧凑状态标记，如 "OL" / "OL CHRG" / "OB"
func (s *Sample) StatusShort() string {
	switch s.RawStatus {
	case StatusOL:
		return "OL"
	case StatusOLCHRG:
		return "OL CHRG"
	case StatusOB:
		return "OB"
	}
	return fmt.Sprintf("0x%02X", s.RawStatus)
}

// RawHex 返回原始帧的十六进制表示
func (s *Sample) RawHex() string {
	return hex.EncodeToString(s.Raw)
}
