package main

// v0.6 控制帧：单键 / 组合键 / 触控板（光标移动、滚动、点击、滑扫）。
// 全部经 CGEvent 本进程直发（与实时同显同一项辅助功能授权）。
// 帧格式见 docs/PROTOCOL.md：K:<key> / C:<combo> / T:m:dx,dy / T:s:dx,dy / T:c:l|r / T:g:<n>:<dir>

/*
#cgo LDFLAGS: -framework ApplicationServices -framework CoreFoundation
#include <stdlib.h>
#include <string.h>
#include <ApplicationServices/ApplicationServices.h>

// 单键：按下并松开一次
static void post_key(int keycode) {
    CGEventSourceRef src = CGEventSourceCreate(kCGEventSourceStateCombinedSessionState);
    if (!src) return;
    CGEventRef down = CGEventCreateKeyboardEvent(src, (CGKeyCode)keycode, true);
    CGEventRef up = CGEventCreateKeyboardEvent(src, (CGKeyCode)keycode, false);
    if (down) CGEventPost(kCGHIDEventTap, down);
    if (up) CGEventPost(kCGHIDEventTap, up);
    if (down) CFRelease(down);
    if (up) CFRelease(up);
    CFRelease(src);
}

// 修饰键 keycode
static int mod_keycode(const char* name) {
    if (strcmp(name, "cmd") == 0) return 55;
    if (strcmp(name, "opt") == 0) return 58;
    if (strcmp(name, "ctrl") == 0) return 59;
    if (strcmp(name, "shift") == 0) return 56;
    return -1;
}

static CGEventFlags mod_flag(const char* name) {
    if (strcmp(name, "cmd") == 0) return kCGEventFlagMaskCommand;
    if (strcmp(name, "opt") == 0) return kCGEventFlagMaskAlternate;
    if (strcmp(name, "ctrl") == 0) return kCGEventFlagMaskControl;
    if (strcmp(name, "shift") == 0) return kCGEventFlagMaskShift;
    return 0;
}

// 组合键：修饰键 down → 主键 down/up → 修饰键 up。
// 主键事件带修饰 flags（部分 App 靠 flags 判定组合键）。
static void post_combo(const char* modName, int keycode) {
    int mod = mod_keycode(modName);
    if (mod < 0) return;
    CGEventFlags flag = mod_flag(modName);
    CGEventSourceRef src = CGEventSourceCreate(kCGEventSourceStateCombinedSessionState);
    if (!src) return;
    CGEventRef modDown = CGEventCreateKeyboardEvent(src, (CGKeyCode)mod, true);
    CGEventRef modUp = CGEventCreateKeyboardEvent(src, (CGKeyCode)mod, false);
    CGEventRef keyDown = CGEventCreateKeyboardEvent(src, (CGKeyCode)keycode, true);
    CGEventRef keyUp = CGEventCreateKeyboardEvent(src, (CGKeyCode)keycode, false);
    if (keyDown) CGEventSetFlags(keyDown, flag);
    if (keyUp) CGEventSetFlags(keyUp, flag);
    // 与真实按键一致的顺序；间隔用 usleep 模拟人手时序，⌘⇥ 切换器对时序敏感
    if (modDown) CGEventPost(kCGHIDEventTap, modDown);
    usleep(15000);
    if (keyDown) CGEventPost(kCGHIDEventTap, keyDown);
    usleep(15000);
    if (keyUp) CGEventPost(kCGHIDEventTap, keyUp);
    usleep(15000);
    if (modUp) CGEventPost(kCGHIDEventTap, modUp);
    if (modDown) CFRelease(modDown);
    if (modUp) CFRelease(modUp);
    if (keyDown) CFRelease(keyDown);
    if (keyUp) CFRelease(keyUp);
    CFRelease(src);
}

// 当前鼠标位置（屏幕像素坐标）
static CGPoint cursor_pos(void) {
    CGEventRef e = CGEventCreate(NULL);
    CGPoint p = e ? CGEventGetLocation(e) : CGPointMake(0, 0);
    if (e) CFRelease(e);
    return p;
}

// 光标相对移动
static void post_mouse_move(double dx, double dy) {
    CGPoint p = cursor_pos();
    CGPoint q = CGPointMake(p.x + dx, p.y + dy);
    CGEventRef e = CGEventCreateMouseEvent(NULL, kCGEventMouseMoved, q, kCGMouseButtonLeft);
    if (e) { CGEventPost(kCGHIDEventTap, e); CFRelease(e); }
}

// 滚动：像素单位；垂直 + 水平双轴
static void post_scroll(double dx, double dy) {
    CGEventRef e = CGEventCreateScrollWheelEvent(NULL, kCGScrollEventUnitPixel, 2, (int32_t)dy, (int32_t)dx);
    if (e) { CGEventPost(kCGHIDEventTap, e); CFRelease(e); }
}

// 左/右键单击（1=左，2=右），位置取当前光标处
static void post_click(int button) {
    CGPoint p = cursor_pos();
    CGEventType downType = (button == 2) ? kCGEventRightMouseDown : kCGEventLeftMouseDown;
    CGEventType upType = (button == 2) ? kCGEventRightMouseUp : kCGEventLeftMouseUp;
    CGMouseButton btn = (button == 2) ? kCGMouseButtonRight : kCGMouseButtonLeft;
    CGEventRef d = CGEventCreateMouseEvent(NULL, downType, p, btn);
    CGEventRef u = CGEventCreateMouseEvent(NULL, upType, p, btn);
    if (d) CGEventPost(kCGHIDEventTap, d);
    if (u) CGEventPost(kCGHIDEventTap, u);
    if (d) CFRelease(d);
    if (u) CFRelease(u);
}
*/
import "C"

import (
	"fmt"
	"strconv"
	"strings"
	"unsafe"
)

// 单键表（docs/PROTOCOL.md）：delete=向前删除(Backspace)
var singleKeys = map[string]int{
	"enter": 36, "delete": 51, "tab": 48, "space": 49, "esc": 53,
	"up": 126, "down": 125, "left": 123, "right": 124,
}

// 组合键主键表（修饰键 + 主键字母/键）
var comboKeys = map[string]int{
	"tab": 48, "space": 49, "a": 0, "c": 8, "v": 9, "z": 13,
}

// moveGain 触控板移动增益（文档建议 1.5~2.0）
const moveGain = 1.5

// handleKeyFrame K:<key>
func handleKeyFrame(arg string) error {
	code, ok := singleKeys[arg]
	if !ok {
		return fmt.Errorf("未知单键 %q", arg)
	}
	C.post_key(C.int(code))
	return nil
}

// handleComboFrame C:<mod>+<key>
func handleComboFrame(arg string) error {
	parts := strings.Split(arg, "+")
	if len(parts) != 2 {
		return fmt.Errorf("组合键格式应为 mod+key，收到 %q", arg)
	}
	mod := strings.ToLower(strings.TrimSpace(parts[0]))
	key := strings.ToLower(strings.TrimSpace(parts[1]))
	code, ok := comboKeys[key]
	if !ok {
		return fmt.Errorf("未知组合主键 %q", key)
	}
	switch mod {
	case "cmd", "opt", "ctrl", "shift":
	default:
		return fmt.Errorf("未知修饰键 %q", mod)
	}
	cMod := C.CString(mod)
	defer C.free(unsafe.Pointer(cMod))
	C.post_combo(cMod, C.int(code))
	return nil
}

// handleTouchFrame T:m:dx,dy / T:s:dx,dy / T:c:l|r / T:g:<n>:<dir>
func handleTouchFrame(arg string) error {
	switch {
	case strings.HasPrefix(arg, "m:"):
		dx, dy, err := parseDelta(arg[2:])
		if err != nil {
			return err
		}
		C.post_mouse_move(C.double(dx*moveGain), C.double(dy*moveGain))
		return nil
	case strings.HasPrefix(arg, "s:"):
		dx, dy, err := parseDelta(arg[2:])
		if err != nil {
			return err
		}
		// 安卓手势约定双指上滑=内容下移；macOS 自然滚动语义下正值即"手指向上"
		C.post_scroll(C.double(dx), C.double(dy))
		return nil
	case arg == "c:l":
		C.post_click(1)
		return nil
	case arg == "c:r":
		C.post_click(2)
		return nil
	case strings.HasPrefix(arg, "g:"):
		// T:g:<n>:<dir> → Ctrl+方向键（3/4 指同映射）
		rest := arg[2:]
		idx := strings.Index(rest, ":")
		if idx < 0 {
			return fmt.Errorf("滑扫帧格式应为 g:<n>:<dir>，收到 %q", arg)
		}
		dir := rest[idx+1:]
		var key int
		switch dir {
		case "up":
			key = 126
		case "down":
			key = 125
		case "left":
			key = 123
		case "right":
			key = 124
		default:
			return fmt.Errorf("未知滑扫方向 %q", dir)
		}
		cCtrl := C.CString("ctrl")
		defer C.free(unsafe.Pointer(cCtrl))
		C.post_combo(cCtrl, C.int(key))
		return nil
	}
	return fmt.Errorf("未知触控帧 %q", arg)
}

func parseDelta(s string) (dx, dy float64, err error) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("增量格式应为 dx,dy，收到 %q", s)
	}
	dx, err = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("dx 解析失败: %v", err)
	}
	dy, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("dy 解析失败: %v", err)
	}
	return dx, dy, nil
}
