package main

// VoiceKey Mac 接收端（带网页仪表盘）
// 1. 自动选端口：优先 7979，被占用则随机分配
// 2. mDNS 广播 _voicekey._tcp（端口随实际值，手机端自动发现）
// 3. 接收 Base64(UTF-8) 文本 → pbcopy 写剪贴板 → osascript 模拟 ⌘V
// 4. 本地网页仪表盘：启动自动弹浏览器，实时显示连接状态与接收记录
//
// 注意: 首次运行后需在 系统设置 → 隐私与安全性 → 辅助功能 勾选本程序。

import (
	"bufio"
	"encoding/base64"
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
	"time"

	"github.com/hashicorp/mdns"
)

const preferredPort = 7979

// version 当前版本，与发布仓库 voicekey-dist 根目录的 version.txt 对应
const version = "0.4.0"

// versionCheckURLs 按顺序尝试的版本检查地址（jsDelivr 国内可达优先）
var versionCheckURLs = []string{
	"https://cdn.jsdelivr.net/gh/chencanxin/voicekey-dist@main/version.txt",
	"https://fastly.jsdelivr.net/gh/chencanxin/voicekey-dist@main/version.txt",
	"https://raw.githubusercontent.com/chencanxin/voicekey-dist/main/version.txt",
}

// ---- 自动更新检查 ----
// 启动后异步检查公开仓库 version.txt；发现新版本则通过 Homebrew 升级。
// brew 升级会拉源码在本地重新构建（见 tap 仓库 Formula）。
// 任何失败只记日志，不影响本次运行。

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

func paste(text string) {
	pbcopy := exec.Command("pbcopy")
	stdin, _ := pbcopy.StdinPipe()
	stdin.Write([]byte(text))
	stdin.Close()
	if err := pbcopy.Run(); err != nil {
		log.Println("pbcopy 失败:", err)
		return
	}
	osa := exec.Command("osascript", "-e",
		`tell application "System Events" to keystroke "v" using command down`)
	if err := osa.Run(); err != nil {
		log.Println("模拟粘贴失败（检查辅助功能权限）:", err)
	}
	log.Printf("已粘贴: %s", text)
	publish(event{Time: time.Now().Format("15:04:05"), Text: text})
}

func handle(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(line[:len(line)-1])
	if err != nil {
		log.Println("Base64 解码失败:", err)
		return
	}
	paste(string(raw))
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
</style>
</head>
<body>
<h1>VoiceKey Mac 端</h1>
<div class="card">
  <div>状态：<span class="ok">● 运行中</span></div>
  <div style="margin-top:8px">端口：<span class="mono">{{.Port}}</span>
       &nbsp; 启动于 <span class="mono">{{.Start}}</span></div>
  <div class="dim" style="margin-top:8px">手机端打开 App 会自动搜索到这台 Mac，无需填写 IP。</div>
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
</script>
</body>
</html>`))

func main() {
	log.Printf("VoiceKey Mac 端 v%s", version)
	go checkUpdate() // 异步检查更新，失败不影响运行

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

	// 网页仪表盘
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		page.Execute(w, map[string]any{"Port": port, "Start": startAt})
	})
	http.HandleFunc("/events", eventsHandler)
	go http.Serve(uiLn, nil)

	url := fmt.Sprintf("http://127.0.0.1:%d", uiLn.Addr().(*net.TCPAddr).Port)
	log.Printf("VoiceKey Mac 端已启动：仪表盘 %s，数据端口 %d（mDNS 广播中）", url, port)
	exec.Command("open", url).Start()

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handle(conn)
	}
}
