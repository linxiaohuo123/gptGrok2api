package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 回归：/api/images 的响应形状必须与前端 GalleryResponse 契约逐一对应。
//
// 前端 galleryQueryRuntime 直接读 data.facets.media_types、data.facets.tags、
// data.total_size_bytes、data.capabilities.genbox_push。字段缺失不是"少显示一块"，
// 而是对 undefined 取属性直接抛 TypeError，被 usePageQuery 的 catch 吞成一条 toast，
// 页面永远停在加载态——这正是图库页此前完全打不开的原因。
//
// 字段清单以 web-vue/src/api/gallery.ts 为唯一依据，本测试就是那份 TS 定义的镜像。
func TestGalleryContractMatchesFrontendShape(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.ImageDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ImageDataDir, "image-one.png"), []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := New(cfg).Handler()

	// 前端 galleryApi.getFiles 发送的正是 page / page_size。
	res := adminRequest(handler, http.MethodGet, "/api/images?page=1&page_size=24&media_type=all", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", res.Code, res.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}

	// ---- 顶层：GalleryResponse ----
	for _, key := range []string{
		"schema_version", "generated_at", "items", "total", "total_size_bytes",
		"retention_hours", "capabilities", "facets", "media_type",
		"page", "page_size", "page_count", "has_more",
	} {
		if _, ok := payload[key]; !ok {
			t.Errorf("顶层缺字段 %q —— 前端读它会抛 TypeError", key)
		}
	}

	facets, ok := payload["facets"].(map[string]any)
	if !ok {
		t.Fatalf("facets 必须是对象，实际 %T", payload["facets"])
	}
	mediaTypes, ok := facets["media_types"].(map[string]any)
	if !ok {
		t.Fatalf("facets.media_types 必须是对象，实际 %T —— 前端读 .all 会抛 TypeError", facets["media_types"])
	}
	for _, key := range []string{"all", "image"} {
		if _, ok := mediaTypes[key].(float64); !ok {
			t.Errorf("facets.media_types.%s 必须是数字，实际 %T", key, mediaTypes[key])
		}
	}
	if _, ok := facets["tags"].([]any); !ok {
		t.Errorf("facets.tags 必须是数组，实际 %T", facets["tags"])
	}
	capabilities, ok := payload["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities 必须是对象，实际 %T", payload["capabilities"])
	}
	if _, ok := capabilities["genbox_push"].(bool); !ok {
		t.Errorf("capabilities.genbox_push 必须是布尔，实际 %T", capabilities["genbox_push"])
	}

	// ---- 每一行：GalleryRow ----
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items 必须是恰好含一张图的数组，实际 %T len=%d", payload["items"], len(items))
	}
	row, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] 必须是对象，实际 %T", items[0])
	}
	for _, key := range []string{
		"id", "path", "filename", "url", "thumbnail_url", "size_bytes",
		"created_at", "date", "media_type", "expired", "expires_at",
		"expires_in_seconds", "tags", "storage", "local", "webdav",
		"available", "width", "height", "genbox_push",
	} {
		if _, ok := row[key]; !ok {
			t.Errorf("items[0] 缺字段 %q —— 前端 GalleryRow 契约要求它存在", key)
		}
	}
	if _, ok := row["size_bytes"].(float64); !ok {
		t.Errorf("size_bytes 必须是数字，实际 %T", row["size_bytes"])
	}
	if got := stringValue(row["filename"]); got != "image-one.png" {
		t.Errorf("filename 应为 image-one.png，实际 %q", got)
	}
	if _, ok := row["tags"].([]any); !ok {
		t.Errorf("items[0].tags 必须是数组（不能是 null），实际 %T", row["tags"])
	}
	if got := stringValue(row["media_type"]); got != "image" {
		t.Errorf("media_type 应为 image，实际 %q", got)
	}
	if _, ok := row["expired"].(bool); !ok {
		t.Errorf("expired 必须是布尔，实际 %T", row["expired"])
	}
	// width/height 在缺元数据时必须是 null，而不是 0——0 会被界面当成真实尺寸。
	if value, exists := row["width"]; !exists || value != nil {
		t.Errorf("缺元数据时 width 应为 null，实际 %v", value)
	}
}

// 回归：图库分页必须认 page/page_size——那是前端实际发送的形态。
//
// 此前后端只读 offset/limit，前端发 page/page_size 时参数被完全忽略，
// 于是无论翻到第几页拿到的都是全部数据，且 page_count 恒为 1。
func TestGalleryAcceptsFrontendPaginationParams(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.ImageDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	for index, name := range []string{"a.png", "b.png", "c.png", "d.png", "e.png"} {
		path := filepath.Join(cfg.ImageDataDir, name)
		if err := os.WriteFile(path, []byte("png"), 0o644); err != nil {
			t.Fatal(err)
		}
		// 拉开修改时间，保证排序稳定可预期。
		stamp := base.Add(time.Duration(index) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(cfg).Handler()

	read := func(query string) map[string]any {
		t.Helper()
		res := adminRequest(handler, http.MethodGet, "/api/images?"+query, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("%s 状态码 %d: %s", query, res.Code, res.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}

	second := read("page=2&page_size=2")
	items, _ := second["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("第 2 页每页 2 条应为 2 条，实际 %d —— page/page_size 未生效", len(items))
	}
	if got := intValue(second["page"]); got != 2 {
		t.Errorf("page 应为 2，实际 %d", got)
	}
	if got := intValue(second["page_size"]); got != 2 {
		t.Errorf("page_size 应为 2，实际 %d", got)
	}
	if got := intValue(second["page_count"]); got != 3 {
		t.Errorf("5 条每页 2 条应有 3 页，实际 %d", got)
	}
	if got := intValue(second["total"]); got != 5 {
		t.Errorf("total 应为 5，实际 %d", got)
	}
	if hasMore, _ := second["has_more"].(bool); !hasMore {
		t.Error("第 2 页之后还有数据，has_more 应为 true")
	}

	last := read("page=3&page_size=2")
	lastItems, _ := last["items"].([]any)
	if len(lastItems) != 1 {
		t.Fatalf("最后一页应为 1 条，实际 %d", len(lastItems))
	}
	if hasMore, _ := last["has_more"].(bool); hasMore {
		t.Error("最后一页的 has_more 应为 false")
	}

	// 极值参数不得让服务 panic——复用 pageBounds 的边界保证。
	for _, query := range []string{
		"page=9223372036854775807&page_size=24",
		"page=-1&page_size=-1",
		"page=0&page_size=0",
		"offset=999999&limit=24",
	} {
		read(query)
	}
}

// 回归：图库必须认 start_date / end_date——前端的选择器发送这两个参数。
func TestGalleryAcceptsDateRangeFilter(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.ImageDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(cfg.ImageDataDir, "old.png")
	newPath := filepath.Join(cfg.ImageDataDir, "new.png")
	for _, path := range []string{oldPath, newPath} {
		if err := os.WriteFile(path, []byte("png"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	recent := time.Now().UTC()
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, recent, recent); err != nil {
		t.Fatal(err)
	}
	handler := New(cfg).Handler()

	res := adminRequest(handler, http.MethodGet, "/api/images?start_date=2024-01-01&end_date=2024-12-31", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", res.Code, res.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := intValue(payload["total"]); got != 1 {
		t.Fatalf("日期区间内应只剩 1 张，实际 %d —— start_date/end_date 未生效", got)
	}
	items, _ := payload["items"].([]any)
	row, _ := items[0].(map[string]any)
	if got := stringValue(row["filename"]); got != "old.png" {
		t.Fatalf("日期筛选命中了错误的文件：%q", got)
	}
}
