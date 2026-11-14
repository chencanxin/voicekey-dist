package main

// App 端自动更新：Android App 从 Mac 守护进程查询新版本并代理下载 APK。
//
// 版本清单：voicekey-dist 仓库根目录 android.json（与 version.txt 同一发布通道）
// 	{"versionCode":6,"versionName":"0.5.1","apkUrl":"https://…/app-release.apk","changelog":"…"}
// APK 本体：GitHub Release 附件（apkUrl），手机经 /app-apk 由 Mac 代理中转下载。

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// androidManifestURLs 与 version.txt 同一组国内优先镜像
var androidManifestURLs = []string{
	"https://cdn.jsdelivr.net/gh/chencanxin/voicekey-dist@main/android.json",
	"https://fastly.jsdelivr.net/gh/chencanxin/voicekey-dist@main/android.json",
	"https://raw.githubusercontent.com/chencanxin/voicekey-dist/main/android.json",
}

type appUpdateInfo struct {
	VersionCode int    `json:"versionCode"`
	VersionName string `json:"versionName"`
	ApkURL      string `json:"apkUrl"`
	Changelog   string `json:"changelog"`
}

var appUpdateCache = struct {
	sync.Mutex
	info      appUpdateInfo
	fetchedAt time.Time
	err       error
}{}

const appUpdateCacheTTL = 5 * time.Minute

// fetchAppUpdate 拉取 android.json。
// 快路径：缓存未过期直接返回。
// 慢路径：三个镜像并发拉（总预算 4 秒），失败则沿用旧缓存（不清空），
// 无缓存才报错——保证 /app-update 与 /push/events 的响应速度，手机端 3 秒超时不至于落空。
func fetchAppUpdate() (appUpdateInfo, error) {
	appUpdateCache.Lock()
	if time.Since(appUpdateCache.fetchedAt) < appUpdateCacheTTL && appUpdateCache.err == nil {
		info := appUpdateCache.info
		appUpdateCache.Unlock()
		return info, nil
	}
	stale := appUpdateCache.info
	haveCache := !appUpdateCache.fetchedAt.IsZero()
	appUpdateCache.Unlock()

	info, err := fetchAppUpdateFresh()
	if err != nil {
		if haveCache {
			log.Println("App 更新检查失败（沿用缓存）:", err)
			return stale, nil
		}
		return appUpdateInfo{}, err
	}
	appUpdateCache.Lock()
	appUpdateCache.info, appUpdateCache.fetchedAt, appUpdateCache.err = info, time.Now(), nil
	appUpdateCache.Unlock()
	return info, nil
}

// fetchAppUpdateFresh 并发拉全部镜像，取第一个成功响应
func fetchAppUpdateFresh() (appUpdateInfo, error) {
	type result struct {
		info appUpdateInfo
		err  error
	}
	ch := make(chan result, len(androidManifestURLs))
	for _, u := range androidManifestURLs {
		go func(u string) {
			client := &http.Client{Timeout: 4 * time.Second}
			resp, err := client.Get(u)
			if err != nil {
				ch <- result{err: err}
				return
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			if err != nil {
				ch <- result{err: err}
				return
			}
			if resp.StatusCode != http.StatusOK {
				ch <- result{err: fmt.Errorf("HTTP %d", resp.StatusCode)}
				return
			}
			var info appUpdateInfo
			if err := json.Unmarshal(body, &info); err != nil {
				ch <- result{err: err}
				return
			}
			if info.VersionCode <= 0 || info.ApkURL == "" {
				ch <- result{err: fmt.Errorf("android.json 字段不完整")}
				return
			}
			ch <- result{info: info}
		}(u)
	}
	var lastErr error
	for range androidManifestURLs {
		r := <-ch
		if r.err == nil {
			return r.info, nil
		}
		lastErr = r.err
	}
	return appUpdateInfo{}, lastErr
}

// appUpdateHandler GET /app-update → 清单 JSON，附 Mac 代理下载地址
func appUpdateHandler(w http.ResponseWriter, r *http.Request) {
	info, err := fetchAppUpdate()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"error":%q}`, "更新清单尚未发布或网络不可达")
		return
	}
	host := r.Host
	fmt.Fprintf(w, `{"versionCode":%d,"versionName":%q,"apkUrl":%q,"changelog":%q,"apkProxyUrl":"http://%s/app-apk"}`,
		info.VersionCode, info.VersionName, info.ApkURL, info.Changelog, host)
}

// localAPKCandidates 本地 APK 候选路径（优先 ~/.voicekey/VoiceKey.apk）。
// 命中本地文件就直接局域网内发给手机，完全不依赖外网/GitHub。
func localAPKCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".voicekey", "VoiceKey.apk"),
		filepath.Join(home, "Library", "Application Support", "voicekey", "VoiceKey.apk"),
	}
}

// findLocalAPK 返回第一个存在的本地 APK 路径
func findLocalAPK() string {
	for _, p := range localAPKCandidates() {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Size() > 1<<20 {
			return p
		}
	}
	return ""
}

// appApkHandler GET /app-apk → 优先本地文件直发（局域网），缺失时回退代理远程 apkUrl
func appApkHandler(w http.ResponseWriter, r *http.Request) {
	if local := findLocalAPK(); local != "" {
		ver := "local"
		if info, err := fetchAppUpdate(); err == nil {
			ver = info.VersionName
		}
		w.Header().Set("Content-Type", "application/vnd.android.package-archive")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=voicekey-%s.apk", ver))
		log.Println("App 更新下载：使用本地 APK", local)
		http.ServeFile(w, r, local)
		return
	}
	info, err := fetchAppUpdate()
	if err != nil || info.ApkURL == "" {
		http.Error(w, "no apk url", http.StatusServiceUnavailable)
		return
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	resp, err := client.Get(info.ApkURL)
	if err != nil {
		http.Error(w, "fetch apk failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream "+resp.Status, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	if ct := resp.Header.Get("Content-Length"); ct != "" {
		w.Header().Set("Content-Length", ct)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=voicekey-%s.apk", info.VersionName))
	io.Copy(w, resp.Body)
}
