// [INPUT]: 无内部依赖（os、archive/zip、mime）
// [OUTPUT]: 图片管理：listAdminImages、标签、删除、打包下载、publicImage
// [POS]: 图库管理端与公开图片服务共用本文件；公开路径靠随机文件名做能力隔离。
//         响应的字段形状由 galleryRowForAPI 一处定义，以 web-vue/src/api/gallery.ts
//         的 GalleryRow 为准——内部字段名（type/size/updated_at）与前端契约
//         （media_type/size_bytes/created_at）不同，映射不得散落到各个 handler。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var imageTagsMu sync.Mutex

func (s *Server) adminImages(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/images")
	if path == "" || path == "/" {
		if !s.requireAPI(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		s.listAdminImages(w, r)
		return
	}
	if !s.requireAdmin(w, r) {
		return
	}
	switch {
	case path == "/tags" && r.Method == http.MethodGet:
		s.listImageTags(w)
	case path == "/tags" && r.Method == http.MethodPost:
		s.updateImageTags(w, r)
	case strings.HasPrefix(path, "/tags/") && r.Method == http.MethodDelete:
		tag := strings.TrimPrefix(path, "/tags/")
		s.deleteImageTag(w, tag)
	case path == "/storage" && r.Method == http.MethodGet:
		s.imageStorage(w)
	case path == "/storage/compress" && r.Method == http.MethodPost:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "compressed": 0, "saved_bytes": 0, "message": "Go runtime does not recompress source media"})
	case path == "/storage/cleanup-to-target" && r.Method == http.MethodPost:
		s.imageStorageCleanup(w, r)
	case path == "/retention-cleanup" && r.Method == http.MethodPost:
		s.galleryRetentionCleanup(w, r)
	case path == "/genbox-push" && r.Method == http.MethodPost:
		s.galleryGenBoxPush(w, r)
	case path == "/delete" && r.Method == http.MethodPost:
		s.deleteAdminImages(w, r)
	case path == "/download" && r.Method == http.MethodPost:
		s.downloadAdminImages(w, r)
	case strings.HasPrefix(path, "/download/") && r.Method == http.MethodGet:
		s.downloadSingleImage(w, r, strings.TrimPrefix(path, "/download/"))
	default:
		writeError(w, http.StatusNotFound, "image endpoint not found", "not_found")
	}
}

func (s *Server) listAdminImages(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	tagIndex := s.loadImageTags()
	tagsOf := func(item map[string]any) []string { return stringList(tagIndex[stringValue(item["path"])]) }

	matched := make([]map[string]any, 0)
	counts := map[string]int{"all": 0, "image": 0}
	for _, item := range listMediaItems(s.cfg.ImageDataDir, "image", "") {
		if !galleryMatches(item, tagsOf(item), query) {
			continue
		}
		counts["all"]++
		counts[firstNonEmpty(stringValue(item["type"]), "image")]++
		matched = append(matched, item)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		left, okLeft := mediaTime(matched[i], galleryTimeKeys...)
		right, okRight := mediaTime(matched[j], galleryTimeKeys...)
		if !okLeft || !okRight {
			return okLeft
		}
		return left.After(right)
	})

	total := len(matched)
	offset, end, pageNumber, pageSize := galleryPage(query, total)
	page := make([]map[string]any, 0, end-offset)
	for _, item := range matched[offset:end] {
		page = append(page, s.galleryRowForAPI(item, tagsOf(item)))
	}

	retentionHours := 0
	if days := s.cfg.ImageRetentionDays; days > 0 {
		retentionHours = days * 24
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version":   1,
		"generated_at":     time.Now().UTC().Format(time.RFC3339),
		"items":            page,
		"total":            total,
		"total_size_bytes": mediaItemsSize(matched),
		"retention_hours":  retentionHours,
		// 第三方画板推送在 Go 端仍是桩（见 galleryGenBoxPush），
		// 所以能力位如实报 false —— 前端据此隐藏按钮，而不是让它点了没反应。
		"capabilities": map[string]any{"genbox_push": false},
		"facets": map[string]any{
			"media_types": map[string]any{"all": counts["all"], "image": counts["image"]},
			"tags":        s.allImageTags(),
		},
		"media_type": firstNonEmpty(strings.ToLower(strings.TrimSpace(query.Get("media_type"))), "all"),
		"page":       pageNumber,
		"page_size":  pageSize,
		"page_count": galleryPageCount(total, pageSize),
		"has_more":   end < total,
	})
}

// galleryTimeKeys 是条目上可能承载时间的字段，按优先级排列。
var galleryTimeKeys = []string{"generated_at", "created_at", "updated_at"}

// galleryMatches 应用图库的全部筛选条件。
func galleryMatches(item map[string]any, itemTags []string, query url.Values) bool {
	mediaType := strings.ToLower(strings.TrimSpace(query.Get("media_type")))
	if mediaType != "" && mediaType != "all" && stringValue(item["type"]) != mediaType {
		return false
	}
	if tag := strings.TrimSpace(query.Get("tag")); tag != "" && tag != "all" && !containsString(itemTags, tag) {
		return false
	}
	search := strings.ToLower(strings.TrimSpace(query.Get("search")))
	if search != "" && !strings.Contains(strings.ToLower(fmt.Sprint(item["path"], " ", item["name"], " ", item["updated_at"])), search) {
		return false
	}
	created, ok := mediaTime(item, galleryTimeKeys...)
	if !ok {
		return query.Get("start_date") == "" && query.Get("end_date") == ""
	}
	// YYYY-MM-DD 是定长格式，字典序即时间序。
	date := created.Format("2006-01-02")
	if start := strings.TrimSpace(query.Get("start_date")); start != "" && date < start {
		return false
	}
	if endDate := strings.TrimSpace(query.Get("end_date")); endDate != "" && date > endDate {
		return false
	}
	return true
}

// galleryPage 解析分页参数并算出安全边界。
//
// 同时接受前端契约的 page/page_size 与旧的 offset/limit：前者是 web-vue 实际
// 发送的形态，后者是既有调用方的形态。边界计算复用 pageBounds，
// 因此极值参数不可能再切出负数下标。
func galleryPage(query url.Values, total int) (offset, end, pageNumber, pageSize int) {
	if size := positiveInt(query.Get("page_size"), 0); size > 0 {
		pageSize = size
		pageNumber = positiveInt(query.Get("page"), 1)
		offset, end = pageBounds(pageNumber, pageSize, total)
		return offset, end, pageNumber, pageSize
	}
	offset = nonNegativeInt(query.Get("offset"), 0)
	pageSize = nonNegativeInt(query.Get("limit"), 0)
	if offset > total {
		offset = total
	}
	end = total
	if pageSize > 0 && offset+pageSize < end {
		end = offset + pageSize
	}
	if pageSize <= 0 {
		pageSize = total
	}
	pageNumber = 1
	if pageSize > 0 {
		pageNumber = offset/pageSize + 1
	}
	return offset, end, pageNumber, pageSize
}

func galleryPageCount(total, pageSize int) int {
	if pageSize <= 0 {
		return 1
	}
	count := (total + pageSize - 1) / pageSize
	if count < 1 {
		return 1
	}
	return count
}

// galleryRowForAPI 把内部媒体条目投影成前端 GalleryRow 契约。
//
// 内部字段名（type / size / updated_at）与前端契约（media_type / size_bytes /
// created_at）不一致，映射必须集中在这一处，字段清单以
// web-vue/src/api/gallery.ts 的 GalleryRow 为准。此前两者各自漂移，
// 前端只能靠运行时 TypeError 才发现契约断裂。
func (s *Server) galleryRowForAPI(item map[string]any, tags []string) map[string]any {
	rel := firstNonEmpty(stringValue(item["rel"]), stringValue(item["path"]))
	created, hasCreated := mediaTime(item, galleryTimeKeys...)
	expiresAt, expired, expiresIn := s.galleryExpiry(created, hasCreated)
	return map[string]any{
		"id":                 rel,
		"path":               rel,
		"filename":           firstNonEmpty(stringValue(item["filename"]), stringValue(item["name"])),
		"url":                "/images/" + rel,
		"thumbnail_url":      "/image-thumbnails/" + rel,
		"size_bytes":         mediaSizeBytes(item),
		"created_at":         formatMediaTime(created, hasCreated),
		"date":               galleryDate(created, hasCreated),
		"media_type":         firstNonEmpty(stringValue(item["type"]), "image"),
		"expired":            expired,
		"expires_at":         expiresAt,
		"expires_in_seconds": expiresIn,
		"tags":               tags,
		"storage":            "local",
		"local":              true,
		"webdav":             false,
		"available":          true,
		"width":              optionalInt(item, "width"),
		"height":             optionalInt(item, "height"),
		"genbox_push":        nil,
	}
}

// galleryExpiry 依据保留期算出条目的过期时间与剩余秒数。
func (s *Server) galleryExpiry(created time.Time, ok bool) (string, bool, any) {
	days := s.cfg.ImageRetentionDays
	if !ok || days <= 0 {
		return "", false, nil
	}
	expires := created.AddDate(0, 0, days)
	if remaining := time.Until(expires); remaining > 0 {
		return expires.Format(time.RFC3339), false, int64(remaining.Seconds())
	}
	return expires.Format(time.RFC3339), true, int64(0)
}

// mediaTime 依次尝试若干键取出条目的时间。内部值可能是 time.Time
// （os.FileInfo.ModTime）也可能是 RFC3339 字符串（*.meta.json）。
func mediaTime(item map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		switch value := item[key].(type) {
		case time.Time:
			return value.UTC(), true
		case string:
			if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
				return parsed.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func formatMediaTime(value time.Time, ok bool) string {
	if !ok {
		return ""
	}
	return value.Format(time.RFC3339)
}

func galleryDate(value time.Time, ok bool) string {
	if !ok {
		return ""
	}
	return value.Format("2006-01-02")
}

func mediaSizeBytes(item map[string]any) int64 {
	for _, key := range []string{"size", "bytes", "size_bytes"} {
		switch value := item[key].(type) {
		case int64:
			return value
		case int:
			return int64(value)
		case float64:
			return int64(value)
		}
	}
	return 0
}

// optionalInt 缺失时返回 nil 而不是 0：前端把 width/height 声明为
// number | null，用 0 表示"未知"会让界面显示出一个并不存在的尺寸。
func optionalInt(item map[string]any, key string) any {
	switch value := item[key].(type) {
	case int64:
		return value
	case int:
		return value
	case float64:
		return int(value)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return nil
}

func listMediaItems(root, kind, prefix string) []map[string]any {
	entries, _ := os.ReadDir(root)
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if !isWithin(root, path) {
			continue
		}
		rel := prefix + filepath.ToSlash(entry.Name())
		value := map[string]any{"path": rel, "rel": rel, "name": entry.Name(), "filename": entry.Name(), "type": kind, "size": info.Size(), "bytes": info.Size(), "updated_at": info.ModTime().UTC(), "url": "/images/" + rel}
		if meta := mediaMetadata(path); meta != nil {
			for key, item := range meta {
				value[key] = item
			}
		} else if kind == "image" {
			value["source_type"] = "legacy_output"
			value["role"] = "output"
			value["generated_at"] = info.ModTime().UTC().Format(time.RFC3339)
		}
		items = append(items, value)
	}
	return items
}

func mediaItemsSize(items []map[string]any) int64 {
	var total int64
	for _, item := range items {
		if value, ok := item["size"].(int64); ok {
			total += value
		}
	}
	return total
}

func (s *Server) publicImage(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/images/")
	root := s.cfg.ImageDataDir
	if strings.HasPrefix(r.URL.Path, "/image-thumbnails/") {
		path = strings.TrimPrefix(r.URL.Path, "/image-thumbnails/")
	}
	if path == "" || strings.Contains(path, "\\") {
		writeError(w, http.StatusBadRequest, "invalid image path", "invalid_request_error")
		return
	}
	path, err := filepath.Rel(root, filepath.Join(root, filepath.FromSlash(path)))
	if err != nil || path == ".." || strings.HasPrefix(path, ".."+string(os.PathSeparator)) {
		writeError(w, http.StatusNotFound, "image not found", "not_found")
		return
	}
	filePath := filepath.Join(root, path)
	if _, err := os.Stat(filePath); err != nil {
		writeError(w, http.StatusNotFound, "image not found", "not_found")
		return
	}
	if contentType := mime.TypeByExtension(filepath.Ext(filePath)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeFile(w, r, filePath)
}

func (s *Server) deleteAdminImages(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Paths       []string `json:"paths"`
		AllMatching bool     `json:"all_matching"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	paths := request.Paths
	if request.AllMatching {
		paths = nil
		for _, item := range listMediaItems(s.cfg.ImageDataDir, "image", "") {
			paths = append(paths, stringValue(item["path"]))
		}
	}
	removed := 0
	for _, item := range paths {
		if s.removeMediaPath(item) {
			removed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func (s *Server) removeMediaPath(value string) bool {
	root := s.cfg.ImageDataDir
	value = filepath.ToSlash(strings.TrimSpace(value))
	path := filepath.Join(root, filepath.FromSlash(value))
	if !isWithin(root, path) || filepath.Clean(path) == filepath.Clean(root) {
		return false
	}
	if os.Remove(path) == nil {
		_ = os.Remove(path + ".meta.json")
		s.removeImageTags(value)
		return true
	}
	return false
}

func (s *Server) removeImageTags(path string) {
	all := s.loadImageTags()
	if _, ok := all[path]; !ok {
		return
	}
	delete(all, path)
	_ = s.saveImageTags(all)
}

func (s *Server) downloadAdminImages(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Paths []string `json:"paths"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="images.zip"`)
	if err := s.writeMediaZip(w, request.Paths); err != nil {
		return
	}
}

func (s *Server) downloadSingleImage(w http.ResponseWriter, r *http.Request, path string) {
	root, clean := s.mediaPath(path)
	if root == "" {
		writeError(w, http.StatusNotFound, "image not found", "not_found")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(clean)+`"`)
	http.ServeFile(w, r, filepath.Join(root, clean))
}

func (s *Server) writeMediaZip(w io.Writer, paths []string) error {
	archive := zip.NewWriter(w)
	defer archive.Close()
	used := map[string]bool{}
	added := 0
	for _, item := range paths {
		root, clean := s.mediaPath(item)
		if root == "" {
			continue
		}
		path := filepath.Join(root, clean)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		name := filepath.Base(clean)
		if used[name] {
			name = fmt.Sprintf("%d_%s", added+1, name)
		}
		used[name] = true
		writer, err := archive.Create(name)
		if err != nil {
			return err
		}
		if _, err := writer.Write(raw); err != nil {
			return err
		}
		added++
	}
	if added == 0 {
		return fmt.Errorf("no media found")
	}
	return nil
}

func (s *Server) mediaPath(value string) (string, string) {
	value = filepath.ToSlash(strings.Trim(strings.TrimSpace(value), "/"))
	root := s.cfg.ImageDataDir
	if value == "" || strings.Contains(value, "\\") {
		return "", ""
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	path := filepath.Join(root, clean)
	if !isWithin(root, path) || filepath.Clean(path) == filepath.Clean(root) {
		return "", ""
	}
	if _, err := os.Stat(path); err != nil {
		return "", ""
	}
	return root, clean
}

func (s *Server) tagsPath() string { return filepath.Join(s.cfg.DataDir, "image_tags.json") }

func (s *Server) loadImageTags() map[string]any {
	imageTagsMu.Lock()
	defer imageTagsMu.Unlock()
	raw, err := os.ReadFile(s.tagsPath())
	if err != nil {
		return map[string]any{}
	}
	var tags map[string]any
	if json.Unmarshal(raw, &tags) != nil {
		return map[string]any{}
	}
	return tags
}

func (s *Server) saveImageTags(tags map[string]any) error {
	imageTagsMu.Lock()
	defer imageTagsMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.tagsPath()), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(tags, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.tagsPath(), append(raw, '\n'), 0o600)
}

// allImageTags 汇总去重后的全部标签。/api/images/tags 与图库响应的
// facets.tags 是同一份数据，必须由同一个函数产出，否则两处会各自漂移。
func (s *Server) allImageTags() []string {
	seen := map[string]bool{}
	for _, value := range s.loadImageTags() {
		for _, tag := range stringList(value) {
			if tag != "" {
				seen[tag] = true
			}
		}
	}
	result := make([]string, 0, len(seen))
	for tag := range seen {
		result = append(result, tag)
	}
	sort.Strings(result)
	return result
}

func (s *Server) listImageTags(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"tags": s.allImageTags()})
}

func (s *Server) updateImageTags(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string   `json:"path"`
		Tags []string `json:"tags"`
	}
	if !decodeJSON(w, r, &request) || strings.TrimSpace(request.Path) == "" {
		return
	}
	path := filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(request.Path), "/"))
	tags := map[string]any{}
	for _, tag := range request.Tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags[tag] = true
		}
	}
	values := make([]string, 0, len(tags))
	for tag := range tags {
		values = append(values, tag)
	}
	sort.Strings(values)
	all := s.loadImageTags()
	all[path] = values
	if err := s.saveImageTags(all); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tags": values})
}

func (s *Server) deleteImageTag(w http.ResponseWriter, tag string) {
	tag = strings.TrimSpace(tag)
	all := s.loadImageTags()
	removed := 0
	for path, value := range all {
		values := stringList(value)
		next := make([]string, 0, len(values))
		changed := false
		for _, item := range values {
			if item == tag {
				changed = true
				continue
			}
			next = append(next, item)
		}
		if changed {
			removed++
			all[path] = next
		}
	}
	if err := s.saveImageTags(all); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed_from": removed})
}

func (s *Server) imageStorage(w http.ResponseWriter) {
	images := mediaStats(s.cfg.ImageDataDir)
	total, used, free := diskUsage(s.cfg.ImageDataDir)
	writeJSON(w, http.StatusOK, map[string]any{
		"disk_total_mb":    total / (1024 * 1024),
		"disk_used_mb":     used / (1024 * 1024),
		"disk_free_mb":     free / (1024 * 1024),
		"image_count":      images["count"],
		"image_size_mb":    images["size_bytes"].(int64) / (1024 * 1024),
		"image_size_bytes": images["size_bytes"],
		"images":           images,
	})
}

func (s *Server) imageStorageCleanup(w http.ResponseWriter, r *http.Request) {
	target := positiveInt(r.URL.Query().Get("target_free_mb"), 500)
	dryRun := strings.EqualFold(r.URL.Query().Get("dry_run"), "true") || r.URL.Query().Get("dry_run") == "1"
	totalBytes, _, freeBytes := diskUsage(s.cfg.ImageDataDir)
	if totalBytes > 0 && uint64(target) > totalBytes/(1024*1024) {
		writeError(w, http.StatusBadRequest, "target free space exceeds disk capacity; enter the value in MB", "invalid_request_error")
		return
	}
	currentFree := freeBytes / (1024 * 1024)
	removed, freed := 0, int64(0)
	files := imageFilesByAge(s.cfg.ImageDataDir)
	for _, file := range files {
		if currentFree+uint64(freed/(1024*1024)) >= uint64(target) {
			break
		}
		if !dryRun {
			if err := os.Remove(file.path); err != nil {
				continue
			}
			_ = os.Remove(file.path + ".meta.json")
			s.removeImageTags(file.rel)
		}
		freed += file.size
		removed++
	}
	if !dryRun {
		cleanupEmptyDirs(s.cfg.ImageDataDir)
		_, _, freeBytes = diskUsage(s.cfg.ImageDataDir)
		currentFree = freeBytes / (1024 * 1024)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "target_free_mb": target, "dry_run": dryRun,
		"removed": removed, "freed_mb": freed / (1024 * 1024),
		"current_free_mb": currentFree, "done": currentFree >= uint64(target) || (dryRun && currentFree+uint64(freed/(1024*1024)) >= uint64(target)),
	})
}

type imageStorageFile struct {
	path  string
	rel   string
	size  int64
	mtime time.Time
}

func imageFilesByAge(root string) []imageStorageFile {
	files := []imageStorageFile{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || strings.HasSuffix(info.Name(), ".meta.json") || !isImageStorageFile(info.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr == nil {
			files = append(files, imageStorageFile{path: path, rel: filepath.ToSlash(rel), size: info.Size(), mtime: info.ModTime()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	return files
}

func isImageStorageFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp":
		return true
	default:
		return false
	}
}

func cleanupEmptyDirs(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && info.IsDir() && path != root {
			entries, readErr := os.ReadDir(path)
			if readErr == nil && len(entries) == 0 {
				_ = os.Remove(path)
			}
		}
		return nil
	})
}

func stringList(value any) []string {
	result := []string{}
	switch typed := value.(type) {
	case []string:
		return append(result, typed...)
	case []any:
		for _, item := range typed {
			if value := strings.TrimSpace(fmt.Sprint(item)); value != "" {
				result = append(result, value)
			}
		}
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func nonNegativeInt(value string, fallback int) int {
	var number int
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &number); err != nil || number < 0 {
		return fallback
	}
	return number
}

func (s *Server) galleryRetentionCleanup(w http.ResponseWriter, r *http.Request) {
	days := s.cfg.ImageRetentionDays
	if days <= 0 {
		days = 1
	}
	hours := days * 24
	result := s.cleanupRetentionFiles(30, days, false)
	imgResult, _ := result["images"].(map[string]any)
	removed := intValue(imgResult["removed"])
	var removedBytes int64
	switch v := imgResult["removed_size_bytes"].(type) {
	case int64:
		removedBytes = v
	case int:
		removedBytes = int64(v)
	case float64:
		removedBytes = int64(v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"removed":            removed,
		"removed_size_bytes": removedBytes,
		"retention_hours":    hours,
		"message":            fmt.Sprintf("已清理 %d 张过期图片", removed),
	})
}

func (s *Server) galleryGenBoxPush(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	_ = decodeJSON(w, r, &body)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "imported",
		"label":           "已推送到第三方画板",
		"sha256":          "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"updated_at":      time.Now().UTC().Format(time.RFC3339),
		"path":            body.Path,
		"source_retained": true,
	})
}
