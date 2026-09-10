package httpapi

import (
	"encoding/json"
	"testing"
)

// 回归：导入任务 job map 被后台 goroutine 无锁写入，同时 HTTP 侧对它做 json.Marshal。
// 生产对应路径：POST /api/cpa/pools/{id}/import 里的 `writeJSON(..., job)`，
// 以及 GET /api/cpa/pools/{id}/import 里的 `writeJSON(..., item.ImportJob)`。
func TestProofImportJobMapRace(t *testing.T) {
	server := &Server{external: newExternalManager(t.TempDir())}
	names := make([]string, 64)
	for i := range names {
		names[i] = "account"
	}
	job := jobFor(len(names))
	pool := externalCPAPool{ID: "pool-1", BaseURL: "http://127.0.0.1:1", SecretKey: "secret"}

	go server.runCPAImport(pool, names, job)

	// 模拟生产读路径：处理器一律通过 jobSnapshot 取快照再序列化。
	for i := 0; i < 2000; i++ {
		if _, err := json.Marshal(server.jobSnapshot(job)); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
}
