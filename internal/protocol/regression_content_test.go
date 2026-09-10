package protocol

import "testing"

// 回归：Responses API 标准内容块 input_text / output_text 被当作非文本丢弃。
func TestProofResponsesInputTextDropped(t *testing.T) {
	cases := []struct {
		name  string
		typ   string
		input any
	}{
		{"input_text 数组形式", "input_text", []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "你好"}}}}},
		{"output_text 数组形式", "output_text", []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "上一轮回复"}}}}},
		{"input_text 单对象形式", "input_text", map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "你好"}}}},
		{"对照组 text 形式", "text", []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "你好"}}}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			messages := ResponsesInputMessages(testCase.input, "")
			if len(messages) == 0 {
				t.Fatalf("type=%q 的内容块被丢弃，消息数为 0", testCase.typ)
			}
			if text := ExtractMessage(messages); text == "" {
				t.Fatalf("type=%q 的消息文本为空", testCase.typ)
			}
		})
	}
}
