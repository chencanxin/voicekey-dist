package main

// v0.7.2 自更新：每 60 秒轮询发布仓库 version.txt，发现新版本则
//   1) brew upgrade voicekey（Formula 从源码构建，见 chencanxin/homebrew-tap）
//   2) 校验二进制确实被替换后 syscall.Exec 自重启（复用看门狗同款进程替换）
// 任何失败只记日志，下轮重试，绝不影响收发链路。
//
// 注意：自重启后二进制指纹变化，macOS 辅助功能授权可能失效（未签名应用
// 按指纹记录授权）。此时看门狗 selfRestartWatchdog 会自动每 60 秒重启
// 进程并在日志里给出授权指引，用户勾选一次即恢复。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// exeFingerprint 可执行文件指纹（brew 用硬链接/新文件替换二进制）
type exeFingerprint struct {
	size  int64
	mtime time.Time
}

func fingerprintSelf() exeFingerprint {
	p, err := os.Executable()
	if err != nil {
		return exeFingerprint{}
	}
	fi, err := os.Stat(p)
	if err != nil {
		return exeFingerprint{}
	}
	return exeFingerprint{size: fi.Size(), mtime: fi.ModTime()}
}

// fetchLatestVersion 按序尝试 versionCheckURLs 全部镜像，
// 取「所有成功响应里的最大版本号」——任何一个源看到新版本都算数，
// 避免单个 CDN 边缘节点缓存陈旧导致自更新被卡住。
// 每个源的明细（状态码/内容/错误）记录到 sourceResults，/diag 直读。
var sourceResults = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

func recordSource(u, info string) {
	sourceResults.Lock()
	sourceResults.m[u] = info
	sourceResults.Unlock()
}

func sourceResultsSnapshot() map[string]string {
	sourceResults.Lock()
	defer sourceResults.Unlock()
	out := make(map[string]string, len(sourceResults.m))
	for k, v := range sourceResults.m {
		out[k] = v
	}
	return out
}

func fetchLatestVersion() string {
	client := &http.Client{Timeout: 5 * time.Second}
	latest := ""
	for _, u := range versionCheckURLs {
		resp, err := client.Get(u)
		if err != nil {
			recordSource(u, "err: "+err.Error())
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			recordSource(u, "read-err: "+err.Error())
			continue
		}
		v := strings.TrimSpace(string(body))
		// api.github.com contents 接口返回 JSON，content 字段是 base64
		if strings.Contains(u, "api.github.com") {
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(body, &payload); err == nil && payload.Content != "" {
				if raw, err := base64.StdEncoding.DecodeString(
					strings.ReplaceAll(payload.Content, "\n", "")); err == nil {
					v = strings.TrimSpace(string(raw))
				}
			}
		}
		recordSource(u, fmt.Sprintf("%d %q", resp.StatusCode, v))
		if resp.StatusCode != http.StatusOK {
			continue
		}
		if v != "" && (latest == "" || isNewer(latest, v)) {
			latest = v
		}
	}
	return latest
}

// isNewer 数值比较版本号（"0.7.3" > "0.7.2"；解析失败视为不更新）
func isNewer(cur, latest string) bool {
	c, err1 := strconv.ParseFloat(cur, 64)
	l, err2 := strconv.ParseFloat(latest, 64)
	return err1 == nil && err2 == nil && l > c
}

// lastUpdateCheck 最近一次版本检查的结果（/diag 直读，联调不再靠猜）
var lastUpdateCheck = struct {
	sync.Mutex
	Latest    string `json:"latest"`
	CheckedAt string `json:"checkedAt"`
	Result    string `json:"result"` // ok / fetch-failed / newer / upgraded / skipped-*
}{Result: "never"}

func recordUpdateCheck(latest, result string) {
	lastUpdateCheck.Lock()
	lastUpdateCheck.Latest = latest
	lastUpdateCheck.CheckedAt = time.Now().Format("15:04:05")
	lastUpdateCheck.Result = result
	lastUpdateCheck.Unlock()
}

func selfUpdateLoop() {
	before := fingerprintSelf()
	check := func() {
		latest := fetchLatestVersion()
		if latest == "" {
			log.Println("自更新：版本检查失败（网络不通？），下轮重试")
			recordUpdateCheck("", "fetch-failed")
			return
		}
		if !isNewer(version, latest) {
			log.Printf("自更新：远端版本 v%s，当前 v%s，无需更新", latest, version)
			recordUpdateCheck(latest, "ok")
			return
		}
		recordUpdateCheck(latest, "newer")
		log.Printf("自更新：发现新版本 v%s（当前 v%s），开始通过 Homebrew 升级", latest, version)
		// launchd/精简 PATH 下可能没有 brew：先 LookPath，再回退固定路径
		brewPath, err := exec.LookPath("brew")
		if err != nil {
			for _, p := range []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"} {
				if _, err := os.Stat(p); err == nil {
					brewPath = p
					break
				}
			}
		}
		if brewPath == "" {
			log.Printf("自更新：未找到 Homebrew，请手动更新到 v%s", latest)
			recordUpdateCheck(latest, "skipped-no-brew")
			return
		}
		cmd := exec.Command(brewPath, "upgrade", "voicekey")
		// 给 brew 一个完整 PATH，不受父进程环境（launchd/自更新重启链）影响
		fullPath := "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
		cmd.Env = append(os.Environ(), "PATH="+fullPath)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			log.Println("自更新：brew upgrade 失败，下轮重试:", err)
			recordUpdateCheck(latest, "brew-failed")
			return
		}
		// 只有当前进程就是 brew 管理的二进制时才自重启；
		// 开发态直接跑 ./voicekey 的话，brew 换的是 /opt/homebrew 那份，
		// 重启自己没有意义。
		self, err := os.Executable()
		if err != nil {
			log.Println("自更新：无法定位自身路径，跳过自重启:", err)
			return
		}
		realSelf, _ := filepath.EvalSymlinks(self)
		realBrew, _ := filepath.EvalSymlinks("/opt/homebrew/bin/voicekey")
		if realBrew == "" || realSelf != realBrew {
			log.Printf("自更新：brew 已装好 v%s，但当前进程不是 brew 管理的二进制（%s），跳过自重启", latest, self)
			recordUpdateCheck(latest, "skipped-not-brew-binary")
			return
		}
		if after := fingerprintSelf(); after == before {
			log.Printf("自更新：brew upgrade 返回成功但二进制未变化（仍是 v%s），跳过自重启", version)
			recordUpdateCheck(latest, "skipped-unchanged")
			return
		}
		recordUpdateCheck(latest, "upgraded")
		log.Printf("自更新：已升级到 v%s，3 秒后自重启进程…", latest)
		time.Sleep(3 * time.Second)
		if err := syscall.Exec(self, os.Args, os.Environ()); err != nil {
			log.Println("自更新：自重启失败:", err)
		}
	}
	check()
	ticker := time.NewTicker(60 * time.Second)
	for range ticker.C {
		check()
	}
}
