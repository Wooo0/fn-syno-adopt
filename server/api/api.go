// Package api 提供检测阶段的只读 HTTP API。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"fnos-syno-adopt/commander"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

type SystemInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Kernel   string `json:"kernel"`
	DryRun   bool   `json:"dryRun"`
}

func systemHandler(w http.ResponseWriter, _ *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"data": SystemInfo{
			Hostname: host,
			OS:       readOSName(),
			Kernel:   commander.KernelVersion(),
			DryRun:   commander.IsDryRun(),
		},
	})
}

func readOSName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return ""
}

func disksHandler(w http.ResponseWriter, _ *http.Request) {
	candidates, disks, err := commander.DetectSynologyDisks()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"candidates": candidates,
		"disks":      disks,
	})
}

// Register 挂载全部 API 路由（只读检测 + 写操作）。
func Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/system", systemHandler)
	mux.HandleFunc("/api/disks", disksHandler)
	RegisterOps(mux)
}
