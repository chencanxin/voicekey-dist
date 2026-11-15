package main

// 更新主动推送：安卓端连 GET /push/events（SSE），Mac 端每 5 分钟自查
// android.json，发现新版本立即向所有在线 App 推送一帧更新清单，
// App 收到即弹「发现新版本」弹窗，无需自己轮询。

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const pushCheckInterval = 60 * time.Second

type pushEvent struct {
	VersionCode int    `json:"versionCode"`
	VersionName string `json:"versionName"`
	ApkURL      string `json:"apkUrl"`
	ProxyURL    string `json:"apkProxyUrl"`
	Changelog   string `json:"changelog"`
	MacVersion  string `json:"macVersion"` // v0.7.16：App 端可显示 Mac 端版本
}

var pushBroker = struct {
	sync.Mutex
	clients map[chan pushEvent]struct{}
}{clients: map[chan pushEvent]struct{}{}}

func publishPush(ev pushEvent) {
	pushBroker.Lock()
	defer pushBroker.Unlock()
	for ch := range pushBroker.clients {
		select {
		case ch <- ev:
		default:
		}
	}
}

// updateWatchLoop 周期检查更新。versionCode 变化时推送给所有在线 App；
// APK 自动拉取与推送解耦：拉取失败下轮（60s）重试，成功才标记，避免一次
// 网络抖动就永远放弃（曾有 403 后不再重试的卡点）。
// ProxyURL 给路径 "/app-apk"：App 端用已连接的 host 拼接全地址。
func updateWatchLoop() {
	var lastCode, ensuredCode int
	first := true
	for {
		info, err := fetchAppUpdate()
		if err == nil && info.VersionCode > 0 {
			if info.VersionCode != ensuredCode {
				if err := ensureAPK(info); err != nil {
					log.Println("App 更新：APK 自动拉取失败（60 秒后重试）:", err)
				} else {
					ensuredCode = info.VersionCode
				}
			}
			if info.VersionCode != lastCode || first {
				lastCode = info.VersionCode
				first = false
				ev := pushEvent{
					VersionCode: info.VersionCode,
					VersionName: info.VersionName,
					ApkURL:      info.ApkURL,
					ProxyURL:    "/app-apk",
					Changelog:   info.Changelog,
					MacVersion:  version,
				}
				publishPush(ev)
				pushBroker.Lock()
				online := len(pushBroker.clients)
				pushBroker.Unlock()
				log.Printf("推送更新通知: v%s（versionCode %d，%d 个 App 在线）",
					info.VersionName, info.VersionCode, online)
			}
		}
		time.Sleep(pushCheckInterval)
	}
}

func pushJSON(ev pushEvent) string {
	b, err := json.Marshal(ev)
	if err != nil {
		return `{"versionCode":0}`
	}
	return string(b)
}

func pushEventsHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan pushEvent, 4)
	pushBroker.Lock()
	pushBroker.clients[ch] = struct{}{}
	pushBroker.Unlock()
	defer func() {
		pushBroker.Lock()
		delete(pushBroker.clients, ch)
		pushBroker.Unlock()
	}()
	host := r.Host
	// 连接即下发当前版本（App 端自行比对 versionCode 决定是否弹窗）
	if info, err := fetchAppUpdate(); err == nil && info.VersionCode > 0 {
		ev := pushEvent{
			VersionCode: info.VersionCode,
			VersionName: info.VersionName,
			ApkURL:      info.ApkURL,
			ProxyURL:    host + "/app-apk",
			Changelog:   info.Changelog,
			MacVersion:  version,
		}
		fmt.Fprintf(w, "data: %s\n\n", pushJSON(ev))
		flusher.Flush()
	}
	for {
		select {
		case ev := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", pushJSON(ev))
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
