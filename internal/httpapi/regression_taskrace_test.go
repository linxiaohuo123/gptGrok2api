package httpapi

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// 回归：任务读取路径必须与 worker 的写入路径正确同步。
//
// imageTaskPublic / editableTaskPublic 读的每个字段都在被 worker 改写。
// 曾经的写法是"锁内取指针或做浅拷贝 → 出锁后再投影"——浅拷贝只复制结构体，
// Data 切片、Result map 仍与 worker 共享，等于没加锁。这类缺陷只在 -race 下
// 现形，所以本测试同时给读写两侧施压，把它交给竞态检测器。
//
// 注意：本测试只在 `go test -race` 下才有判别力，而仓库的基线命令正是 -race。
func TestImageTaskReadPathIsRaceFree(t *testing.T) {
	stamp := time.Now().UTC().Format(time.RFC3339)
	task := &imageTaskState{ID: "race-task", Status: "queued", Mode: "generate", Model: "gpt-image-2", N: 4, Size: "1024x1024", Quality: "auto", CreatedAt: stamp, UpdatedAt: stamp}
	server := taskServerWith(t, task)
	handler := server.Handler()

	var wg sync.WaitGroup
	// 写侧：模拟 runImageTask 持锁改写任务。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				server.imageTaskMu.Lock()
				task.Status = "running"
				task.Data = append(task.Data, map[string]any{"url": "/v1/files/image?id=x", "width": "1024"})
				task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				server.imageTaskMu.Unlock()
			}
		}()
	}
	// 读侧：走真实的 HTTP 路径。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				adminRequest(handler, http.MethodGet, "/api/image-tasks", nil)
				adminRequest(handler, http.MethodGet, "/api/image-tasks/race-task", nil)
			}
		}()
	}
	wg.Wait()
}

// 同一条约束作用于可编辑文件任务：editableFileTaskByID、editableGeneration
// 与提交路径都曾把活 task 带出 fileTaskMu 之后再投影。
func TestEditableFileTaskReadPathIsRaceFree(t *testing.T) {
	stamp := time.Now().UTC().Format(time.RFC3339)
	task := &editableFileTaskState{ID: "race-file-task", TaskID: "race-file-task", Owner: "admin", Status: "queued", Kind: "ppt", CreatedAt: stamp, UpdatedAt: stamp}
	server := New(adminTestConfig(t.TempDir()))
	server.fileTaskMu.Lock()
	server.fileTasks[editableTaskKey("admin", task.ID)] = task
	server.fileTaskMu.Unlock()
	handler := server.Handler()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				server.fileTaskMu.Lock()
				task.Status = "running"
				task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				// Result 是 map，正是浅拷贝挡不住的那一层。
				task.Result = map[string]any{"files": []any{map[string]any{"name": "out.pptx"}}}
				server.fileTaskMu.Unlock()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				adminRequest(handler, http.MethodGet, "/v1/editable-file-tasks/race-file-task", nil)
				adminRequest(handler, http.MethodGet, "/v1/editable-file-tasks", nil)
			}
		}()
	}
	wg.Wait()
}
