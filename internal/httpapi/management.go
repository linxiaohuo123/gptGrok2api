// [INPUT]: proxy
// [OUTPUT]: 通用任务 API 与代理配置增删改
// [POS]: 管理端的配置读写入口，写回后触发 proxyRuntime 重载。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Server) taskAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/tasks")
	if path == "" || path == "/" {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{"items": s.taskQueue.List()})
			return
		}
		if r.Method == http.MethodPost {
			var request struct {
				Kind    string         `json:"kind"`
				Payload map[string]any `json:"payload"`
			}
			if !decodeJSON(w, r, &request) {
				return
			}
			if strings.TrimSpace(request.Kind) == "" {
				writeError(w, 400, "kind is required", "invalid_request_error")
				return
			}
			writeJSON(w, 202, s.taskQueue.Submit(request.Kind, request.Payload))
			return
		}
	}
	id := strings.Trim(path, "/")
	if strings.HasSuffix(id, "/cancel") && r.Method == http.MethodPost {
		id = strings.TrimSuffix(id, "/cancel")
		if !s.taskQueue.Cancel(id) {
			writeError(w, 404, "task not found or already finished", "not_found")
			return
		}
		writeJSON(w, 200, map[string]any{"cancelled": true, "id": id})
		return
	}
	if r.Method == http.MethodGet {
		task, ok := s.taskQueue.Get(id)
		if !ok {
			writeError(w, 404, "task not found", "not_found")
			return
		}
		writeJSON(w, 200, task)
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
}

func (s *Server) proxyProfiles(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	cfg, _ := s.store.Config()
	profiles := mapList(cfg["proxy_profiles"])
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"profiles": profiles})
	case http.MethodPost:
		var request map[string]any
		if !decodeJSON(w, r, &request) {
			return
		}
		id := slugID(stringValue(request["id"]))
		if id == "" {
			id = fmt.Sprintf("proxy-%d", time.Now().UnixNano())
		}
		request["id"] = id
		next := make([]map[string]any, 0, len(profiles))
		replaced := false
		for _, item := range profiles {
			if stringValue(item["id"]) == id {
				next = append(next, request)
				replaced = true
			} else {
				next = append(next, item)
			}
		}
		if !replaced {
			next = append(next, request)
		}
		updated, err := s.store.UpdateConfig("proxy_profiles", next)
		if err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		writeJSON(w, 200, map[string]any{"profile": request, "profiles": mapList(updated["proxy_profiles"])})
	default:
		writeError(w, 405, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) proxyGroups(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	cfg, _ := s.store.Config()
	groups := mapList(cfg["proxy_groups"])
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"groups": groups})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var request map[string]any
	if !decodeJSON(w, r, &request) {
		return
	}
	id := slugID(stringValue(request["id"]))
	if id == "" {
		id = fmt.Sprintf("group-%d", time.Now().UnixNano())
	}
	request["id"] = id
	next := make([]map[string]any, 0, len(groups))
	found := false
	for _, item := range groups {
		if stringValue(item["id"]) == id {
			next = append(next, request)
			found = true
		} else {
			next = append(next, item)
		}
	}
	if !found {
		next = append(next, request)
	}
	updated, err := s.store.UpdateConfig("proxy_groups", next)
	if err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	if err := s.refreshProxyRuntime(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, 200, map[string]any{"group": request, "groups": mapList(updated["proxy_groups"])})
}

func (s *Server) proxyHealth(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "runtime": "go", "checked_at": time.Now().UTC(), "proxy": s.proxyManager.Snapshot()})
}
