package main

// 诊断日志：把运行日志、HTTP 访问记录、帧计数全部落到一个内存环形缓冲，
// 通过 GET /diag 暴露成 JSON。联调时用手机浏览器直接打开
// http://<mac-ip>:7980/diag，立刻能看到「手机请求到底有没有到 Mac」，
// 不再靠猜。

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	diagLogCap    = 200  // 运行日志环形缓冲行数
	diagAccessCap = 300  // HTTP 访问记录环形缓冲条数
)

// diagLogRing 运行日志环形缓冲（log 包输出 tee 到这里）
type diagLogRing struct {
	mu   sync.Mutex
	lines []string
}

func (r *diagLogRing) Write(p []byte) (int, error) {
	s := strings.TrimRight(string(p), "\n")
	if s != "" {
		r.mu.Lock()
		r.lines = append(r.lines, s)
		if len(r.lines) > diagLogCap {
			r.lines = r.lines[len(r.lines)-diagLogCap:]
		}
		r.mu.Unlock()
	}
	return len(p), nil
}

func (r *diagLogRing) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

var logRing = &diagLogRing{}

// accessEntry 一条 HTTP 访问记录
type accessEntry struct {
	TS     string `json:"ts"`
	Remote string `json:"remote"`
	Path   string `json:"path"`
	UA     string `json:"ua"`
	Status int    `json:"status"`
	MS     int64  `json:"ms"`
}

var accessRing = struct {
	mu      sync.Mutex
	entries []accessEntry
}{}

func recordAccess(r *http.Request, status int, elapsed time.Duration) {
	remote := r.RemoteAddr
	if i := strings.LastIndex(remote, ":"); i > 0 {
		remote = remote[:i]
	}
	accessRing.mu.Lock()
	accessRing.entries = append(accessRing.entries, accessEntry{
		TS: time.Now().Format("15:04:05.000"), Remote: remote,
		Path: r.URL.Path, UA: r.UserAgent(), Status: status,
		MS: elapsed.Milliseconds(),
	})
	if len(accessRing.entries) > diagAccessCap {
		accessRing.entries = accessRing.entries[len(accessRing.entries)-diagAccessCap:]
	}
	accessRing.mu.Unlock()
}

func accessSnapshot() []accessEntry {
	accessRing.mu.Lock()
	defer accessRing.mu.Unlock()
	out := make([]accessEntry, len(accessRing.entries))
	copy(out, accessRing.entries)
	return out
}

// frameCounters TCP 帧计数（按类型）
var frameCounters = struct {
	sync.Mutex
	m map[string]int
}{m: map[string]int{}}

func countFrame(kind string) {
	frameCounters.Lock()
	frameCounters.m[kind]++
	frameCounters.Unlock()
}

func frameCounterSnapshot() map[string]int {
	frameCounters.Lock()
	defer frameCounters.Unlock()
	out := make(map[string]int, len(frameCounters.m))
	for k, v := range frameCounters.m {
		out[k] = v
	}
	return out
}

// ---- 安卓端日志上行（L: 帧）----

const diagAppLogCap = 300

var appLogRing = struct {
	sync.Mutex
	entries []string // "时间 来自IP 内容"
}{}

// recordAppLog 记一行安卓端上报的日志
func recordAppLog(text, from string) {
	appLogRing.Lock()
	appLogRing.entries = append(appLogRing.entries,
		time.Now().Format("15:04:05.000")+" ["+from+"] "+text)
	if len(appLogRing.entries) > diagAppLogCap {
		appLogRing.entries = appLogRing.entries[len(appLogRing.entries)-diagAppLogCap:]
	}
	appLogRing.Unlock()
}

func appLogSnapshot() []string {
	appLogRing.Lock()
	defer appLogRing.Unlock()
	out := make([]string, len(appLogRing.entries))
	copy(out, appLogRing.entries)
	return out
}

// statusWriter 包装 ResponseWriter 记录状态码
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// withAccessLog HTTP 中间件：记录每次访问（含状态码与耗时）
func withAccessLog(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h(sw, r)
		recordAccess(r, sw.status, time.Since(start))
	}
}

var startedAt = time.Now()

// diagHandler GET /diag → 诊断信息 JSON
func diagHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"version":    version,
		"uptimeSec":  int64(time.Since(startedAt).Seconds()),
		"axTrusted":  axTrusted(),
		"logs":       logRing.snapshot(),
		"access":     accessSnapshot(),
		"frameStats": frameCounterSnapshot(),
		"appLogs":    appLogSnapshot(),
	})
}

// initDiagLog 把 log 包输出同时写到 stderr 和环形缓冲
func initDiagLog() {
	log.SetOutput(io.MultiWriter(os.Stderr, logRing))
}

// summary 帧内容摘要（日志用，防刷屏）
func summary(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
