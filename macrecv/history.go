package main

// 发送历史：持久化手机端发来的文本段，供 App 历史列表与回填使用。
// 存储：~/.voicekey/history.jsonl（JSON Lines，启动时载入内存，最新在前）。
// 上限 500 条；连续重复防抖（10 秒内与最新一条完全相同则不记，挡 app 端重传）。

import (
	"bufio"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	historyMaxEntries = 500
	historyDedupeWin  = 10 * time.Second
)

type historyEntry struct {
	TS   string `json:"ts"`   // RFC3339，本地时区
	Text string `json:"text"` // 原文（UTF-8）
}

var historyStore = struct {
	entries []historyEntry // 最新在前
	file    string
}{}

func historyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".voicekey", "history.jsonl")
}

// loadHistory 启动时从 jsonl 载入（文件顺序为旧→新，载入后翻转为新在前）
func loadHistory() {
	historyStore.file = historyPath()
	if historyStore.file == "" {
		log.Println("历史记录: 无法确定主目录，历史功能停用")
		return
	}
	if err := os.MkdirAll(filepath.Dir(historyStore.file), 0o755); err != nil {
		log.Println("历史记录: 创建目录失败:", err)
		return
	}
	f, err := os.Open(historyStore.file)
	if err != nil {
		return // 首次运行无文件，正常
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e historyEntry
		if json.Unmarshal(scanner.Bytes(), &e) == nil && e.Text != "" {
			historyStore.entries = append(historyStore.entries, e)
		}
	}
	// 翻转为最新在前
	for i, j := 0, len(historyStore.entries)-1; i < j; i, j = i+1, j-1 {
		historyStore.entries[i], historyStore.entries[j] = historyStore.entries[j], historyStore.entries[i]
	}
	if len(historyStore.entries) > historyMaxEntries {
		historyStore.entries = historyStore.entries[:historyMaxEntries]
	}
	log.Printf("历史记录: 已载入 %d 条", len(historyStore.entries))
}

// recordHistory 记一条发送历史（内存 + 追加文件）。在 handle() 解码成功后调用。
func recordHistory(text string) {
	if historyStore.file == "" {
		return
	}
	now := time.Now()
	// 连续重复防抖：与最新一条相同且间隔很短 → 忽略（挡 app 端无 ACK 重传）
	if len(historyStore.entries) > 0 {
		newest := historyStore.entries[0]
		if newest.Text == text {
			if ts, err := time.Parse(time.RFC3339, newest.TS); err == nil && now.Sub(ts) < historyDedupeWin {
				return
			}
		}
	}
	e := historyEntry{TS: now.Format(time.RFC3339), Text: text}
	historyStore.entries = append([]historyEntry{e}, historyStore.entries...)
	if len(historyStore.entries) > historyMaxEntries {
		historyStore.entries = historyStore.entries[:historyMaxEntries]
	}
	f, err := os.OpenFile(historyStore.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Println("历史记录: 写入失败:", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.Encode(e)
}

// historyHandler GET /history?limit=N → 最新在前的 JSON 数组，N 默认 500
func historyHandler(w http.ResponseWriter, r *http.Request) {
	limit := historyMaxEntries
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
			if limit > historyMaxEntries {
				limit = historyMaxEntries
			}
		}
	}
	entries := historyStore.entries
	if len(entries) > limit {
		entries = entries[:limit]
	}
	if entries == nil {
		entries = []historyEntry{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"entries": entries})
}
