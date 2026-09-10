package httpapi

import (
	"encoding/json"
	"sync"
	"testing"
)

// 回归：monitor 快照与在途记录共享 map —— snapshot() 出锁后序列化，enrich() 持锁写同一个 map。
func TestProofMonitorSnapshotRace(t *testing.T) {
	monitor := newRuntimeMonitor()
	monitor.start("call-1", "/v1/images/generations", "gpt-image-2", "proof")

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				if raw, err := json.Marshal(monitor.snapshot()); err != nil {
					t.Errorf("marshal: %v", err)
					return
				} else if len(raw) == 0 {
					t.Error("empty snapshot")
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				monitor.enrich("call-1", map[string]any{"metrics": map[string]any{"stage_ms": j}, "perf": map[string]any{"total_ms": j}, "extra_ms": j})
				monitor.update("call-1", "image_starting_generation", j%100, "")
			}
		}(i)
	}
	wg.Wait()
}
