"""抓取 GUI 窗口截图（纯标准库：ctypes + zlib，无需第三方依赖）。

用途：在无人值守的环境里核对 lxn/walk 窗口的实际渲染效果。

用法（在仓库根目录下，程序已运行时）：
    python tools/uicheck/screenshot.py
输出：当前目录下的 ui_shot.png（24 位 RGB PNG）

注意：必须先声明 DPI 感知。否则本进程被 Windows 做 DPI 虚拟化，
GetWindowRect 返回虚拟坐标而 PrintWindow 按物理像素渲染，二者比例不一致会导致截图被裁切。
"""
import ctypes
import ctypes.wintypes as wt
import struct
import sys
import time
import zlib

user32 = ctypes.WinDLL("user32", use_last_error=True)
gdi32 = ctypes.WinDLL("gdi32", use_last_error=True)

TITLE = "UGREEN US3000 UPS Monitor"


class BITMAPINFOHEADER(ctypes.Structure):
    _fields_ = [
        ("biSize", wt.DWORD),
        ("biWidth", wt.LONG),
        ("biHeight", wt.LONG),
        ("biPlanes", wt.WORD),
        ("biBitCount", wt.WORD),
        ("biCompression", wt.DWORD),
        ("biSizeImage", wt.DWORD),
        ("biXPelsPerMeter", wt.LONG),
        ("biYPelsPerMeter", wt.LONG),
        ("biClrUsed", wt.DWORD),
        ("biClrImportant", wt.DWORD),
    ]


def write_png(path, w, h, rgb):
    rows = b"".join(b"\x00" + rgb[y * w * 3:(y + 1) * w * 3] for y in range(h))

    def chunk(tag, data):
        body = tag + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

    out = b"\x89PNG\r\n\x1a\n"
    out += chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
    out += chunk(b"IDAT", zlib.compress(rows, 6))
    out += chunk(b"IEND", b"")
    with open(path, "wb") as f:
        f.write(out)


def main():
    # 关键：进程必须先声明 DPI 感知，否则 GetWindowRect 返回的是虚拟化坐标，
    # 而 PrintWindow 按物理像素渲染，两者比例不一致会导致截图被裁切/放大。
    try:
        user32.SetProcessDpiAwarenessContext(ctypes.c_void_p(-4))  # PER_MONITOR_AWARE_V2
    except Exception:
        pass
    try:
        user32.SetProcessDPIAware()
    except Exception:
        pass

    hwnd = user32.FindWindowW(None, TITLE)
    if not hwnd:
        print("window not found")
        return 1
    print("hwnd", hwnd)

    try:
        print("dpi", user32.GetDpiForWindow(hwnd))
    except Exception:
        pass

    user32.ShowWindow(hwnd, 5)  # SW_SHOW
    user32.SetWindowPos(hwnd, 0, 20, 20, 0, 0, 0x0001 | 0x0004 | 0x0040)  # NOSIZE|NOZORDER|SHOWWINDOW
    time.sleep(1.0)

    rect = wt.RECT()
    user32.GetWindowRect(hwnd, ctypes.byref(rect))
    w = rect.right - rect.left
    h = rect.bottom - rect.top
    print("size", w, h)
    if w <= 0 or h <= 0:
        return 1

    hdc = user32.GetWindowDC(hwnd)
    mdc = gdi32.CreateCompatibleDC(hdc)
    bmp = gdi32.CreateCompatibleBitmap(hdc, w, h)
    old = gdi32.SelectObject(mdc, bmp)
    ok = user32.PrintWindow(hwnd, mdc, 2)  # PW_RENDERFULLCONTENT
    print("PrintWindow", ok)

    bih = BITMAPINFOHEADER()
    bih.biSize = ctypes.sizeof(BITMAPINFOHEADER)
    bih.biWidth = w
    bih.biHeight = -h  # 负数 = 自上而下
    bih.biPlanes = 1
    bih.biBitCount = 32
    bih.biCompression = 0

    buf = ctypes.create_string_buffer(w * h * 4)
    got = gdi32.GetDIBits(mdc, bmp, 0, h, buf, ctypes.byref(bih), 0)
    print("GetDIBits", got)

    src = buf.raw
    rgb = bytearray(w * h * 3)
    for i in range(w * h):
        b, g, r = src[i * 4], src[i * 4 + 1], src[i * 4 + 2]
        rgb[i * 3] = r
        rgb[i * 3 + 1] = g
        rgb[i * 3 + 2] = b

    gdi32.SelectObject(mdc, old)
    gdi32.DeleteObject(bmp)
    gdi32.DeleteDC(mdc)
    user32.ReleaseDC(hwnd, hdc)

    write_png("ui_shot.png", w, h, bytes(rgb))
    print("saved ui_shot.png")
    return 0


if __name__ == "__main__":
    sys.exit(main())
