package main

// VoiceKey Mac 接收端（带网页仪表盘 + 实时同显）
// 1. 自动选端口：优先 7979，被占用则随机分配
// 2. mDNS 广播 _voicekey._tcp（端口随实际值，手机端自动发现）
// 3. 接收 Base64(UTF-8) 文本 → pbcopy 写剪贴板 → osascript 模拟 ⌘V
//    行首 R: 前缀表示全选替换（⌘A + ⌘V），用于"手机改完一次性发回"
// 4. 实时同显：轮询前台 App 聚焦输入框文本，经 /mirror 与 /mirror/events 推送
// 5. 本地网页仪表盘：启动自动弹浏览器，实时显示连接状态与接收记录
//
// 注意: 首次运行后需在 系统设置 → 隐私与安全性 → 辅助功能 勾选本程序。

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/mdns"
)

const preferredPort = 7979

// version 当前版本，与发布仓库 voicekey-dist 根目录的 version.txt 对应
const version = "0.7.18"

// versionCheckURLs 版本检查地址。fetchLatestVersion 会取所有成功响应中的
// 最大版本号，单个源陈旧不影响判定。api.github.com 最快最新排首位，
// jsDelivr 系列是国内可达兜底（边缘缓存可能滞后几分钟），raw 最后。
var versionCheckURLs = []string{
	"https://api.github.com/repos/chencanxin/voicekey-dist/contents/version.txt?ref=main",
	"https://gcore.jsdelivr.net/gh/chencanxin/voicekey-dist@main/version.txt",
	"https://cdn.jsdelivr.net/gh/chencanxin/voicekey-dist@main/version.txt",
	"https://fastly.jsdelivr.net/gh/chencanxin/voicekey-dist@main/version.txt",
	"https://raw.githubusercontent.com/chencanxin/voicekey-dist/main/version.txt",
}

// ---- 自动更新检查（详见 selfupdate.go 的 60 秒自更新循环） ----
// checkUpdate 启动时立即查一次版本并提示；自更新循环 selfUpdateLoop 负责
// 后续每 60 秒轮询 + brew upgrade + 自重启。

func checkUpdate() {
	var latest string
	client := &http.Client{Timeout: 5 * time.Second}
	for _, u := range versionCheckURLs {
		resp, err := client.Get(u)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		latest = strings.TrimSpace(string(body))
		break
	}
	if latest == "" {
		log.Println("更新检查失败（网络不通？）")
		return
	}
	cur, cerr := strconv.ParseFloat(version, 64)
	lat, lerr := strconv.ParseFloat(latest, 64)
	if cerr != nil || lerr != nil || lat <= cur {
		log.Printf("当前已是最新版本 v%s", version)
		return
	}
	log.Printf("发现新版本 v%s（当前 v%s）", latest, version)
	if _, err := exec.LookPath("brew"); err != nil {
		log.Printf("未安装 Homebrew，无法自动升级，请手动更新到 v%s", latest)
		return
	}
	log.Printf("正在通过 Homebrew 升级… 完成后请重启本程序")
	cmd := exec.Command("brew", "upgrade", "voicekey")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Println("brew upgrade 失败:", err)
	}
}

// ---- 事件广播（推送到网页） ----

type event struct {
	Time string `json:"time"`
	Text string `json:"text"`
}

var (
	mu      sync.Mutex
	clients = map[chan event]struct{}{}
	startAt = time.Now().Format("15:04:05")
)

func publish(ev event) {
	mu.Lock()
	defer mu.Unlock()
	for ch := range clients {
		select {
		case ch <- ev:
		default:
		}
	}
}

func eventsHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan event, 16)
	mu.Lock()
	clients[ch] = struct{}{}
	mu.Unlock()
	defer func() {
		mu.Lock()
		delete(clients, ch)
		mu.Unlock()
	}()
	fmt.Fprintf(w, "data: {\"type\":\"hello\",\"start\":\"%s\"}\n\n", startAt)
	flusher.Flush()
	for {
		select {
		case ev := <-ch:
			fmt.Fprintf(w, "data: {\"type\":\"text\",\"time\":\"%s\",\"text\":%q}\n\n",
				ev.Time, ev.Text)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ---- 核心功能 ----

func paste(text string, replace bool) {
	// 修复: pbcopy 在 C/POSIX locale（如 launchd/后台进程环境）下会静默丢弃
	// 非 ASCII 字符，导致中文永远进不了剪贴板。必须显式指定 UTF-8 locale。
	utf8Env := append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")

	pbcopy := exec.Command("pbcopy")
	pbcopy.Env = utf8Env
	stdin, _ := pbcopy.StdinPipe()
	stdin.Write([]byte(text))
	stdin.Close()
	if err := pbcopy.Run(); err != nil {
		log.Println("pbcopy 失败:", err)
		return
	}

	// 回读校验：剪贴板为空说明写入没生效（locale/会话问题），
	// 此时再模拟 ⌘V 只会粘贴出旧内容，直接报失败。
	if out, _ := exec.Command("pbpaste").Output(); text != "" && len(out) == 0 {
		log.Println("剪贴板写入校验失败（pbpaste 为空），已跳过本次粘贴")
		return
	}

	// CGEvent 发送按键需要辅助功能授权；未授权时静默无效果，提前拦截报错
	if !axTrusted() {
		log.Println("模拟粘贴失败（辅助功能权限未授予：系统设置 → 隐私与安全性 → 辅助功能 → 勾选 /opt/homebrew/bin/voicekey）")
		return
	}
	if replace {
		// 替换语义：先 ⌘A 全选聚焦输入框，再 ⌘V 覆盖
		postCmdKey(vkANSI_A)
		time.Sleep(40 * time.Millisecond)
	}
	postCmdKey(vkANSI_V)
	app, _, _ := focusedFieldText()
	suffix := ""
	if replace {
		suffix = "，已全选替换"
	}
	log.Printf("已粘贴: %s（前台: %s%s）", text, app, suffix)
	publish(event{Time: time.Now().Format("15:04:05"), Text: text})
}

func remoteIP(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

// handle 处理一条 TCP 连接：支持一连接多帧（每帧一行，\n 结尾），
// 手机端可复用连接流水线发帧（如高频移动帧），避免每帧一次握手造成的掉帧。
func handle(conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		handleFrame(strings.TrimRight(line, "\n"), conn)
	}
}

func handleFrame(s string, conn net.Conn) {
	from := remoteIP(conn)

	// v0.6 控制帧：K: 单键 / C: 组合键 / T: 触控板（不走 base64、不进历史）
	// v0.7 L: 帧：安卓端日志上行（base64 的一行日志文本），供 /diag 直读
	// v0.7.16 V: 帧：安卓端版本上报（明文版本号，如 0.7.0），用于版本互显
	// v0.7.17 N: 帧：换行（Unicode \n 字符直投，见 control.go handleNewlineFrame）
	switch {
	case strings.HasPrefix(s, "V:"):
		countFrame("version")
		setAppVersion(strings.TrimSpace(s[2:]), from)
		return
	case strings.HasPrefix(s, "L:"):
		countFrame("log")
		raw, err := base64.StdEncoding.DecodeString(s[2:])
		if err != nil {
			log.Printf("日志帧解码失败（来自 %s）: %v", from, err)
			return
		}
		recordAppLog(string(raw), from)
		return
	case strings.HasPrefix(s, "K:"):
		countFrame("key")
		if err := handleKeyFrame(s[2:]); err != nil {
			log.Printf("按键帧失败（来自 %s）: %v", from, err)
			return
		}
		log.Printf("按键: %s（来自 %s）", s[2:], from)
		return
	case strings.HasPrefix(s, "N:"):
		countFrame("newline")
		if err := handleNewlineFrame(); err != nil {
			log.Printf("换行帧失败（来自 %s）: %v", from, err)
			return
		}
		log.Printf("换行（来自 %s）", from)
		return
	case strings.HasPrefix(s, "C:"):
		countFrame("combo")
		if err := handleComboFrame(s[2:]); err != nil {
			log.Printf("组合键帧失败（来自 %s）: %v", from, err)
			return
		}
		log.Printf("组合键: %s（来自 %s）", s[2:], from)
		return
	case strings.HasPrefix(s, "T:"):
		countFrame("touch")
		if err := handleTouchFrame(s[2:]); err != nil {
			log.Printf("触控帧失败（来自 %s）: %v", from, err)
			return
		}
		if !strings.HasPrefix(s[2:], "m:") { // 移动帧太密，只在计数里
			log.Printf("触控: %s（来自 %s）", summary(s, 40), from)
		}
		return
	}

	// 协议：R:<base64> 全选替换；<base64> 光标处粘贴
	replace := false
	if strings.HasPrefix(s, "R:") {
		replace = true
		s = s[2:]
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		log.Println("Base64 解码失败:", err)
		return
	}
	countFrame("text")
	notePhoneFrame(string(raw)) // 同显防回环：Mac 已知情，快照一致时不回推 App
	// 实时同显清空：手机端把文字删光时发 R: + 空串，Mac 端全选删除聚焦输入框内容。
	// 不经过剪贴板（pbcopy 空串在部分环境被 DLP 拦截），直接发删除键，最可靠。
	if replace && len(raw) == 0 {
		if !axTrusted() {
			log.Println("模拟清空失败（辅助功能权限未授予）")
			return
		}
		clearFocusedField()
		app, _, _ := focusedFieldText()
		log.Printf("已清空聚焦输入框（前台: %s，来自 %s）", app, from)
		publish(event{Time: time.Now().Format("15:04:05"), Text: ""})
		return
	}
	recordHistory(string(raw))
	paste(string(raw), replace)
}


// selfRestartWatchdog 自愈看门狗：启动时未获辅助功能授权则周期性自重启。
// macOS 对运行中的进程缓存权限查询结果，用户勾选授权后已运行的进程拿不到，
// 必须换进程镜像。每 60 秒 exec 重启自己一次，直到启动时检测到已授权。
func selfRestartWatchdog() {
	if axTrusted() {
		return
	}
	log.Println("看门狗：等待辅助功能授权，每 60 秒自动重启进程直到授权生效")
	ticker := time.NewTicker(60 * time.Second)
	for range ticker.C {
		if axTrusted() {
			log.Println("看门狗：检测到授权状态变化，重启进程使其生效")
		} else {
			log.Println("看门狗：仍未授权，重启进程重试")
		}
		self, err := os.Executable()
		if err != nil {
			continue
		}
		syscall.Exec(self, os.Args, os.Environ()) // 成功则不再返回
	}
}

// startMDNS 广播 _voicekey._tcp 服务
func startMDNS(port int) (*mdns.Server, error) {
	zone, err := mdns.NewMDNSService("VoiceKey", "_voicekey._tcp",
		"", "", port, nil, []string{"VoiceKey Mac receiver"})
	if err != nil {
		return nil, err
	}
	return mdns.NewServer(&mdns.Config{Zone: zone})
}

// ---- 网页 ----

var page = template.Must(template.New("ui").Parse(`<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>VoiceKey Mac 端</title>
<style>
  body { font-family: -apple-system, "PingFang SC", sans-serif;
         max-width: 720px; margin: 40px auto; padding: 0 16px; background: #f5f5f7; }
  h1 { font-size: 22px; }
  .card { background: #fff; border-radius: 12px; padding: 20px;
          box-shadow: 0 1px 4px rgba(0,0,0,.08); margin-bottom: 16px; }
  .ok { color: #1a9c48; font-weight: 600; }
  .mono { font-family: ui-monospace, monospace; background: #f0f0f2;
          padding: 2px 6px; border-radius: 6px; }
  #log div { padding: 8px 0; border-bottom: 1px solid #eee;
             font-size: 14px; color: #333; }
  #log time { color: #999; margin-right: 10px; font-family: ui-monospace, monospace; }
  .dim { color: #888; font-size: 13px; }
  .row { margin-top: 12px; font-size: 14px; color: #333; }
  .row label { display: block; margin-bottom: 4px; }
  .row input[type=range] { width: 100%; }
  .switch-row { display: flex; align-items: center; gap: 8px; margin-top: 14px; font-size: 14px; }
  button { margin-top: 14px; padding: 8px 20px; border: 0; border-radius: 8px;
           background: #0071e3; color: #fff; font-size: 14px; cursor: pointer; }
  button:active { opacity: .7; }
</style>
</head>
<body>
<h1>VoiceKey Mac 端</h1>
<div class="card">
  <div>状态：<span class="ok">● 运行中</span></div>
  <div style="margin-top:8px">版本：<span class="mono">v{{.Version}}</span>
       &nbsp; 端口：<span class="mono">{{.Port}}</span>
       &nbsp; 启动于 <span class="mono">{{.Start}}</span></div>
  <div style="margin-top:8px">已连接手机端：<span class="mono" id="appVer">未上报（App 需 ≥ v0.7.0）</span></div>
  <div class="dim" style="margin-top:8px">手机端打开 App 会自动搜索到这台 Mac，无需填写 IP。</div>
</div>
<div class="card">
  <b>实时同显</b>
  <div style="margin-top:8px">前台 App：<span class="mono" id="mApp">—</span>
       &nbsp; 焦点：<span id="mFocus" class="dim">检测中…</span></div>
  <div class="dim" style="margin-top:8px; white-space:pre-wrap" id="mText"></div>
</div>
<div class="card">
  <b>触控板体验设置</b> <span class="dim">（保存后即时生效，存于 ~/.voicekey/settings.json）</span>
  <div class="row"><label>移动敏感度 <span id="vMove" class="mono">1.5</span></label>
    <input type="range" id="sMove" min="0.2" max="3" step="0.1" value="1.5"></div>
  <div class="row"><label>上下滚动敏感度 <span id="vSy" class="mono">1.0</span></label>
    <input type="range" id="sSy" min="0.2" max="3" step="0.1" value="1.0"></div>
  <div class="row"><label>左右滚动敏感度 <span id="vSx" class="mono">1.0</span></label>
    <input type="range" id="sSx" min="0.2" max="3" step="0.1" value="1.0"></div>
  <div class="switch-row"><input type="checkbox" id="sInv">
    <label for="sInv">滚动方向反转（双指上滑 = 内容上移/传统滚动）</label></div>
  <button id="saveBtn">保存设置</button> <span id="saveMsg" class="dim"></span>
</div>
<div class="card">
  <b>接收记录</b>
  <div id="log"><div class="dim">等待手机发送…</div></div>
</div>
<script>
const es = new EventSource("/events");
const log = document.getElementById("log");
es.onmessage = e => {
  const d = JSON.parse(e.data);
  if (d.type !== "text") return;
  if (log.firstChild && log.firstChild.className === "dim") log.innerHTML = "";
  const div = document.createElement("div");
  div.innerHTML = "<time>" + d.time + "</time>" + d.text.replace(/</g, "&lt;");
  log.prepend(div);
};
const mes = new EventSource("/mirror/events");
mes.onmessage = e => {
  const d = JSON.parse(e.data);
  document.getElementById("mApp").textContent = d.app || "—";
  const f = document.getElementById("mFocus");
  f.textContent = d.focused ? "输入框已捕获" : "无文本焦点";
  f.className = d.focused ? "ok" : "dim";
  document.getElementById("mText").textContent = d.focused ? d.text : "（点击任意输入框开始同显）";
};
// 触控板设置：载入当前值 → 保存
function refreshAppVer() {
  fetch("/diag").then(r => r.json()).then(d => {
    const v = d.appVersion && d.appVersion.version;
    document.getElementById("appVer").textContent = v ? ("v" + v) : "未上报（App 需 ≥ v0.7.0）";
  }).catch(() => {});
}
refreshAppVer();
setInterval(refreshAppVer, 5000);
const S = { move: document.getElementById("sMove"), sy: document.getElementById("sSy"),
            sx: document.getElementById("sSx"), inv: document.getElementById("sInv") };
const V = { move: document.getElementById("vMove"), sy: document.getElementById("vSy"),
            sx: document.getElementById("vSx") };
function refreshLabels() {
  V.move.textContent = Number(S.move.value).toFixed(1);
  V.sy.textContent = Number(S.sy.value).toFixed(1);
  V.sx.textContent = Number(S.sx.value).toFixed(1);
}
["move","sy","sx"].forEach(k => S[k].addEventListener("input", refreshLabels));
fetch("/settings").then(r => r.json()).then(d => {
  S.move.value = d.moveGain; S.sy.value = d.scrollGainY; S.sx.value = d.scrollGainX;
  S.inv.checked = d.scrollInvert; refreshLabels();
}).catch(() => {});
document.getElementById("saveBtn").onclick = () => {
  const body = { moveGain: Number(S.move.value), scrollGainY: Number(S.sy.value),
                 scrollGainX: Number(S.sx.value), scrollInvert: S.inv.checked };
  fetch("/settings", { method: "POST", headers: {"Content-Type": "application/json"},
                       body: JSON.stringify(body) })
    .then(r => r.json())
    .then(() => { document.getElementById("saveMsg").textContent = "已保存 ✓"; })
    .catch(() => { document.getElementById("saveMsg").textContent = "保存失败"; });
};
</script>
</body>
</html>`))

func main() {
	initDiagLog()
	loadSettings()
	log.Printf("VoiceKey Mac 端 v%s", version)
	go selfUpdateLoop() // 60 秒轮询自更新：发现新版本 → brew upgrade → 自重启

	// 数据端口：优先 7979，被占用则用系统分配的空闲端口（mDNS 广播此端口）
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", preferredPort))
	if err != nil {
		ln, err = net.Listen("tcp", ":0")
		if err != nil {
			log.Fatal(err)
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port

	// 网页仪表盘端口：7980 起步，被占用则顺延
	uiLn, err := net.Listen("tcp", ":7980")
	if err != nil {
		uiLn, err = net.Listen("tcp", ":0")
		if err != nil {
			log.Fatal(err)
		}
	}

	// mDNS 广播（端口为实际值）
	// mDNS 广播失败不致命：只是手机无法自动发现，仍可手动填 IP
	mdnsServer, err := startMDNS(port)
	if err != nil {
		log.Println("警告: mDNS 广播失败（手机将无法自动发现）:", err)
	} else {
		defer mdnsServer.Shutdown()
	}

	// 网页仪表盘（全部经访问日志中间件，联调可查）
	http.HandleFunc("/", withAccessLog(func(w http.ResponseWriter, r *http.Request) {
		page.Execute(w, map[string]any{"Port": port, "Start": startAt, "Version": version})
	}))
	http.HandleFunc("/events", withAccessLog(eventsHandler))
	http.HandleFunc("/settings", withAccessLog(settingsHandler))
	http.HandleFunc("/mirror", withAccessLog(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(mirrorSnapshot())
	}))
	http.HandleFunc("/mirror/events", withAccessLog(mirrorEventsHandler))
	http.HandleFunc("/history", withAccessLog(historyHandler))
	http.HandleFunc("/app-update", withAccessLog(appUpdateHandler))
	http.HandleFunc("/app-apk", withAccessLog(appApkHandler))
	http.HandleFunc("/diag", withAccessLog(diagHandler))
	http.HandleFunc("/push/events", withAccessLog(pushEventsHandler))
	go http.Serve(uiLn, nil)

	// 发送历史持久化（~/.voicekey/history.jsonl）
	loadHistory()

	// 实时同显：轮询前台聚焦输入框（与 ⌘V 模拟共用辅助功能权限）
	if axTrusted() {
		log.Println("实时同显：辅助功能权限已授予")
	} else {
		log.Println("实时同显：等待辅助功能权限（系统设置 → 隐私与安全性 → 辅助功能 → 勾选 /opt/homebrew/bin/voicekey），授权后自动生效")
		go selfRestartWatchdog() // 授权后自动重启生效，无需人工干预
	}
	go mirrorLoop()
	go updateWatchLoop() // 更新主动推送：发现新版本即向所有在线 App 广播

	url := fmt.Sprintf("http://127.0.0.1:%d", uiLn.Addr().(*net.TCPAddr).Port)
	log.Printf("VoiceKey Mac 端已启动：仪表盘 %s，数据端口 %d（mDNS 广播中，实时同显已启用）", url, port)
	// 浏览器弹窗策略：
	// - VK_NO_BROWSER=1 完全不弹（看门狗/自更新重启、launchd 后台运行时用）
	// - 正常启动也只后台开标签页（open -g 不抢前台焦点），不打扰用户当前工作
	if os.Getenv("VK_NO_BROWSER") == "1" {
		log.Println("仪表盘未自动打开（VK_NO_BROWSER=1），可手动访问 " + url)
	} else {
		exec.Command("open", "-g", url).Start()
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handle(conn)
	}
}