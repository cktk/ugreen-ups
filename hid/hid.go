//go:build windows

// Package hid 提供 Windows 平台纯 Go 的 USB HID 访问能力（无 cgo、无第三方依赖）。
// 直接调用 setupapi.dll / hid.dll / kernel32.dll。
package hid

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// ---------------------------------------------------------------- 结构体

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// HID 设备接口类 GUID: {4D1E55B2-F16F-11CF-88CB-001111000030}
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

// Attributes 对应 HIDD_ATTRIBUTES
type Attributes struct {
	Size          uint32
	VendorID      uint16
	ProductID     uint16
	VersionNumber uint16
}

// Caps 对应 HIDP_CAPS
type Caps struct {
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

type valueCaps struct {
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
	U                 [8]uint16
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
	procHidPGetUsages          = hidDLL.NewProc("HidP_GetUsages")
	procHidPGetUsageValue      = hidDLL.NewProc("HidP_GetUsageValue")
	procHidPGetUsageValueArray = hidDLL.NewProc("HidP_GetUsageValueArray")
	procHidDGetManufacturerStr = hidDLL.NewProc("HidD_GetManufacturerString")
	procHidDGetProductString   = hidDLL.NewProc("HidD_GetProductString")
	procHidDGetSerialNumberStr = hidDLL.NewProc("HidD_GetSerialNumberString")
	procHidDGetInputReport     = hidDLL.NewProc("HidD_GetInputReport")
	procHidDGetFeature         = hidDLL.NewProc("HidD_GetFeature")
	procHidDSetFeature         = hidDLL.NewProc("HidD_SetFeature")

	procCreateFileW         = kernel32.NewProc("CreateFileW")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
	procReadFile            = kernel32.NewProc("ReadFile")
	procWriteFile           = kernel32.NewProc("WriteFile")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procCancelIo            = kernel32.NewProc("CancelIo")
)

const (
	digcfPresent         = 0x00000002
	digcfDeviceInterface = 0x00000010

	genericRead        = 0x80000000
	genericWrite       = 0x40000000
	fileShareRead      = 0x00000001
	fileShareWrite     = 0x00000002
	openExisting       = 3
	fileFlagOverlapped = 0x40000000

	waitTimeout    = 0x00000102
	errorIOPending = 997

	HidPInput   = 0
	HidPOutput  = 1
	HidPFeature = 2
)

func ntSuccess(status uintptr) bool { return int32(status) >= 0 }

// ---------------------------------------------------------------- 设备

// Device 表示一个已打开的 HID 接口（Top-Level Collection）。
type Device struct {
	Path         string
	VendorID     uint16
	ProductID    uint16
	Version      uint16
	Manufacturer string
	Product      string
	Serial       string
	Caps         Caps

	handle    syscall.Handle
	preparsed uintptr
	writeable bool
}

// Info 描述一个未打开的 HID 接口。
type Info struct {
	Path      string
	VendorID  uint16
	ProductID uint16
	Version   uint16
	UsagePage uint16
	Usage     uint16
	InputLen  uint16
}

func utf16ToString(buf []uint16) string {
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return syscall.UTF16ToString(buf[:n])
}

// Enumerate 返回系统中所有 HID 接口的路径。
func Enumerate() ([]string, error) {
	hdev, _, err := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&hidClassGUID)), 0, 0,
		uintptr(digcfPresent|digcfDeviceInterface))
	if hdev == uintptr(^uintptr(0)) {
		return nil, fmt.Errorf("SetupDiGetClassDevsW: %v", err)
	}
	defer procSetupDiDestroyDeviceInfoList.Call(hdev)

	var probe spDeviceInterfaceDetailData
	pathOffset := unsafe.Offsetof(probe.devicePath)

	var paths []string
	for i := uint32(0); ; i++ {
		var ifData spDeviceInterfaceData
		ifData.cbSize = uint32(unsafe.Sizeof(ifData))
		ret, _, _ := procSetupDiEnumDeviceInterfaces.Call(hdev, 0,
			uintptr(unsafe.Pointer(&hidClassGUID)), uintptr(i),
			uintptr(unsafe.Pointer(&ifData)))
		if ret == 0 {
			break
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

// List 枚举并识别所有 HID 接口（会短暂打开设备以读取属性）。
func List() ([]Info, error) {
	paths, err := Enumerate()
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, p := range paths {
		d, err := OpenPath(p)
		if err != nil {
			continue
		}
		out = append(out, Info{
			Path: p, VendorID: d.VendorID, ProductID: d.ProductID,
			Version: d.Version, UsagePage: d.Caps.UsagePage, Usage: d.Caps.Usage,
			InputLen: d.Caps.InputReportByteLength,
		})
		d.Close()
	}
	return out, nil
}

func open(path string) (syscall.Handle, bool, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, false, err
	}
	tryOpen := func(access uint32) (uintptr, error) {
		h, _, err := procCreateFileW.Call(
			uintptr(unsafe.Pointer(p)), uintptr(access),
			uintptr(fileShareRead|fileShareWrite), 0,
			uintptr(openExisting), uintptr(fileFlagOverlapped), 0)
		if h == uintptr(^uintptr(0)) || h == 0 {
			return 0, err
		}
		return h, nil
	}
	h, err := tryOpen(genericRead | genericWrite)
	if err == nil {
		return syscall.Handle(h), true, nil
	}
	h, err = tryOpen(genericRead)
	if err == nil {
		return syscall.Handle(h), false, nil
	}
	return 0, false, fmt.Errorf("CreateFileW: %v", err)
}

// OpenPath 打开指定路径的 HID 接口。
func OpenPath(path string) (*Device, error) {
	h, writeable, err := open(path)
	if err != nil {
		return nil, err
	}
	d := &Device{Path: path, handle: h, writeable: writeable}

	var attr Attributes
	attr.Size = uint32(unsafe.Sizeof(attr))
	if ret, _, _ := procHidDGetAttributes.Call(uintptr(h), uintptr(unsafe.Pointer(&attr))); ret != 0 {
		d.VendorID = attr.VendorID
		d.ProductID = attr.ProductID
		d.Version = attr.VersionNumber
	}

	if ret, _, _ := procHidDGetPreparsedData.Call(uintptr(h), uintptr(unsafe.Pointer(&d.preparsed))); ret != 0 && d.preparsed != 0 {
		procHidPGetCaps.Call(d.preparsed, uintptr(unsafe.Pointer(&d.Caps)))
	}

	d.Manufacturer = d.getString(procHidDGetManufacturerStr)
	d.Product = d.getString(procHidDGetProductString)
	d.Serial = d.getString(procHidDGetSerialNumberStr)
	return d, nil
}

// OpenFirst 打开第一个满足条件的 HID 接口。
func OpenFirst(match func(Info) bool) (*Device, error) {
	infos, err := List()
	if err != nil {
		return nil, err
	}
	for _, i := range infos {
		if match(i) {
			return OpenPath(i.Path)
		}
	}
	return nil, fmt.Errorf("未找到匹配的设备")
}

const (
	UgreenVID       = 0x2B89
	UgreenPID       = 0xFFFF
	PageVendor      = 0xFF00 // 绿联私有遥测
	PagePowerDevice = 0x0084 // 标准 HID PDC
)

// OpenVendor 打开绿联私有遥测接口（UsagePage 0xFF00）。
func OpenVendor() (*Device, error) {
	paths, err := Enumerate()
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		d, err := OpenPath(p)
		if err != nil {
			continue
		}
		if d.VendorID == UgreenVID && d.Caps.UsagePage == PageVendor {
			return d, nil
		}
		d.Close()
	}
	return nil, fmt.Errorf("未找到绿联 UPS 私有接口 (VID:%04X, Page 0x%04X)", UgreenVID, PageVendor)
}

// OpenPDC 打开标准 HID Power Device 接口（UsagePage 0x84）。
func OpenPDC() (*Device, error) {
	paths, err := Enumerate()
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		d, err := OpenPath(p)
		if err != nil {
			continue
		}
		if d.VendorID == UgreenVID && d.Caps.UsagePage == PagePowerDevice {
			return d, nil
		}
		d.Close()
	}
	return nil, fmt.Errorf("未找到标准 UPS 接口 (Page 0x%04X)", PagePowerDevice)
}

func (d *Device) getString(proc *syscall.LazyProc) string {
	buf := make([]uint16, 256)
	ret, _, _ := proc.Call(uintptr(d.handle), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2))
	if ret == 0 {
		return ""
	}
	return utf16ToString(buf)
}

// Close 释放设备句柄。
func (d *Device) Close() {
	if d.preparsed != 0 {
		procHidDFreePreparsedData.Call(d.preparsed)
		d.preparsed = 0
	}
	if d.handle != 0 {
		procCloseHandle.Call(uintptr(d.handle))
		d.handle = 0
	}
}

// Read 读取一个输入报告（带超时，毫秒）。
func (d *Device) Read(timeoutMs uint32) ([]byte, error) {
	n := int(d.Caps.InputReportByteLength)
	if n == 0 {
		n = 64
	}
	buf := make([]byte, n)
	got, err := d.readInto(buf, timeoutMs)
	if err != nil {
		return nil, err
	}
	return buf[:got], nil
}

func (d *Device) readInto(buf []byte, timeoutMs uint32) (int, error) {
	ev, _, err := procCreateEventW.Call(0, 1, 0, 0)
	if ev == 0 {
		return 0, fmt.Errorf("CreateEventW: %v", err)
	}
	defer procCloseHandle.Call(ev)

	var ov overlapped
	ov.HEvent = syscall.Handle(ev)

	var read uint32
	ret, _, errno := procReadFile.Call(uintptr(d.handle),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&ov)))
	if ret != 0 {
		return int(read), nil
	}
	e, ok := errno.(syscall.Errno)
	if !ok || e != errorIOPending {
		return 0, fmt.Errorf("ReadFile: %v", errno)
	}
	st, _, _ := procWaitForSingleObject.Call(ev, uintptr(timeoutMs))
	if uint32(st) == waitTimeout {
		procCancelIo.Call(uintptr(d.handle))
		return 0, fmt.Errorf("读取超时 %dms", timeoutMs)
	}
	var transferred uint32
	r2, _, err2 := procGetOverlappedResult.Call(uintptr(d.handle),
		uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&transferred)), 1)
	if r2 == 0 {
		procCancelIo.Call(uintptr(d.handle))
		return 0, fmt.Errorf("GetOverlappedResult: %v", err2)
	}
	return int(transferred), nil
}

// Write 发送一个输出报告。
func (d *Device) Write(data []byte) error {
	if !d.writeable {
		return fmt.Errorf("设备句柄不可写")
	}
	ev, _, err := procCreateEventW.Call(0, 1, 0, 0)
	if ev == 0 {
		return fmt.Errorf("CreateEventW: %v", err)
	}
	defer procCloseHandle.Call(ev)

	var ov overlapped
	ov.HEvent = syscall.Handle(ev)
	var written uint32
	ret, _, errno := procWriteFile.Call(uintptr(d.handle),
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)),
		uintptr(unsafe.Pointer(&written)), uintptr(unsafe.Pointer(&ov)))
	if ret != 0 {
		return nil
	}
	e, ok := errno.(syscall.Errno)
	if !ok || e != errorIOPending {
		return fmt.Errorf("WriteFile: %v", errno)
	}
	st, _, _ := procWaitForSingleObject.Call(ev, 3000)
	if uint32(st) == waitTimeout {
		procCancelIo.Call(uintptr(d.handle))
		return fmt.Errorf("写入超时")
	}
	return nil
}

// GetFeature 读取指定 ID 的 Feature 报告。
func (d *Device) GetFeature(reportID byte, length int) ([]byte, error) {
	if length == 0 {
		length = int(d.Caps.FeatureReportByteLength)
	}
	if length == 0 {
		length = 64
	}
	buf := make([]byte, length)
	buf[0] = reportID
	ret, _, err := procHidDGetFeature.Call(uintptr(d.handle),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(length))
	if ret == 0 {
		return nil, fmt.Errorf("HidD_GetFeature(ID=0x%02X): %v", reportID, err)
	}
	return buf, nil
}

// GetInputReport 通过控制端点同步获取输入报告。
func (d *Device) GetInputReport(reportID byte, length int) ([]byte, error) {
	if length == 0 {
		length = int(d.Caps.InputReportByteLength)
	}
	buf := make([]byte, length)
	buf[0] = reportID
	ret, _, err := procHidDGetInputReport.Call(uintptr(d.handle),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(length))
	if ret == 0 {
		return nil, fmt.Errorf("HidD_GetInputReport(ID=0x%02X): %v", reportID, err)
	}
	return buf, nil
}

// UsageValue 从报告数据中按 Usage 提取数值（由报告描述符定位位偏移）。
func (d *Device) UsageValue(reportType int, page, usage uint16, report []byte) (uint32, bool) {
	if d.preparsed == 0 {
		return 0, false
	}
	var v uint32
	status, _, _ := procHidPGetUsageValue.Call(
		uintptr(reportType), uintptr(page), 0, uintptr(usage),
		uintptr(unsafe.Pointer(&v)), d.preparsed,
		uintptr(unsafe.Pointer(&report[0])), uintptr(len(report)))
	if !ntSuccess(status) {
		return 0, false
	}
	return v, true
}

// Usages 提取报告中处于按下状态的 Usage（用于位域型状态字段）。
func (d *Device) Usages(reportType int, page uint16, report []byte) ([]uint16, error) {
	if d.preparsed == 0 {
		return nil, fmt.Errorf("无报告描述符")
	}
	out := make([]uint16, 64)
	var count uint32 = uint32(len(out))
	status, _, _ := procHidPGetUsages.Call(
		uintptr(reportType), uintptr(page), 0,
		uintptr(unsafe.Pointer(&out[0])), uintptr(unsafe.Pointer(&count)),
		d.preparsed, uintptr(unsafe.Pointer(&report[0])), uintptr(len(report)))
	if !ntSuccess(status) {
		return nil, fmt.Errorf("HidP_GetUsages 状态 0x%X", uint32(status))
	}
	return out[:count], nil
}

// ValueCaps 返回指定报告类型的用法能力列表。
func (d *Device) ValueCaps(reportType int) [][3]uint16 {
	if d.preparsed == 0 {
		return nil
	}
	var n int
	switch reportType {
	case HidPInput:
		n = int(d.Caps.NumberInputValueCaps)
	case HidPOutput:
		n = int(d.Caps.NumberOutputValueCaps)
	case HidPFeature:
		n = int(d.Caps.NumberFeatureValueCaps)
	}
	if n <= 0 || n > 512 {
		return nil
	}
	arr := make([]valueCaps, n)
	length := uint32(n)
	status, _, _ := procHidPGetValueCaps.Call(uintptr(reportType),
		uintptr(unsafe.Pointer(&arr[0])), uintptr(unsafe.Pointer(&length)), d.preparsed)
	if !ntSuccess(status) {
		return nil
	}
	out := make([][3]uint16, 0, length)
	for i := 0; i < int(length); i++ {
		out = append(out, [3]uint16{uint16(arr[i].ReportID), arr[i].UsagePage, arr[i].U[0]})
	}
	return out
}

// Describe 生成设备的可读摘要。
func (d *Device) Describe() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s (VID:%04X PID:%04X 版本 %d.%02d)",
		d.Manufacturer, d.Product, d.VendorID, d.ProductID,
		d.Version>>8, d.Version&0xFF)
	if d.Serial != "" {
		fmt.Fprintf(&sb, " SN:%s", d.Serial)
	}
	return sb.String()
}
