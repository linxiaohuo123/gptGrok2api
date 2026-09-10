package httpapi

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHourlyMetricsStore(t *testing.T) {
	dir := t.TempDir()
	metricsPath := filepath.Join(dir, "hourly_metrics.json")
	store := NewHourlyMetricsStore(metricsPath, "")

	now := time.Date(2026, 8, 30, 13, 45, 0, 0, time.FixedZone("CST", 8*60*60))

	callLog := func(id, status string, statusCode int, startedAt time.Time, duration int) map[string]any {
		return map[string]any{
			"id": id, "type": "call", "time": startedAt.Format(time.RFC3339), "summary": id,
			"detail": map[string]any{
				"status": status, "status_code": statusCode, "started_at": startedAt.Format(time.RFC3339),
				"endpoint": "/v1/images/generations", "model": "gpt-image-2", "duration_ms": duration,
				"monitor": map[string]any{"metrics": map[string]any{"http_ttfb_ms": 500, "total_ms": duration}},
			},
		}
	}

	store.RecordCall(callLog("s1", "success", 200, now.Add(-30*time.Minute), 2000))
	store.RecordCall(callLog("f1", "failed", 429, now.Add(-10*time.Minute), 3000))
	store.RecordCall(callLog("old", "success", 200, now.Add(-48*time.Hour), 1000))

	summary := store.Summary("24h", now)
	if intValue(summary["total"]) != 2 || intValue(summary["success"]) != 1 || intValue(summary["failed"]) != 1 {
		t.Fatalf("unexpected summary totals: %#v", summary)
	}

	trend := mapValue(summary["trend"])
	labels, ok := trend["labels"].([]string)
	if !ok || len(labels) != 24 {
		t.Fatalf("unexpected trend labels: %#v", trend["labels"])
	}

	modelSeries, ok := trend["model_requests"].(map[string][]int)
	if !ok || len(modelSeries["gpt-image-2"]) != 24 || modelSeries["gpt-image-2"][23] != 2 {
		t.Fatalf("unexpected model series: %#v", trend["model_requests"])
	}

	rateLimited := trend["rate_limited_requests"].([]int)
	if rateLimited[23] != 1 {
		t.Fatalf("rate limited request missing: %#v", rateLimited)
	}

	// 验证持久化与重新加载
	store.Flush()
	storeReloaded := NewHourlyMetricsStore(metricsPath, "")
	summaryReloaded := storeReloaded.Summary("24h", now)
	if intValue(summaryReloaded["total"]) != 2 {
		t.Fatalf("reloaded store total mismatch: %#v", summaryReloaded)
	}
}
