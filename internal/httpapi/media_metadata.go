// [INPUT]: 无内部依赖（os、encoding/json）
// [OUTPUT]: 图片元数据与标签：recordGeneratedMedia、mediaMetadata
// [POS]: 图片落盘时同步写元数据；标签文件读改写需整体加锁。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"context"
	"encoding/json"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var generatedMediaMetaMu sync.Mutex

func (s *Server) recordGeneratedMedia(ctx context.Context, result map[string]string) {
	rawURL := strings.TrimSpace(result["url"])
	if rawURL == "" {
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	id := strings.TrimSpace(parsed.Query().Get("id"))
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(parsed.Path), filepath.Ext(parsed.Path))
	}
	entries, _ := os.ReadDir(s.cfg.ImageDataDir)
	filename := ""
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), id+".") && !strings.HasSuffix(entry.Name(), ".meta.json") {
			filename = entry.Name()
			break
		}
	}
	if filename == "" {
		return
	}
	callID, _ := ctx.Value(monitorCallIDKey{}).(string)
	endpoint, model := "", ""
	if callID != "" {
		if record, ok := s.monitor.detail(callID); ok {
			endpoint = record.Endpoint
			model = record.Model
		}
	}
	width, _ := strconv.Atoi(result["width"])
	height, _ := strconv.Atoi(result["height"])
	if width <= 0 || height <= 0 {
		filePath := filepath.Join(s.cfg.ImageDataDir, filename)
		if f, err := os.Open(filePath); err == nil {
			if cfg, _, err := image.DecodeConfig(f); err == nil && cfg.Width > 0 && cfg.Height > 0 {
				width = cfg.Width
				height = cfg.Height
			}
			_ = f.Close()
		}
	}
	meta := map[string]any{
		"call_id":      callID,
		"endpoint":     endpoint,
		"model":        model,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"source_type":  "generated_output",
		"role":         "output",
		"width":        width,
		"height":       height,
	}
	b, _ := json.Marshal(meta)
	generatedMediaMetaMu.Lock()
	defer generatedMediaMetaMu.Unlock()
	_ = os.WriteFile(filepath.Join(s.cfg.ImageDataDir, filename+".meta.json"), b, 0o600)
}

func mediaMetadata(path string) map[string]any {
	b, err := os.ReadFile(path + ".meta.json")
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}
