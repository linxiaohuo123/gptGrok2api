package tasks

import "testing"

// 回归：clone 必须深拷贝 Payload/Result。
//
// 它的用途是把任务交给 handler 独占使用。只重建顶层 map 的话，
// 嵌套的 map 与 slice 仍与队列内的原件共享——handler 改一个嵌套值
// 就会改到队列里的原件，而那是下一次 Submit 的输入。
func TestTaskCloneIsDeep(t *testing.T) {
	original := &Task{
		ID: "task-1",
		Payload: map[string]any{
			"nested": map[string]any{"model": "gpt-image-2"},
			"list":   []any{map[string]any{"name": "one"}},
		},
		Result: map[string]any{"items": []any{"a", "b"}},
	}

	cloned := clone(original)

	cloned.Payload["nested"].(map[string]any)["model"] = "mutated"
	cloned.Payload["list"].([]any)[0].(map[string]any)["name"] = "mutated"
	cloned.Result["items"].([]any)[0] = "mutated"

	if original.Payload["nested"].(map[string]any)["model"] != "gpt-image-2" {
		t.Fatal("嵌套 map 被共享：改 clone 污染了队列里的原件")
	}
	if original.Payload["list"].([]any)[0].(map[string]any)["name"] != "one" {
		t.Fatal("slice 内的 map 被共享")
	}
	if original.Result["items"].([]any)[0] != "a" {
		t.Fatal("Result 的 slice 被共享")
	}
}

// nil 输入必须返回 nil：调用方用 nil 判断"没有 payload"。
func TestTaskClonePreservesNil(t *testing.T) {
	cloned := clone(&Task{ID: "task-2"})
	if cloned.Payload != nil || cloned.Result != nil {
		t.Fatalf("nil 输入应保持 nil，实际 payload=%v result=%v", cloned.Payload, cloned.Result)
	}
}
