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
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// fetchLatestVersion 按序尝试 versionCheckURLs（jsDelivr 国内可达优先）
func fetchLatestVersion() string {
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
		return strings.TrimSpace(string(body))
	}
	return ""
}

// isNewer 数值比较版本号（"0.7.3" > "0.7.2"；解析失败视为不更新）
func isNewer(cur, latest string) bool {
	c, err1 := strconv.ParseFloat(cur, 64)
	l, err2 := strconv.ParseFloat(latest, 64)
	return err1 == nil && err2 == nil && l > c
}

func selfUpdateLoop() {
	before := fingerprintSelf()
	check := func() {
		latest := fetchLatestVersion()
		if latest == "" {
			log.Println("自更新：版本检查失败（网络不通？），下轮重试")
			return
		}
		if !isNewer(version, latest) {
			log.Printf("自更新：当前已是最新版本 v%s", version)
			return
		}
		log.Printf("自更新：发现新版本 v%s（当前 v%s），开始通过 Homebrew 升级", latest, version)
		if _, err := exec.LookPath("brew"); err != nil {
			log.Printf("自更新：未安装 Homebrew，请手动更新到 v%s", latest)
			return
		}
		cmd := exec.Command("brew", "upgrade", "voicekey")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			log.Println("自更新：brew upgrade 失败，下轮重试:", err)
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
			return
		}
		if after := fingerprintSelf(); after == before {
			log.Printf("自更新：brew upgrade 返回成功但二进制未变化（仍是 v%s），跳过自重启", version)
			return
		}
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
