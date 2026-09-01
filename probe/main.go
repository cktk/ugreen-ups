//go:build windows

// probe - HID 协议探测器：枚举 USB HID 设备，读取绿联 UPS (VID:2B89 PID:FFFF) 的原始报告。
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ---------------------------------------------------------------- Windows GUID

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var hidClassGUID = guid{0x4D1E55B2, 0xF16F, 0x11CF, [8]byte{0x88, 0xCB, 0x00, 0x11, 0x11, 0x00, 0x00, 0x30}}

type spDevInfoData struct {
	cbSize    uint32
	classGUID guid
	devInst   uint32
	reserved  uintptr
}

type spDeviceInterfaceData struct {
	cbSize             uint32
	interfaceClassGUID guid
	flags              uint32
	reserved           uintptr
}

type spDeviceInterfaceDetailData struct {
	cbSize     uint32
	devicePath [1]uint16
}

// ---------------------------------------------------------------- HID 结构

type hidDAttributes struct {
	Size          uint32
	VendorID      uint16
	ProductID     uint16
	VersionNumber uint16
}

type hidPCaps struct {
	Usage                     uint16
	UsagePage                 uint16
	InputReportByteLength     uint16
	OutputReportByteLength    uint16
	FeatureReportByteLength   uint16
	Reserved                  [17]uint16
	NumberLinkCollectionNodes uint16
	NumberInputButtonCaps     uint16
	NumberInputValueCaps      uint16
	NumberInputDataIndices    uint16
	NumberOutputButtonCaps    uint16
	NumberOutputValueCaps     uint16
	NumberOutputDataIndices   uint16
	NumberFeatureButtonCaps   uint16
	NumberFeatureValueCaps    uint16
	NumberFeatureDataIndices  uint16
}

type hidPValueCaps struct {
	UsagePage         uint16
	ReportID          byte
	IsAlias           byte
	BitField          uint16
	LinkCollection    uint16
	LinkUsage         uint16
	LinkUsagePage     uint16
	IsRange           byte
	IsStringRange     byte
	IsDesignatorRange byte
	IsAbsolute        byte
	HasNull           byte
	Reserved          byte
	BitSize           uint16
	ReportCount       uint16
	Reserved2         [5]uint16
	UnitsExp          uint32
	Units             uint32
	LogicalMin        int32
	LogicalMax        int32
	PhysicalMin       int32
	PhysicalMax       int32
	U                 [8]uint16 // union: Range / NotRange
}

type overlapped struct {
	Internal     uintptr
	InternalHigh uintptr
	Offset       uint32
	OffsetHigh   uint32
	HEvent       syscall.Handle
}

// ---------------------------------------------------------------- DLL

var (
	setupapi = syscall.NewLazyDLL("setupapi.dll")
	hidDLL   = syscall.NewLazyDLL("hid.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procSetupDiGetClassDevsW             = setupapi.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces      = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetailW = setupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList     = setupapi.NewProc("SetupDiDestroyDeviceInfoList")

	procHidDGetAttributes      = hidDLL.NewProc("HidD_GetAttributes")
	procHidDGetPreparsedData   = hidDLL.NewProc("HidD_GetPreparsedData")
	procHidDFreePreparsedData  = hidDLL.NewProc("HidD_FreePreparsedData")
	procHidPGetCaps            = hidDLL.NewProc("HidP_GetCaps")
	procHidPGetValueCaps       = hidDLL.NewProc("HidP_GetValueCaps")
	procHidDGetManufacturerStr = hidDLL.NewProc("HidD_GetManufacturerString")
	procHidDGetProductString   = hidDLL.NewProc("HidD_GetProductString")
	procHidDGetSerialNumberStr = hidDLL.NewProc("HidD_GetSerialNumberString")
	procHidDGetInputReport     = hidDLL.NewProc("HidD_GetInputReport")
	procHidDGetFeature         = hidDLL.NewProc("HidD_GetFeature")

	procCreateFileW         = kernel32.NewProc("CreateFileW")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
	procReadFile            = kernel32.NewProc("ReadFile")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procCancelIo            = kernel32.NewProc("CancelIo")
)

const (
	digcfPresent         = 0x00000002
	digcfDeviceInterface = 0x00000010

	genericRead       = 0x80000000
	genericWrite      = 0x40000000
	fileShareRead     = 0x00000001
	fileShareWrite    = 0x00000002
	openExisting      = 3
	fileFlagOverlapped = 0x40000000

	waitTimeout   = 0x00000102
	errorIOPending = 997

	hidpInput   = 0
	hidpOutput  = 1
	hidpFeature = 2
)

func ntSuccess(status uintptr) bool { return int32(status) >= 0 }

// ---------------------------------------------------------------- 枚举

func utf16ToString(buf []uint16) string {
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return syscall.UTF16ToString(buf[:n])
}

func enumHIDDevices() ([]string, error) {
	hdev, _, err := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&hidClassGUID)), 0, 0,
		uintptr(digcfPresent|digcfDeviceInterface))
	if hdev == uintptr(^uintptr(0)) {
		return nil, fmt.Errorf("SetupDiGetClassDevsW: %v", err)
	}
	defer procSetupDiDestroyDeviceInfoList.Call(hdev)

	var detail spDeviceInterfaceDetailData
	pathOffset := unsafe.Offsetof(detail.devicePath)

	var paths []string
	for i := uint32(0); ; i++ {
		var ifData spDeviceInterfaceData
		ifData.cbSize = uint32(unsafe.Sizeof(ifData))
		ret, _, _ := procSetupDiEnumDeviceInterfaces.Call(hdev, 0,
			uintptr(unsafe.Pointer(&hidClassGUID)), uintptr(i),
			uintptr(unsafe.Pointer(&ifData)))
		if ret == 0 {
			break // 结束或错误
		}

		var required uint32
		var devInfo spDevInfoData
		devInfo.cbSize = uint32(unsafe.Sizeof(devInfo))
		procSetupDiGetDeviceInterfaceDetailW.Call(hdev,
			uintptr(unsafe.Pointer(&ifData)), 0, 0,
			uintptr(unsafe.Pointer(&required)),
			uintptr(unsafe.Pointer(&devInfo)))
		if required == 0 {
			continue
		}

		buf := make([]byte, int(required))
		d := (*spDeviceInterfaceDetailData)(unsafe.Pointer(&buf[0]))
		if unsafe.Sizeof(uintptr(0)) == 8 {
			d.cbSize = 8
		} else {
			d.cbSize = 6
		}
		ret, _, _ = procSetupDiGetDeviceInterfaceDetailW.Call(hdev,
			uintptr(unsafe.Pointer(&ifData)),
			uintptr(unsafe.Pointer(d)), uintptr(required),
			uintptr(unsafe.Pointer(&required)),
			uintptr(unsafe.Pointer(&devInfo)))
		if ret == 0 {
			continue
		}
		u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[pathOffset])), (len(buf)-int(pathOffset))/2)
		paths = append(paths, utf16ToString(u16))
	}
	return paths, nil
}

func openHID(path string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	open := func(access uint32) (uintptr, error) {
		h, _, err := procCreateFileW.Call(
			uintptr(unsafe.Pointer(p)), uintptr(access),
			uintptr(fileShareRead|fileShareWrite), 0,
			uintptr(openExisting), uintptr(fileFlagOverlapped), 0)
		if h == uintptr(^uintptr(0)) || h == 0 {
			return 0, err
		}
		return h, nil
	}
	h, err := open(genericRead | genericWrite)
	if err != nil {
		h, err = open(genericRead)
		if err != nil {
			return 0, fmt.Errorf("CreateFileW 失败: %v", err)
		}
	}
	return syscall.Handle(h), nil
}

func getAttributes(h syscall.Handle) (*hidDAttributes, error) {
	var attr hidDAttributes
	attr.Size = uint32(unsafe.Sizeof(attr))
	ret, _, err := procHidDGetAttributes.Call(uintptr(h), uintptr(unsafe.Pointer(&attr)))
	if ret == 0 {
		return nil, err
	}
	return &attr, nil
}

func getCaps(h syscall.Handle) (*hidPCaps, uintptr, error) {
	var preparsed uintptr
	ret, _, err := procHidDGetPreparsedData.Call(uintptr(h), uintptr(unsafe.Pointer(&preparsed)))
	if ret == 0 || preparsed == 0 {
		return nil, 0, err
	}
	var caps hidPCaps
	status, _, _ := procHidPGetCaps.Call(preparsed, uintptr(unsafe.Pointer(&caps)))
	if !ntSuccess(status) {
		procHidDFreePreparsedData.Call(preparsed)
		return nil, 0, fmt.Errorf("HidP_GetCaps 状态 0x%X", uint32(status))
	}
	return &caps, preparsed, nil
}

func getValueCaps(preparsed uintptr, reportType int, count int) []hidPValueCaps {
	if count <= 0 || count > 512 {
		return nil
	}
	arr := make([]hidPValueCaps, count)
	length := uint32(count)
	status, _, _ := procHidPGetValueCaps.Call(uintptr(reportType),
		uintptr(unsafe.Pointer(&arr[0])), uintptr(unsafe.Pointer(&length)), preparsed)
	if !ntSuccess(status) {
		return nil
	}
	return arr[:length]
}

func getString(h syscall.Handle, proc *syscall.LazyProc) string {
	buf := make([]uint16, 256)
	ret, _, _ := proc.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2))
	if ret == 0 {
		return ""
	}
	return utf16ToString(buf)
}

func readReportOverlapped(h syscall.Handle, buf []byte, timeoutMs uint32) (int, error) {
	ev, _, err := procCreateEventW.Call(0, 1, 0, 0)
	if ev == 0 {
		return 0, fmt.Errorf("CreateEventW: %v", err)
	}
	defer procCloseHandle.Call(ev)

	var ov overlapped
	ov.HEvent = syscall.Handle(ev)

	var read uint32
	ret, _, errno := procReadFile.Call(uintptr(h),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&ov)))
	if ret == 0 {
		e, ok := errno.(syscall.Errno)
		if !ok || e != errorIOPending {
			return 0, fmt.Errorf("ReadFile: %v", errno)
		}
		st, _, _ := procWaitForSingleObject.Call(ev, uintptr(timeoutMs))
		if uint32(st) == waitTimeout {
			procCancelIo.Call(uintptr(h))
			return 0, fmt.Errorf("超时 %dms（无输入报告上报）", timeoutMs)
		}
		var transferred uint32
		r2, _, err2 := procGetOverlappedResult.Call(uintptr(h),
			uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&transferred)), 1)
		if r2 == 0 {
			procCancelIo.Call(uintptr(h))
			return 0, fmt.Errorf("GetOverlappedResult: %v", err2)
		}
		return int(transferred), nil
	}
	return int(read), nil
}

// ---------------------------------------------------------------- Usage 名称

var powerUsages = map[uint32]string{
	0x840001: "iName", 0x840002: "PresentStatus", 0x840003: "ChangedStatus",
	0x840004: "UPS", 0x840005: "PowerSupply", 0x840006: "Flow",
	0x840010: "Outlet", 0x840012: "Charger", 0x840016: "PowerConverter",
	0x840024: "FlowConfig", 0x84002C: "ConfigVoltage", 0x840030: "Voltage",
	0x840031: "Current", 0x840032: "Frequency", 0x840033: "ApparentPower(VA)",
	0x840034: "ActivePower(W)", 0x840035: "PercentLoad", 0x840036: "Temperature",
	0x840037: "Humidity", 0x840040: "ConfigDelay", 0x840041: "DelayBeforeStartup",
	0x840042: "DelayBeforeShutdown", 0x840043: "Test", 0x840044: "ModuleReset",
	0x840045: "AudibleAlarmControl", 0x840053: "LowVoltageTransfer",
	0x840054: "HighVoltageTransfer", 0x840056: "DelayBeforeReboot",
	0x840065: "Manufacturer", 0x840066: "Product", 0x840067: "SerialNumber",
	0x84008B: "Rechargeable", 0x84008D: "Capacity(%)",
	0x84008E: "RemainingCapacity", 0x84008F: "RunTimeToEmpty(s)",
	0x8400FD: "iManufacturer/iProduct/iSerial", 0x8400FE: "iVersionNumber",
	0x850008: "BatterySystem", 0x850030: "Voltage", 0x850031: "Current",
	0x850032: "Frequency", 0x850033: "ApparentPower", 0x850034: "ActivePower",
	0x850035: "PercentLoad", 0x850036: "Temperature", 0x850083: "CapacityMode",
	0x850085: "Capacity(%)", 0x850089: "AbsoluteStateOfCharge",
}

func usageName(page, usage uint16) string {
	if n, ok := powerUsages[uint32(page)<<16|uint32(usage)]; ok {
		return n
	}
	return ""
}

func bytesToDec(b []byte) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, v := range b {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "%3d", v)
	}
	sb.WriteByte(']')
	return sb.String()
}

// ---------------------------------------------------------------- 主流程

func main() {
	vidHex := flag.String("vid", "2B89", "目标 VID（十六进制）")
	samples := flag.Int("n", 8, "采样次数")
	interval := flag.Int("i", 1000, "采样间隔(ms)")
	showAll := flag.Bool("all", false, "列出所有 HID 设备")
	flag.Parse()

	var vid uint16
	fmt.Sscanf(*vidHex, "%04X", &vid)

	paths, err := enumHIDDevices()
	if err != nil {
		fmt.Fprintf(os.Stderr, "枚举失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("系统共枚举到 %d 个 HID 接口\n", len(paths))

	if *showAll {
		fmt.Println()
		for _, p := range paths {
			h, err := openHID(p)
			if err != nil {
				continue
			}
			attr, err1 := getAttributes(h)
			caps, pp, err2 := getCaps(h)
			if err1 == nil && err2 == nil {
				fmt.Printf("VID:%04X PID:%04X  Page:0x%04X Usage:0x%04X  输入=%dB\n    %s\n",
					attr.VendorID, attr.ProductID, caps.UsagePage, caps.Usage,
					caps.InputReportByteLength, p)
				procHidDFreePreparsedData.Call(pp)
			}
			procCloseHandle.Call(uintptr(h))
		}
		return
	}

	var targets []string
	for _, p := range paths {
		h, err := openHID(p)
		if err != nil {
			continue
		}
		if attr, err := getAttributes(h); err == nil && attr.VendorID == vid {
			targets = append(targets, p)
		}
		procCloseHandle.Call(uintptr(h))
	}

	if len(targets) == 0 {
		fmt.Printf("\n未找到 VID:%04X 的设备\n", vid)
		os.Exit(2)
	}
	fmt.Printf("命中 VID:%04X 的接口共 %d 个\n", vid, len(targets))

	for idx, path := range targets {
		fmt.Printf("\n%s\n", strings.Repeat("=", 78))
		fmt.Printf("[接口 %d/%d] %s\n", idx+1, len(targets), path)

		h, err := openHID(path)
		if err != nil {
			fmt.Printf("  打开失败: %v\n", err)
			continue
		}

		attr, err := getAttributes(h)
		caps, preparsed, err2 := getCaps(h)
		if err != nil || err2 != nil {
			fmt.Printf("  获取属性/Caps 失败: %v %v\n", err, err2)
			procCloseHandle.Call(uintptr(h))
			continue
		}

		fmt.Printf("  VID:PID   = %04X:%04X   版本 = %d.%02d\n",
			attr.VendorID, attr.ProductID, attr.VersionNumber>>8, attr.VersionNumber&0xFF)
		fmt.Printf("  厂商      = %q\n", getString(h, procHidDGetManufacturerStr))
		fmt.Printf("  产品      = %q\n", getString(h, procHidDGetProductString))
		fmt.Printf("  序列号    = %q\n", getString(h, procHidDGetSerialNumberStr))
		fmt.Printf("  UsagePage = 0x%04X   Usage = 0x%04X\n", caps.UsagePage, caps.Usage)
		fmt.Printf("  报告长度  = 输入 %d / 输出 %d / Feature %d\n",
			caps.InputReportByteLength, caps.OutputReportByteLength, caps.FeatureReportByteLength)

		for _, rt := range []struct {
			t int
			n string
			c int
		}{{hidpInput, "输入", int(caps.NumberInputValueCaps)},
			{hidpFeature, "Feature", int(caps.NumberFeatureValueCaps)},
			{hidpOutput, "输出", int(caps.NumberOutputValueCaps)}} {
			vcs := getValueCaps(preparsed, rt.t, rt.c)
			if len(vcs) == 0 {
				continue
			}
			fmt.Printf("  --- %s报告用法 (%d 项) ---\n", rt.n, len(vcs))
			for _, vc := range vcs {
				name := usageName(vc.UsagePage, vc.U[0])
				if vc.IsRange != 0 {
					fmt.Printf("      ID=0x%02X page=0x%04X usage=0x%04X..0x%04X bits=%d cnt=%d [%d..%d]\n",
						vc.ReportID, vc.UsagePage, vc.U[0], vc.U[1], vc.BitSize, vc.ReportCount,
						vc.LogicalMin, vc.LogicalMax)
					continue
				}
				fmt.Printf("      ID=0x%02X page=0x%04X usage=0x%04X bits=%d cnt=%d [%d..%d] %s\n",
					vc.ReportID, vc.UsagePage, vc.U[0], vc.BitSize, vc.ReportCount,
					vc.LogicalMin, vc.LogicalMax, name)
			}
		}

		rlen := int(caps.InputReportByteLength)
		if rlen == 0 {
			rlen = 64
		}
		fmt.Printf("  --- 输入报告采样 (%d 次, 间隔 %dms) ---\n", *samples, *interval)
		for i := 0; i < *samples; i++ {
			buf := make([]byte, rlen)
			n, err := readReportOverlapped(h, buf, 3000)
			if err != nil {
				fmt.Printf("      #%-2d %v\n", i+1, err)
			} else {
				fmt.Printf("      #%-2d [%2dB] %s\n", i+1, n, hex.EncodeToString(buf[:n]))
				fmt.Printf("          dec: %s\n", bytesToDec(buf[:n]))
			}
			if i < *samples-1 {
				time.Sleep(time.Duration(*interval) * time.Millisecond)
			}
		}

		if caps.FeatureReportByteLength > 0 {
			fmt.Printf("  --- Feature 报告探测 (长度 %d) ---\n", caps.FeatureReportByteLength)
			flen := int(caps.FeatureReportByteLength)
			for rid := 1; rid <= 32; rid++ {
				buf := make([]byte, flen)
				buf[0] = byte(rid)
				ret, _, _ := procHidDGetFeature.Call(uintptr(h),
					uintptr(unsafe.Pointer(&buf[0])), uintptr(flen))
				if ret != 0 {
					fmt.Printf("      Feature ID=0x%02X: %s\n          dec: %s\n",
						rid, hex.EncodeToString(buf), bytesToDec(buf))
				}
			}
		}

		procHidDFreePreparsedData.Call(preparsed)
		procCloseHandle.Call(uintptr(h))
	}
	fmt.Println()
}
