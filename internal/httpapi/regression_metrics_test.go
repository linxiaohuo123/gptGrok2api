package httpapi

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 回归：客户端可控的指标键必须被收敛。
//
// model / endpoint / error_code 全部直接来自请求体，长度与基数都没有天然上界。
// 不收敛的话，任何持有 API Key 的人都能用随机 model 名撑爆内存、落盘体积与
// dashboard 响应——这是唯一不需要控制台权限的远程打爆入口。
func TestMetricKeyBoundsLengthAndCardinality(t *testing.T) {
	// ① 已收录的键必须原样放行，否则同名指标会漂移到 overflow 桶。
	existing := map[string]int{"gpt-image-2": 1}
	if got := metricKey(existing, "gpt-image-2"); got != "gpt-image-2" {
		t.Fatalf("已存在的键被改写为 %q", got)
	}

	// ② 超长键被截断，且不能把多字节字符切半。
	long := strings.Repeat("模", 500)
	got := metricKey(map[string]int{}, long)
	if len(got) > maxMetricKeyLength {
		t.Fatalf("超长键未被截断：长度 %d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断产生了非法 UTF-8：%q", got)
	}

	// ③ 基数达到上限后归入 overflow，而不是静默丢弃。
	full := make(map[string]int, maxMetricKeysPerBucket)
	for i := 0; i < maxMetricKeysPerBucket; i++ {
		full[string(rune('a'+i%26))+strings.Repeat("x", i%10)+string(rune(i))] = 1
	}
	if len(full) < maxMetricKeysPerBucket {
		t.Fatalf("测试用例构造失败，基数只有 %d", len(full))
	}
	if got := metricKey(full, "brand-new-key"); got != overflowMetricKey {
		t.Fatalf("基数超限的键应归入 %q，实际 %q", overflowMetricKey, got)
	}

	// ④ 空键返回空串，调用方据此跳过写入。
	if got := metricKey(map[string]int{}, "   "); got != "" {
		t.Fatalf("空白键应返回空串，实际 %q", got)
	}
}

// callLog 构造一条可供 RecordCall 消费的调用日志。
func callLog(model string) map[string]any {
	return map[string]any{
		"id":   "call-1",
		"type": "call",
		"time": time.Now().UTC().Format(time.RFC3339),
		"detail": map[string]any{
			"endpoint":    "/v1/chat/completions",
			"model":       model,
			"status":      "success",
			"duration_ms": 120,
		},
	}
}

// 回归：随机 model 名不得让聚合 map 无界增长。
func TestRecordCallBoundsModelCardinality(t *testing.T) {
	store := NewHourlyMetricsStore(t.TempDir()+"/metrics.json", t.TempDir()+"/logs.jsonl")

	// 模拟攻击：2000 次请求，每次一个随机 model 名。
	for i := 0; i < 2000; i++ {
		store.RecordCall(callLog("attacker-model-" + strings.Repeat("z", i%50) + string(rune(i%1000))))
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.buckets) == 0 {
		t.Fatal("没有产生任何桶，测试构造失败")
	}
	for key, bucket := range store.buckets {
		if len(bucket.ByModel) > maxMetricKeysPerBucket+1 {
			t.Fatalf("桶 %d 的 ByModel 基数 %d 超过上限 %d", key, len(bucket.ByModel), maxMetricKeysPerBucket)
		}
		// 六张 model 相关 map 必须共用同一套键，否则分项统计会互相错位。
		for name, size := range map[string]int{
			"ModelRequests":    len(bucket.ModelRequests),
			"ModelTotalSums":   len(bucket.ModelTotalSums),
			"ModelTotalCounts": len(bucket.ModelTotalCounts),
			"ModelTTFBSums":    len(bucket.ModelTTFBSums),
			"ModelTTFBCounts":  len(bucket.ModelTTFBCounts),
		} {
			if size > len(bucket.ByModel) {
				t.Fatalf("%s 的基数 %d 超过了 ByModel 的 %d", name, size, len(bucket.ByModel))
			}
		}
		// 溢出桶必须存在，说明是收敛而不是静默丢弃。
		if _, ok := bucket.ByModel[overflowMetricKey]; !ok && len(bucket.ByModel) >= maxMetricKeysPerBucket {
			t.Fatal("基数已满却没有 overflow 桶——被丢弃的请求会让分项之和 ≠ 总数")
		}
	}
}

// 回归：裁剪基准必须是真实的当前时间，不能用事件时间。
//
// 此前传的是 hourStart（事件所在整点）。一条时间戳领先当前 32 天以上的记录
// 会把 cutoff 推到未来，一次抹掉全部历史桶。
func TestPruneUsesWallClockNotEventTime(t *testing.T) {
	store := NewHourlyMetricsStore(t.TempDir()+"/metrics.json", t.TempDir()+"/logs.jsonl")

	// 先写入一条正常记录，形成一个"历史"桶。
	store.RecordCall(callLog("gpt-image-2"))
	store.mu.Lock()
	historyCount := len(store.buckets)
	store.mu.Unlock()
	if historyCount == 0 {
		t.Fatal("没有形成初始桶")
	}

	// 再来一条时间戳远在未来的记录。
	future := map[string]any{
		"id":   "call-future",
		"type": "call",
		"time": time.Now().UTC().Add(400 * 24 * time.Hour).Format(time.RFC3339),
		"detail": map[string]any{
			"endpoint": "/v1/chat/completions",
			"model":    "gpt-image-2",
			"status":   "success",
		},
	}
	store.RecordCall(future)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.buckets) < historyCount {
		t.Fatalf("未来时间戳抹掉了历史桶：裁剪前 %d 个，现在只剩 %d 个", historyCount, len(store.buckets))
	}
	// 未来时间戳本身也要被拉回当前小时，不能凭空造出一个永不被裁剪的桶。
	limit := time.Now().UTC().Add(2 * time.Hour).Unix()
	for key := range store.buckets {
		if key > limit {
			t.Fatalf("出现了远在未来的桶 %d —— 它会一直留在 dashboard 上", key)
		}
	}
}
