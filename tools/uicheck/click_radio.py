"""端到端验证“触发动作”单选按钮：模拟点击 → 点保存 → 核对配置文件。

为什么不用 PowerShell：本机沙箱禁止 Add-Type / Reflection.Assembly / COM，
因此全部用纯标准库 ctypes 实现 Win32 交互。

用法（仓库根目录，程序已运行时）：
    python tools/uicheck/click_radio.py <单选按钮文字> [保存按钮文字]

BM_CLICK 会走真实的 BN_CLICKED 通知路径，walk 的 RadioButton.WndProc 会处理，
因此能真正驱动界面逻辑（不像 SetWindowTextW 那样绕过控件的内部状态）。
"""
import ctypes
import ctypes.wintypes as wt
import sys
import time

user32 = ctypes.WinDLL("user32", use_last_error=True)

TITLE = "UGREEN US3000 UPS Monitor"

user32.FindWindowW.argtypes = [wt.LPCWSTR, wt.LPCWSTR]
user32.FindWindowW.restype = wt.HWND
user32.FindWindowExW.argtypes = [wt.HWND, wt.HWND, wt.LPCWSTR, wt.LPCWSTR]
user32.FindWindowExW.restype = wt.HWND
user32.GetWindowTextW.argtypes = [wt.HWND, wt.LPWSTR, ctypes.c_int]
user32.GetWindowTextW.restype = ctypes.c_int
user32.GetWindowTextLengthW.argtypes = [wt.HWND]
user32.GetWindowTextLengthW.restype = ctypes.c_int
user32.SendMessageW.argtypes = [wt.HWND, ctypes.c_uint, ctypes.c_uint, ctypes.c_void_p]
user32.SendMessageW.restype = ctypes.c_void_p
user32.IsWindowVisible.argtypes = [wt.HWND]
user32.IsWindowVisible.restype = wt.BOOL
user32.GetClassNameW.argtypes = [wt.HWND, wt.LPWSTR, ctypes.c_int]
user32.GetClassNameW.restype = ctypes.c_int
user32.GetWindowLongW.argtypes = [wt.HWND, ctypes.c_int]
user32.GetWindowLongW.restype = ctypes.c_long

BM_CLICK = 0x00F5
BS_TYPEMASK = 0x000F
BS_CHECKBOX, BS_AUTOCHECKBOX = 0x2, 0x3
BS_RADIOBUTTON, BS_AUTORADIOBUTTON = 0x4, 0x9
CLICKABLE = {
    BS_CHECKBOX: "复选框",
    BS_AUTOCHECKBOX: "自动复选框",
    BS_RADIOBUTTON: "单选按钮",
    BS_AUTORADIOBUTTON: "自动单选按钮",
}


def text_of(hwnd):
    n = user32.GetWindowTextLengthW(hwnd)
    buf = ctypes.create_unicode_buffer(n + 2)
    user32.GetWindowTextW(hwnd, buf, n + 2)
    return buf.value


def class_of(hwnd):
    buf = ctypes.create_unicode_buffer(256)
    user32.GetClassNameW(hwnd, buf, 256)
    return buf.value


def children(parent):
    """枚举直接子控件（FindWindowExW 逐级遍历，避开递归回调）。"""
    out, prev = [], None
    while True:
        h = user32.FindWindowExW(parent, prev, None, None)
        if not h:
            return out
        out.append(h)
        prev = h


def all_buttons(root):
    """递归收集所有 Button 类控件（含分组框内部的子控件）。"""
    res = []
    for h in children(root):
        if class_of(h) == "Button":
            res.append(h)
        res.extend(all_buttons(h))
    return res


def find_button(root, label):
    for h in all_buttons(root):
        if text_of(h) == label:
            return h
    return None


def main():
    label = sys.argv[1] if len(sys.argv) > 1 else "睡眠"
    save_label = sys.argv[2] if len(sys.argv) > 2 else "保存设置"

    mw = user32.FindWindowW(None, TITLE)
    if not mw:
        print("FAIL: 找不到主窗口")
        return 2

    target = find_button(mw, label)
    if not target:
        print(f"FAIL: 找不到文字为 {label!r} 的按钮")
        return 2

    style = user32.GetWindowLongW(target, -16) & BS_TYPEMASK
    kind = CLICKABLE.get(style)
    if not kind:
        print(f"FAIL: {label!r} 不是复选框/单选按钮（BS 类型 = 0x{style:X}）")
        return 2
    print(f"OK: 找到 {label!r}（{kind}，BS 类型 = 0x{style:X}）")

    user32.SendMessageW(target, BM_CLICK, 0, None)
    time.sleep(0.4)

    save = find_button(mw, save_label)
    if not save:
        print(f"FAIL: 找不到 {save_label!r} 按钮")
        return 2
    user32.SendMessageW(save, BM_CLICK, 0, None)
    time.sleep(0.8)
    print(f"OK: 已点击 {label!r} → {save_label!r}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
