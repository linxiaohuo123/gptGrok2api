// [INPUT]: 仅标准库（encoding/json、os、sync）
// [OUTPUT]: JSON 文件队列：New、Queue、Submit/Get/Cancel/List、worker
// [POS]: 文件队列实现。离开锁的 *Task 必须是 clone 快照；损坏文件留档不覆盖。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package tasks

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Task struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Status    string         `json:"status"`
	Progress  int            `json:"progress"`
	Payload   map[string]any `json:"payload,omitempty"`
	Result    map[string]any `json:"result,omitempty"`
	Error     string         `json:"error,omitempty"`
	CreatedAt int64          `json:"created_at"`
	UpdatedAt int64          `json:"updated_at"`
}

type Queue struct {
	path     string
	mu       sync.Mutex
	items    map[string]*Task
	wake     chan struct{}
	handlers map[string]func(*Task) (map[string]any, error)
}

type QueueAPI interface {
	Register(string, func(*Task) (map[string]any, error))
	Start(int)
	Submit(string, map[string]any) *Task
	Get(string) (Task, bool)
	Cancel(string) bool
	List() []Task
}

func New(path string) *Queue {
	q := &Queue{path: path, items: map[string]*Task{}, wake: make(chan struct{}, 1), handlers: map[string]func(*Task) (map[string]any, error){}}
	if err := q.load(); err != nil {
		// 绝不静默：读不出来的文件先留档，否则下一次 Submit 的原子替换
		// 会把全部历史任务无声抹掉。
		backup := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if renameErr := os.Rename(path, backup); renameErr == nil {
			log.Printf("task queue file unreadable, kept as %s: %v", backup, err)
		} else {
			log.Printf("task queue file unreadable and could not be kept: %v", err)
		}
	}
	return q
}

func (q *Queue) Register(kind string, handler func(*Task) (map[string]any, error)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handlers[kind] = handler
	q.signal()
}

func (q *Queue) Start(workers int) {
	if workers < 1 {
		workers = 1
	}
	for index := 0; index < workers; index++ {
		go q.worker()
	}
	q.signal()
}

func (q *Queue) Submit(kind string, payload map[string]any) *Task {
	now := time.Now().Unix()
	task := &Task{ID: taskID(), Kind: kind, Status: "queued", Payload: payload, CreatedAt: now, UpdatedAt: now}
	q.mu.Lock()
	q.items[task.ID] = task
	_ = q.saveLocked()
	// 快照必须在锁内取：worker 一旦拿到这个指针就会写 Status/UpdatedAt。
	snapshot := clone(task)
	q.mu.Unlock()
	q.signal()
	return snapshot
}

func (q *Queue) Get(id string) (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	task, ok := q.items[id]
	if !ok {
		return Task{}, false
	}
	return *clone(task), true
}

func (q *Queue) Cancel(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	task, ok := q.items[id]
	if !ok {
		return false
	}
	if task.Status == "completed" || task.Status == "failed" {
		return false
	}
	task.Status = "cancelled"
	task.UpdatedAt = time.Now().Unix()
	_ = q.saveLocked()
	return true
}

func (q *Queue) List() []Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	result := make([]Task, 0, len(q.items))
	for _, task := range q.items {
		result = append(result, *clone(task))
	}
	return result
}

func (q *Queue) worker() {
	for {
		q.mu.Lock()
		var selected *Task
		for _, task := range q.items {
			if task.Status == "queued" {
				if selected == nil || task.CreatedAt < selected.CreatedAt {
					selected = task
				}
			}
		}
		if selected != nil {
			selected.Status = "running"
			selected.UpdatedAt = time.Now().Unix()
			_ = q.saveLocked()
		}
		handler := func(*Task) (map[string]any, error) { return nil, errors.New("no task handler") }
		if selected != nil {
			if current, ok := q.handlers[selected.Kind]; ok {
				handler = current
			}
		}
		q.mu.Unlock()
		if selected == nil {
			<-q.wake
			continue
		}
		// handler 只拿快照：活指针一旦出锁，Get/List/Cancel 的克隆就会与它并发读写。
		result, err := handler(clone(selected))
		q.mu.Lock()
		if current, ok := q.items[selected.ID]; ok && current.Status == "running" {
			current.UpdatedAt = time.Now().Unix()
			if err != nil {
				current.Status = "failed"
				current.Error = err.Error()
			} else {
				current.Status = "completed"
				current.Progress = 100
				current.Result = result
			}
			_ = q.saveLocked()
		}
		q.mu.Unlock()
	}
}

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
func (q *Queue) load() error {
	raw, err := os.ReadFile(q.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var items []Task
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("decode queue file %s: %w", q.path, err)
	}
	for _, task := range items {
		if task.Status == "running" {
			task.Status = "queued"
		}
		copy := task
		q.items[task.ID] = &copy
	}
	return nil
}
func (q *Queue) saveLocked() error {
	items := make([]Task, 0, len(q.items))
	for _, task := range q.items {
		items = append(items, *task)
	}
	raw, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(q.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(q.path), ".tasks-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_, err = tmp.Write(raw)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, q.path)
}

// clone 深拷贝一份任务。
//
// Payload / Result 是任意 JSON，这里必须**递归**复制：只重建顶层 map 的话，
// 嵌套的 map 与 slice 仍与队列内的原件共享内存——而 clone 的用途正是
// 把任务交给 handler 独占使用，handler 改一个嵌套值就会改到队列里的原件。
func clone(task *Task) *Task {
	copy := *task
	copy.Payload = cloneJSONMap(task.Payload)
	copy.Result = cloneJSONMap(task.Result)
	return &copy
}

func cloneJSONMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = cloneJSONValue(value)
	}
	return output
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneJSONMap(typed)
	case []any:
		items := make([]any, len(typed))
		for index, item := range typed {
			items[index] = cloneJSONValue(item)
		}
		return items
	default:
		return value
	}
}
func taskID() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "task_" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return "task_" + base64.RawURLEncoding.EncodeToString(raw)
}
