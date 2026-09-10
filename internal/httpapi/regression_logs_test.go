package httpapi

import (
	"net/url"
	"testing"
)

// 与 appendCallLog 写出的行同构。
func sampleLogEntry() map[string]any {
	return map[string]any{
		"id":      "chatcmpl-1",
		"time":    "2026-09-10T02:36:57Z",
		"type":    "call",
		"summary": "一只猫",
		"detail": map[string]any{
			"call_id":       "chatcmpl-1",
			"endpoint":      "/v1/images/generations",
			"model":         "gpt-image-2",
			"status":        "failed",
			"account_email": "user@example.com",
			"started_at":    "2026-09-10T02:36:57Z",
			"ended_at":      "2026-09-10T02:37:11Z",
			"error":         "upstream 500",
		},
	}
}

func TestLogMatchesReadsNestedDetail(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"按状态筛选失败", "status=failed", true},
		{"按状态筛选成功", "status=succeeded", false},
		{"按端点筛选", "endpoint=/v1/images/generations", true},
		{"按模型筛选", "model=gpt-image-2", true},
		{"按账号筛选", "account=user@example.com", true},
		{"按账号筛选不匹配", "account=other@example.com", false},
		{"按类型筛选", "type=call", true},
		{"会话 id 回退到 call_id", "conversation_id=chatcmpl-1", true},
		{"日期区间命中当天", "start_date=2026-09-01&end_date=2026-09-10", true},
		{"日期区间早于记录", "start_date=2026-09-11", false},
		{"日期区间晚于记录", "end_date=2026-09-09", false},
		{"关键词搜索", "search=upstream", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values, err := url.ParseQuery(testCase.query)
			if err != nil {
				t.Fatal(err)
			}
			if got := logMatches(sampleLogEntry(), values); got != testCase.want {
				t.Fatalf("query=%q 得到 %v，期望 %v", testCase.query, got, testCase.want)
			}
		})
	}
}
