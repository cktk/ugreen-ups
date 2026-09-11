"""导出 walk 主窗口的子控件树（类名 / 客户区坐标 / 文本），用于精确诊断布局。

纯标准库：ctypes 枚举窗口即可，只读，不修改任何东西。

用法（在仓库根目录下，程序已运行时）：
    python tools/uicheck/dump_window.py

典型用途：确认某个分组的实际宽度、控件是否被 walk 的 BoxLayout 居中等。
"""
import ctypes
import ctypes.wintypes as wt
import sys

user32 = ctypes.WinDLL("user32", use_last_error=True)

TITLE = "UGREEN US3000 UPS Monitor"
WNDENUMPROC = ctypes.WINFUNCTYPE(wt.BOOL, wt.HWND, wt.LPARAM)

try:
    user32.SetProcessDpiAwarenessContext(ctypes.c_void_p(-4))
except Exception:
    pass
try:
    user32.SetProcessDPIAware()
except Exception:
    pass

user32.GetWindowTextW.argtypes = [wt.HWND, wt.LPWSTR, ctypes.c_int]
user32.GetClassNameW.argtypes = [wt.HWND, wt.LPWSTR, ctypes.c_int]


def text_of(hwnd):
    buf = ctypes.create_unicode_buffer(512)
    user32.GetWindowTextW(hwnd, buf, 512)
    return buf.value


def class_of(hwnd):
    buf = ctypes.create_unicode_buffer(256)
    user32.GetClassNameW(hwnd, buf, 256)
    return buf.value


def main():
    top = user32.FindWindowW(None, TITLE)
    if not top:
        print("window not found")
        return 1
    print("top hwnd", top)

    origin = wt.POINT(0, 0)
    user32.ClientToScreen(top, ctypes.byref(origin))
    print("client origin on screen:", origin.x, origin.y)

    rows = []

    def walk(hwnd, depth):
        rect = wt.RECT()
        user32.GetWindowRect(hwnd, ctypes.byref(rect))
        x = rect.left - origin.x
        y = rect.top - origin.y
        w = rect.right - rect.left
        h = rect.bottom - rect.top
        rows.append((y, x, w, h, "*" * depth + class_of(hwnd), text_of(hwnd)[:38]))

        # 只枚举直接子窗口，EnumChildWindows 会递归导致重复
        child = 0
        while True:
            child = user32.FindWindowExW(hwnd, child, None, None)
            if not child:
                break
            walk(child, depth + 1)

    walk(top, 0)
    rows.sort()
    for y, x, w, h, cls, txt in rows:
        print(f"x={x:5d} y={y:5d} w={w:5d} h={h:4d}  {cls:<34} {txt}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
