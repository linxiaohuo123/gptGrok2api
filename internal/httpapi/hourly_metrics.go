// [INPUT]: 标准库 time, sync, encoding/json, os, path/filepath, strings
// [OUTPUT]: 增量小时聚合器：HourlyMetricsStore, NewHourlyMetricsStore, HourlyBucket
// [POS]: 仪表盘监控投影的核心聚合器，以写时代价替换读时扫表，提供 O(1) 的趋势查询。
//         核心不变量：**所有聚合键必须过 metricKey**。model / endpoint / error_code
//         全部直接来自请求体，长度与基数都没有天然上界——不收敛的话，任何持有
//         API Key 的人都能用随机 model 名撑爆内存、落盘体积与 dashboard 响应。
//         裁剪基准必须是真实当前时间，不能用事件时间。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type HourlyBucket struct {
	StartTime        time.Time          `json:"start_time"`
	Total            int                `json:"total"`
	Success          int                `json:"success"`
	Failed           int                `json:"failed"`
	RateLimited      int                `json:"rate_limited"`
	SwitchCount      int                `json:"switch_count"`
	SwitchRecovered  int                `json:"switch_recovered"`
	ByEndpoint       map[string]int     `json:"by_endpoint"`
	ByModel          map[string]int     `json:"by_model"`
	ByStatus         map[string]int     `json:"by_status"`
	ByErrorCode      map[string]int     `json:"by_error_code"`
	ModelRequests    map[string]int     `json:"model_requests"`
	ModelTotalSums   map[string]float64 `json:"model_total_sums"`
	ModelTotalCounts map[string]int     `json:"model_total_counts"`
	ModelTTFBSums    map[string]float64 `json:"model_ttfb_sums"`
	ModelTTFBCounts  map[string]int     `json:"model_ttfb_counts"`
}

func newHourlyBucket(start time.Time) *HourlyBucket {
	return &HourlyBucket{
		StartTime:        start,
		ByEndpoint:       make(map[string]int),
		ByModel:          make(map[string]int),
		ByStatus:         make(map[string]int),
		ByErrorCode:      make(map[string]int),
		ModelRequests:    make(map[string]int),
		ModelTotalSums:   make(map[string]float64),
		ModelTotalCounts: make(map[string]int),
		ModelTTFBSums:    make(map[string]float64),
		ModelTTFBCounts:  make(map[string]int),
	}
}

type HourlyMetricsStore struct {
	mu             sync.RWMutex
	filePath       string
	buckets        map[int64]*HourlyBucket // key: UTC Unix timestamp of hour start
	recentFailures []map[string]any
	dirty          bool
}

func NewHourlyMetricsStore(filePath string, logsPath string) *HourlyMetricsStore {
	store := &HourlyMetricsStore{
		filePath:       filePath,
		buckets:        make(map[int64]*HourlyBucket),
		recentFailures: make([]map[string]any, 0, 50),
	}
	if filePath != "" {
		if store.loadPersisted() {
			return store
		}
	}
	// 如果没有持久化的小时聚合文件，但存在历史 logs.jsonl，则做一次冷启动回填
	if logsPath != "" {
		store.bootstrapFromLogs(logsPath)
		store.persistLocked()
	}
	return store
}

func (s *HourlyMetricsStore) loadPersisted() bool {
	raw, err := os.ReadFile(s.filePath)
	if err != nil || len(raw) == 0 {
		return false
	}
	var data struct {
		Buckets        map[int64]*HourlyBucket `json:"buckets"`
		RecentFailures []map[string]any        `json:"recent_failures"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if data.Buckets != nil {
		s.buckets = data.Buckets
	}
	if data.RecentFailures != nil {
		s.recentFailures = data.RecentFailures
	}
	return len(s.buckets) > 0
}

func (s *HourlyMetricsStore) bootstrapFromLogs(logsPath string) {
	file, err := os.Open(logsPath)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal(scanner.Bytes(), &item) == nil && item != nil {
			s.RecordCall(item)
		}
	}
}

// RecordCall 在写日志时增量折叠当前调用记录，微秒级完成
const (
	// maxMetricKeyLength 是进入聚合 map 的键长度上限。
	maxMetricKeyLength = 96
	// maxMetricKeysPerBucket 是单个小时内每个维度的键基数上限。
	maxMetricKeysPerBucket = 200
	// overflowMetricKey 收纳超出基数上限的键。刻意不丢弃——静默丢弃会让
	// "分项之和 ≠ 总数"，那是比内存增长更难查的坑。
	overflowMetricKey = "__other__"
)

// metricKey 把一个客户端可控的字符串收敛成安全的聚合键。
//
// model / endpoint / error_code 三者都直接来自请求体，长度与基数都没有天然上界。
// 不收敛的话，任何持有 API Key 的人都能用随机 model 名把内存、落盘体积与
// dashboard 响应一起撑爆——这是唯一一个不需要控制台权限的远程打爆入口
// （实测：3 次带随机 model 的请求即写入 3 个新键）。
//
// 已收录的键总是原样放行，保证同一小时内同名指标不会漂移到 overflow 桶。
func metricKey(existing map[string]int, raw string) string {
	key := strings.TrimSpace(raw)
	if key == "" {
		return ""
	}
	if len(key) > maxMetricKeyLength {
		// 直接切片会切断多字节字符，落到 JSON 里就是一片 U+FFFD。
		key = strings.ToValidUTF8(key[:maxMetricKeyLength], "")
	}
	if _, ok := existing[key]; ok {
		return key
	}
	if len(existing) >= maxMetricKeysPerBucket {
		return overflowMetricKey
	}
	return key
}

func (s *HourlyMetricsStore) RecordCall(item map[string]any) {
	if !strings.EqualFold(stringValue(item["type"]), "call") {
		return
	}
	detail := mapValue(item["detail"])
	location := time.UTC
	startedAt, ok := dashboardLogTime(firstNonEmpty(stringValue(detail["started_at"]), stringValue(item["time"])), location)
	if !ok {
		startedAt = time.Now().UTC()
	}
	// 时钟向前校正后回拨、VM 快照恢复、或日志文件被外部写入，都可能带来
	// 未来时间戳。那样的桶既不会被裁掉，又会把 dashboard 的时间轴拉长。
	// 超过一小时就按当前时间处理。
	if now := time.Now(); startedAt.After(now.Add(time.Hour)) {
		startedAt = now.UTC()
	}
	hourStart := startedAt.UTC().Truncate(time.Hour)
	hourKey := hourStart.Unix()

	status := strings.ToLower(strings.TrimSpace(firstNonEmpty(stringValue(detail["status"]), stringValue(item["status"]))))
	endpoint := strings.TrimSpace(firstNonEmpty(stringValue(detail["endpoint"]), stringValue(item["endpoint"])))
	modelName := strings.TrimSpace(firstNonEmpty(stringValue(detail["model"]), stringValue(item["model"])))
	errorText := strings.TrimSpace(firstNonEmpty(stringValue(detail["error"]), stringValue(detail["raw_error"])))
	errorCode := strings.TrimSpace(firstNonEmpty(stringValue(detail["error_code"]), stringValue(item["error_code"])))
	statusCode := intValue(detail["status_code"])
	isFailed := status == "failed" || status == "error" || status == "fail" || errorText != "" || statusCode >= 400
	isRateLimited := statusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(errorCode), "rate_limit") || errorCode == "429"
	if errorCode == "" && statusCode >= 400 {
		errorCode = fmt.Sprintf("%d", statusCode)
	}

	duration := dashboardLogMetric(detail, "duration_ms", "total_ms")
	ttfb := dashboardLogMetric(detail, "http_ttfb_ms", "sse_first_event_ms", "stream_first_queue_ms")

	s.mu.Lock()
	defer s.mu.Unlock()

	bucket, exists := s.buckets[hourKey]
	if !exists {
		bucket = newHourlyBucket(hourStart)
		s.buckets[hourKey] = bucket
	}

	bucket.Total++
	if isFailed {
		bucket.Failed++
		if isRateLimited {
			bucket.RateLimited++
		}
		// 环形缓冲记录最近失败
		failureItem := map[string]any{
			"id":              item["id"],
			"time":            firstNonEmpty(stringValue(item["time"]), stringValue(detail["started_at"])),
			"summary":         item["summary"],
			"endpoint":        endpoint,
			"error_code":      errorCode,
			"stage":           detail["stage"],
			"reason":          firstNonEmpty(errorText, stringValue(detail["reason"])),
			"conversation_id": detail["conversation_id"],
		}
		if len(s.recentFailures) >= 50 {
			s.recentFailures = append(s.recentFailures[1:], failureItem)
		} else {
			s.recentFailures = append(s.recentFailures, failureItem)
		}
	} else {
		bucket.Success++
	}

	// status 由后端自己判定、取值有限，仍走同一个收敛函数保持一致。
	if key := metricKey(bucket.ByStatus, status); key != "" {
		bucket.ByStatus[key]++
	}
	if strings.HasPrefix(endpoint, "/") {
		if key := metricKey(bucket.ByEndpoint, endpoint); key != "" {
			bucket.ByEndpoint[key]++
		}
	}
	// 张数相关的六张 map 共用同一个键：字典树、请求数、总耗时分母与分子、
	// TTFB 分母与分子必须落在同一个 modelKey 上，否则分项统计会互相错位。
	if modelKey := metricKey(bucket.ByModel, modelName); modelKey != "" {
		bucket.ByModel[modelKey]++
		bucket.ModelRequests[modelKey]++
		if duration > 0 {
			bucket.ModelTotalSums[modelKey] += duration
			bucket.ModelTotalCounts[modelKey]++
		}
		if ttfb > 0 {
			bucket.ModelTTFBSums[modelKey] += ttfb
			bucket.ModelTTFBCounts[modelKey]++
		}
	}
	if key := metricKey(bucket.ByErrorCode, errorCode); key != "" {
		bucket.ByErrorCode[key]++
	}

	switchCount := intValue(detail["switch_count"])
	if switchCount == 0 {
		switchCount = intValue(detail["account_switch_count"])
	}
	if switchCount > 0 {
		bucket.SwitchCount += switchCount
		if !isFailed {
			bucket.SwitchRecovered++
		}
	}

	// 触发轻量持久化
	s.dirty = true
	// 裁剪基准必须是**真实的当前时间**。此前传的是 hourStart —— 那是事件时间，
	// 一条时间戳领先当前 32 天以上的记录会把 cutoff 推到未来，一次抹掉全部历史桶。
	s.pruneOldBucketsLocked(time.Now().UTC())
}

func (s *HourlyMetricsStore) pruneOldBucketsLocked(now time.Time) {
	cutoff := now.Add(-32 * 24 * time.Hour).Unix()
	for k := range s.buckets {
		if k < cutoff {
			delete(s.buckets, k)
		}
	}
}

func (s *HourlyMetricsStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistLocked()
}

func (s *HourlyMetricsStore) persistLocked() {
	if s.filePath == "" || !s.dirty {
		return
	}
	data := map[string]any{
		"buckets":         s.buckets,
		"recent_failures": s.recentFailures,
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.filePath), 0o755)
	_ = os.WriteFile(s.filePath, raw, 0o600)
	s.dirty = false
}

// Summary 直接提取聚合结果，常数级 O(1) 耗时 (< 50 微秒)
func (s *HourlyMetricsStore) Summary(timeRange string, now time.Time) map[string]any {
	timeRange = strings.ToLower(strings.TrimSpace(timeRange))
	if timeRange != "7d" && timeRange != "30d" {
		timeRange = "24h"
	}
	location := now.Location()
	now = now.In(location)

	bucketCount := 24
	bucketStep := time.Hour
	currentStart := now.Truncate(time.Hour)
	labelFormat := "15:00"

	if timeRange != "24h" {
		bucketCount = 7
		if timeRange == "30d" {
			bucketCount = 30
		}
		bucketStep = 24 * time.Hour
		currentStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
		labelFormat = "01-02"
	}

	firstStart := currentStart.Add(-time.Duration(bucketCount-1) * bucketStep)
	labels := make([]string, bucketCount)
	totalRequests := make([]int, bucketCount)
	successRequests := make([]int, bucketCount)
	failedRequests := make([]int, bucketCount)
	rateLimitedRequests := make([]int, bucketCount)

	modelRequests := make(map[string][]int)
	modelTotalSums := make(map[string][]float64)
	modelTotalCounts := make(map[string][]int)
	modelTTFBSums := make(map[string][]float64)
	modelTTFBCounts := make(map[string][]int)

	for index := range labels {
		labels[index] = firstStart.Add(time.Duration(index) * bucketStep).Format(labelFormat)
	}

	byEndpoint := make(map[string]any)
	byModel := make(map[string]any)
	byStatus := make(map[string]any)
	byErrorCode := make(map[string]any)
	total, success, failed := 0, 0, 0

	s.mu.RLock()
	defer s.mu.RUnlock()

	// 遍历时间段内的所有时间槽
	for i := 0; i < bucketCount; i++ {
		slotStart := firstStart.Add(time.Duration(i) * bucketStep)
		slotEnd := slotStart.Add(bucketStep)

		// 累加该 slot 包含的所有小时桶
		for h := slotStart; h.Before(slotEnd); h = h.Add(time.Hour) {
			b, ok := s.buckets[h.UTC().Truncate(time.Hour).Unix()]
			if !ok {
				continue
			}
			total += b.Total
			success += b.Success
			failed += b.Failed

			totalRequests[i] += b.Total
			successRequests[i] += b.Success
			failedRequests[i] += b.Failed
			rateLimitedRequests[i] += b.RateLimited

			for ep, count := range b.ByEndpoint {
				byEndpoint[ep] = intValue(byEndpoint[ep]) + count
			}
			for m, count := range b.ByModel {
				byModel[m] = intValue(byModel[m]) + count
			}
			for st, count := range b.ByStatus {
				byStatus[st] = intValue(byStatus[st]) + count
			}
			for ec, count := range b.ByErrorCode {
				byErrorCode[ec] = intValue(byErrorCode[ec]) + count
			}

			for model, count := range b.ModelRequests {
				ensureDashboardIntSeries(modelRequests, model, bucketCount)[i] += count
			}
			for model, sum := range b.ModelTotalSums {
				ensureDashboardFloatSeries(modelTotalSums, model, bucketCount)[i] += sum
			}
			for model, count := range b.ModelTotalCounts {
				ensureDashboardIntSeries(modelTotalCounts, model, bucketCount)[i] += count
			}
			for model, sum := range b.ModelTTFBSums {
				ensureDashboardFloatSeries(modelTTFBSums, model, bucketCount)[i] += sum
			}
			for model, count := range b.ModelTTFBCounts {
				ensureDashboardIntSeries(modelTTFBCounts, model, bucketCount)[i] += count
			}
		}
	}

	recentFailures := make([]map[string]any, 0, 10)
	if len(s.recentFailures) > 0 {
		start := 0
		if len(s.recentFailures) > 10 {
			start = len(s.recentFailures) - 10
		}
		recentFailures = append(recentFailures, s.recentFailures[start:]...)
	}

	return map[string]any{
		"total":           total,
		"success":         success,
		"failed":          failed,
		"by_endpoint":     byEndpoint,
		"by_model":        byModel,
		"by_status":       byStatus,
		"by_error_code":   byErrorCode,
		"recent_failures": recentFailures,
		"trend": map[string]any{
			"labels":                labels,
			"total_requests":        totalRequests,
			"success_requests":      successRequests,
			"failed_requests":       failedRequests,
			"rate_limited_requests": rateLimitedRequests,
			"model_requests":        modelRequests,
			"model_total_times":     dashboardAverageSeries(modelTotalSums, modelTotalCounts),
			"model_ttfb_times":      dashboardAverageSeries(modelTTFBSums, modelTTFBCounts),
		},
	}
}

// BuildRangesSchemaV5 生成符合前端 Schema v5 规范的 24h, 7d, 30d 完整统计
func (s *HourlyMetricsStore) BuildRangesSchemaV5(now time.Time) map[string]any {
	location := now.Location()
	now = now.In(location)

	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]any{
		"24h": s.buildSingleRangeSchemaV5("24h", now, location),
		"7d":  s.buildSingleRangeSchemaV5("7d", now, location),
		"30d": s.buildSingleRangeSchemaV5("30d", now, location),
	}
}

func (s *HourlyMetricsStore) buildSingleRangeSchemaV5(timeRange string, now time.Time, location *time.Location) map[string]any {
	bucketCount := 24
	bucketStep := time.Hour
	bucketUnit := "hour"
	currentStart := now.Truncate(time.Hour)
	labelFormat := "15:00"

	if timeRange == "7d" {
		bucketCount = 7
		bucketStep = 24 * time.Hour
		bucketUnit = "day"
		currentStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
		labelFormat = "01-02"
	} else if timeRange == "30d" {
		bucketCount = 30
		bucketStep = 24 * time.Hour
		bucketUnit = "day"
		currentStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
		labelFormat = "01-02"
	}

	firstStart := currentStart.Add(-time.Duration(bucketCount-1) * bucketStep)
	rangeEnd := now

	labels := make([]string, bucketCount)
	successRequests := make([]int, bucketCount)
	finalFailedRequests := make([]int, bucketCount)
	switchCounts := make([]int, bucketCount)
	successRates := make([]any, bucketCount) // *float64 or nil

	modelSuccessRequests := make(map[string][]int)
	modelDurationSums := make(map[string][]float64)
	modelDurationCounts := make(map[string][]int)

	buckets := make([]map[string]any, bucketCount)

	totalCalls := 0
	totalSuccess := 0
	totalFinalFailed := 0
	totalDurationSum := 0.0
	totalDurationCount := 0

	totalSwitchRequests := 0
	totalSwitchCount := 0
	totalSwitchRecovered := 0

	for i := 0; i < bucketCount; i++ {
		slotStart := firstStart.Add(time.Duration(i) * bucketStep)
		slotEnd := slotStart.Add(bucketStep)
		label := slotStart.Format(labelFormat)
		labels[i] = label

		slotTotal := 0
		slotSuccess := 0
		slotFailed := 0
		slotSwitch := 0
		slotRecovered := 0
		slotDurSum := 0.0
		slotDurCount := 0

		// 聚合时间槽包含的小时桶
		for h := slotStart; h.Before(slotEnd); h = h.Add(time.Hour) {
			b, ok := s.buckets[h.UTC().Truncate(time.Hour).Unix()]
			if !ok {
				continue
			}
			slotTotal += b.Total
			slotSuccess += b.Success
			slotFailed += b.Failed
			slotSwitch += b.SwitchCount
			slotRecovered += b.SwitchRecovered

			for m, count := range b.ModelRequests {
				ensureDashboardIntSeries(modelSuccessRequests, m, bucketCount)[i] += count
			}
			for m, sum := range b.ModelTotalSums {
				ensureDashboardFloatSeries(modelDurationSums, m, bucketCount)[i] += sum
			}
			for m, count := range b.ModelTotalCounts {
				ensureDashboardIntSeries(modelDurationCounts, m, bucketCount)[i] += count
				slotDurCount += count
			}
			for _, sum := range b.ModelTotalSums {
				slotDurSum += sum
			}
		}

		totalCalls += slotTotal
		totalSuccess += slotSuccess
		totalFinalFailed += slotFailed
		totalDurationSum += slotDurSum
		totalDurationCount += slotDurCount

		if slotSwitch > 0 {
			totalSwitchRequests++
			totalSwitchCount += slotSwitch
			totalSwitchRecovered += slotRecovered
		}

		successRequests[i] = slotSuccess
		finalFailedRequests[i] = slotFailed
		switchCounts[i] = slotSwitch

		var slotSuccessRate any = nil
		if slotTotal > 0 {
			rate := float64(slotSuccess) / float64(slotTotal) * 100.0
			slotSuccessRate = rate
		}
		successRates[i] = slotSuccessRate

		var slotAvgDur any = nil
		if slotDurCount > 0 {
			avg := slotDurSum / float64(slotDurCount)
			slotAvgDur = avg
		}

		var slotSwitchRate any = nil
		if slotSwitch > 0 {
			rate := float64(slotRecovered) / float64(slotSwitch) * 100.0
			slotSwitchRate = rate
		}

		buckets[i] = map[string]any{
			"label":                   label,
			"start_at":                slotStart.UTC().Format(time.RFC3339),
			"end_at":                  slotEnd.UTC().Format(time.RFC3339),
			"total_calls":             slotTotal,
			"success_calls":           slotSuccess,
			"final_failed_calls":      slotFailed,
			"switch_count":            slotSwitch,
			"switch_recovered":        slotRecovered,
			"success_rate":            slotSuccessRate,
			"avg_success_duration_ms": slotAvgDur,
			"switch_recovery_rate":    slotSwitchRate,
		}
	}

	modelAvgDuration := make(map[string][]any)
	for m, sums := range modelDurationSums {
		counts := modelDurationCounts[m]
		series := make([]any, bucketCount)
		for idx := range series {
			if counts != nil && counts[idx] > 0 {
				series[idx] = sums[idx] / float64(counts[idx])
			} else {
				series[idx] = nil
			}
		}
		modelAvgDuration[m] = series
	}

	var totalSuccessRate any = nil
	if totalCalls > 0 {
		totalSuccessRate = float64(totalSuccess) / float64(totalCalls) * 100.0
	}
	var totalAvgDur any = nil
	if totalDurationCount > 0 {
		totalAvgDur = totalDurationSum / float64(totalDurationCount)
	}

	var switchRecoveryRate any = nil
	if totalSwitchCount > 0 {
		switchRecoveryRate = float64(totalSwitchRecovered) / float64(totalSwitchCount) * 100.0
	}

	return map[string]any{
		"time_range": timeRange,
		"window": map[string]any{
			"requested":    timeRange,
			"start_at":     firstStart.UTC().Format(time.RFC3339),
			"end_at":       rangeEnd.UTC().Format(time.RFC3339),
			"bucket_unit":  bucketUnit,
			"bucket_count": bucketCount,
		},
		"totals": map[string]any{
			"total":                   totalCalls,
			"success":                 totalSuccess,
			"final_failed":            totalFinalFailed,
			"success_rate":            totalSuccessRate,
			"avg_success_duration_ms": totalAvgDur,
		},
		"switching": map[string]any{
			"requests":      totalSwitchRequests,
			"count":         totalSwitchCount,
			"recovered":     totalSwitchRecovered,
			"recovery_rate": switchRecoveryRate,
		},
		"trend": map[string]any{
			"labels":                        labels,
			"success_requests":              successRequests,
			"final_failed_requests":         finalFailedRequests,
			"switch_count":                  switchCounts,
			"success_rate":                  successRates,
			"model_success_requests":        modelSuccessRequests,
			"model_avg_success_duration_ms": modelAvgDuration,
		},
		"buckets": buckets,
	}
}
