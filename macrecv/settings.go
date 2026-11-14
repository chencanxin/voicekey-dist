package main

// 触控板体验设置：持久化到 ~/.voicekey/settings.json，仪表盘 /settings 读写。
// 移动敏感度、垂直/水平滚动敏感度、滚动方向反转。

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

type touchSettings struct {
	MoveGain     float64 `json:"moveGain"`
	ScrollGainX  float64 `json:"scrollGainX"`
	ScrollGainY  float64 `json:"scrollGainY"`
	ScrollInvert bool    `json:"scrollInvert"`
}

func defaultTouchSettings() touchSettings {
	return touchSettings{MoveGain: 1.5, ScrollGainX: 1.0, ScrollGainY: 1.0, ScrollInvert: false}
}

var settingsStore = struct {
	sync.RWMutex
	cur touchSettings
}{cur: defaultTouchSettings()}

func settingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".voicekey", "settings.json")
}

// sanitizeSettings 防呆：非法/越界值回落默认
func sanitizeSettings(s touchSettings) touchSettings {
	d := defaultTouchSettings()
	if s.MoveGain < 0.2 || s.MoveGain > 3 {
		s.MoveGain = d.MoveGain
	}
	if s.ScrollGainX < 0.2 || s.ScrollGainX > 3 {
		s.ScrollGainX = d.ScrollGainX
	}
	if s.ScrollGainY < 0.2 || s.ScrollGainY > 3 {
		s.ScrollGainY = d.ScrollGainY
	}
	return s
}

func loadSettings() {
	p := settingsPath()
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return // 无文件用默认值
	}
	var s touchSettings
	if err := json.Unmarshal(data, &s); err != nil {
		log.Println("设置文件解析失败，使用默认值:", err)
		return
	}
	s = sanitizeSettings(s)
	settingsStore.Lock()
	settingsStore.cur = s
	settingsStore.Unlock()
	log.Printf("设置已载入: 移动敏感度 %.1f 滚动上下 %.1f 滚动左右 %.1f 反转 %v",
		s.MoveGain, s.ScrollGainY, s.ScrollGainX, s.ScrollInvert)
}

func currentSettings() touchSettings {
	settingsStore.RLock()
	defer settingsStore.RUnlock()
	return settingsStore.cur
}

func saveSettings(s touchSettings) error {
	s = sanitizeSettings(s)
	settingsStore.Lock()
	settingsStore.cur = s
	settingsStore.Unlock()
	p := settingsPath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// settingsHandler GET 返回当前设置；POST 保存设置（JSON 体）
func settingsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(currentSettings())
	case http.MethodPost:
		var s touchSettings
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			http.Error(w, `{"error":"JSON 解析失败"}`, http.StatusBadRequest)
			return
		}
		if err := saveSettings(s); err != nil {
			http.Error(w, `{"error":"保存失败"}`, http.StatusInternalServerError)
			return
		}
		s = currentSettings()
		log.Printf("设置已更新: 移动敏感度 %.1f 滚动上下 %.1f 滚动左右 %.1f 反转 %v",
			s.MoveGain, s.ScrollGainY, s.ScrollGainX, s.ScrollInvert)
		json.NewEncoder(w).Encode(s)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
