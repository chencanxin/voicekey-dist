package main

// 实时同显：读取前台 App 聚焦输入框文本并推送给手机端。
// 依赖 macOS 辅助功能 API（与模拟 ⌘V 同一项权限）。
// 安全输入框（密码框）一律跳过，不读取其内容。

/*
#cgo LDFLAGS: -framework ApplicationServices -framework CoreFoundation
#include <stdlib.h>
#include <ApplicationServices/ApplicationServices.h>

// ax_trusted 查询本进程辅助功能授权状态（仅诊断日志用）
static int ax_trusted(void) { return AXIsProcessTrusted() ? 1 : 0;}

// post_cmd_key 直接本进程发送 ⌘+<keycode> 组合键（经 HID 事件层）。
// 不再走 osascript→System Events：省一次自动化授权、无 AppleEvent 超时风险。
// 前提：进程已获辅助功能授权（与实时同显同一项）。
static void post_cmd_key(int keycode) {
    CGEventSourceRef src = CGEventSourceCreate(kCGEventSourceStateCombinedSessionState);
    if (!src) return;
    CGEventRef down = CGEventCreateKeyboardEvent(src, (CGKeyCode)keycode, true);
    CGEventRef up = CGEventCreateKeyboardEvent(src, (CGKeyCode)keycode, false);
    if (down && up) {
        CGEventSetFlags(down, kCGEventFlagMaskCommand);
        CGEventSetFlags(up, kCGEventFlagMaskCommand);
        CGEventPost(kCGHIDEventTap, down);
        CGEventPost(kCGHIDEventTap, up);
    }
    if (down) CFRelease(down);
    if (up) CFRelease(up);
    CFRelease(src);
}

// ax_focused_text 读取前台 App 聚焦元素的文本。
// appBuf 输出前台 App 名；返回 malloc 的 UTF-8 字符串（可为空串），
// 调用方负责 free；返回 NULL 表示当前没有可读取的文本焦点。
static char* ax_focused_text(char* appBuf, int appBufLen) {
    if (appBuf && appBufLen > 0) appBuf[0] = 0;
    AXUIElementRef sys = AXUIElementCreateSystemWide();
    if (!sys) return NULL;
    AXUIElementRef app = NULL, elem = NULL;
    CFTypeRef val = NULL, sub = NULL, nameRef = NULL;
    char* out = NULL;
    if (AXUIElementCopyAttributeValue(sys, kAXFocusedApplicationAttribute, (CFTypeRef*)&app) != kAXErrorSuccess || !app)
        goto done;
    if (AXUIElementCopyAttributeValue(app, kAXTitleAttribute, &nameRef) == kAXErrorSuccess && nameRef) {
        if (appBuf && CFGetTypeID(nameRef) == CFStringGetTypeID())
            CFStringGetCString((CFStringRef)nameRef, appBuf, appBufLen, kCFStringEncodingUTF8);
        goto name_done;
    }
name_done:
    if (AXUIElementCopyAttributeValue(app, kAXFocusedUIElementAttribute, (CFTypeRef*)&elem) != kAXErrorSuccess || !elem)
        goto done;
    // 安全输入框（密码）不读取
    if (AXUIElementCopyAttributeValue(elem, kAXSubroleAttribute, &sub) == kAXErrorSuccess && sub) {
        if (CFGetTypeID(sub) == CFStringGetTypeID() &&
            CFStringCompare((CFStringRef)sub, kAXSecureTextFieldSubrole, 0) == kCFCompareEqualTo)
            goto done; // sub 在下面统一 release
    }
    if (AXUIElementCopyAttributeValue(elem, kAXValueAttribute, &val) != kAXErrorSuccess || !val)
        goto done;
    CFStringRef s = NULL;
    if (CFGetTypeID(val) == CFStringGetTypeID()) {
        s = (CFStringRef)val;
        CFRetain(s);
    } else if (CFGetTypeID(val) == CFAttributedStringGetTypeID()) {
        s = CFAttributedStringGetString((CFAttributedStringRef)val);
        if (s) CFRetain(s);
    }
    if (s) {
        CFIndex len = CFStringGetLength(s);
        CFIndex max = CFStringGetMaximumSizeForEncoding(len, kCFStringEncodingUTF8) + 1;
        out = (char*)malloc(max);
        if (out) CFStringGetCString(s, out, max, kCFStringEncodingUTF8);
        CFRelease(s);
    }
done:
    if (sub) CFRelease(sub);
    if (val) CFRelease(val);
    if (nameRef) CFRelease(nameRef);
    if (elem) CFRelease(elem);
    if (app) CFRelease(app);
    CFRelease(sys);
    return out;
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
	"unsafe"
)

// mustJSON 把同显事件序列化成单行 JSON（SSE 用）
func mustJSON(ev mirrorEvent) string {
	b, err := json.Marshal(ev)
	if err != nil {
		return `{"app":"","text":"","focused":false}`
	}
	return string(b)
}

// mirrorState 当前聚焦输入框的快照（供 /mirror 轮询和 SSE 推送共用）
var mirrorState = struct {
	sync.Mutex
	App     string
	Text    string
	Focused bool
	Updated time.Time
}{}

type mirrorEvent struct {
	App     string `json:"app"`
	Text    string `json:"text"`
	Focused bool   `json:"focused"`
}

var (
	mirrorMu      sync.Mutex
	mirrorClients = map[chan mirrorEvent]struct{}{}
)

func publishMirror(ev mirrorEvent) {
	mirrorMu.Lock()
	defer mirrorMu.Unlock()
	for ch := range mirrorClients {
		select {
		case ch <- ev:
		default:
		}
	}
}

// axTrusted 本进程是否已获得辅助功能授权
func axTrusted() bool { return C.ax_trusted() != 0 }

// 虚拟键码：A=0x00, V=0x09
const (
	vkANSI_A = 0x00
	vkANSI_V = 0x09
)

// postCmdKey 发送 ⌘+keycode 组合键
func postCmdKey(keycode int) { C.post_cmd_key(C.int(keycode)) }

// focusedFieldText 返回前台 App 名与聚焦元素文本。
// ok=false 表示没有可读取的文本焦点（焦点在按钮/桌面等，或权限不足）。
func focusedFieldText() (app, text string, ok bool) {
	appBuf := make([]byte, 256)
	cText := C.ax_focused_text((*C.char)(unsafe.Pointer(&appBuf[0])), C.int(len(appBuf)))
	if cText != nil {
		text = C.GoString(cText)
		C.free(unsafe.Pointer(cText))
		ok = true
	}
	app = string(appBuf[:clen(appBuf)])
	return
}

func clen(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}

// lastPhoneFrame 最近一条手机端文本/清空帧（Mac 已知情的内容+时间）。
// 用途：同显防回环——App 收到 mirror 后回填自家输入框会触发它的编辑监听
// 回发帧，Mac 粘贴后快照与帧内容一致，此时不再推回 App，避免来回闪烁。
var lastPhoneFrame = struct {
	sync.Mutex
	Text string
	At   time.Time
}{}

func notePhoneFrame(text string) {
	lastPhoneFrame.Lock()
	lastPhoneFrame.Text, lastPhoneFrame.At = text, time.Now()
	lastPhoneFrame.Unlock()
}

// isEchoOfRecentFrame 快照文本是否为最近手机帧的回显（1 秒窗口）
func isEchoOfRecentFrame(text string) bool {
	lastPhoneFrame.Lock()
	defer lastPhoneFrame.Unlock()
	return lastPhoneFrame.At.IsZero() == false &&
		text == lastPhoneFrame.Text &&
		time.Since(lastPhoneFrame.At) < time.Second
}

// mirrorLoop 轮询聚焦输入框文本，变化时更新快照并推送。
// 轮询而非 AX 通知：通知需要逐 App 注册观察者，轮询 200ms 对 LAN 同显足够。
func mirrorLoop() {
	var lastApp, lastText string
	var lastFocused bool
	ticker := time.NewTicker(200 * time.Millisecond)
	for range ticker.C {
		app, text, focused := focusedFieldText()
		if focused == lastFocused && app == lastApp && text == lastText {
			continue
		}
		lastFocused, lastApp, lastText = focused, app, text
		mirrorState.Lock()
		mirrorState.App, mirrorState.Text, mirrorState.Focused = app, text, focused
		mirrorState.Updated = time.Now()
		mirrorState.Unlock()
		if isEchoOfRecentFrame(text) {
			// 同显防回环：内容与刚粘贴/清空的手机帧一致（Mac 已知情），不再广播
			continue
		}
		// 焦点变化记一行（不含全文，只记长度，同显排障用）
		log.Printf("同显焦点: app=%q focused=%v 文本长度=%d", app, focused, len([]rune(text)))
		publishMirror(mirrorEvent{App: app, Text: text, Focused: focused})
	}
}

// mirrorSnapshot 供 HTTP 处理函数读取当前快照
func mirrorSnapshot() mirrorEvent {
	mirrorState.Lock()
	defer mirrorState.Unlock()
	return mirrorEvent{App: mirrorState.App, Text: mirrorState.Text, Focused: mirrorState.Focused}
}

func mirrorEventsHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan mirrorEvent, 16)
	mirrorMu.Lock()
	mirrorClients[ch] = struct{}{}
	mirrorMu.Unlock()
	defer func() {
		mirrorMu.Lock()
		delete(mirrorClients, ch)
		mirrorMu.Unlock()
	}()
	// 连接即下发当前快照，手机端无需先轮询
	snap := mirrorSnapshot()
	fmt.Fprintf(w, "data: %s\n\n", mustJSON(snap))
	flusher.Flush()
	for {
		select {
		case ev := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(ev))
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
