package store

import "testing"

// 回归：CloneMap 必须是深拷贝。
//
// 浅拷贝只复制顶层，嵌套 map/slice 仍与来源共享内存。来源若是活缓存
// （configCache / accountsCache），调用方在锁外写嵌套字段就会与并发读者
// 撞成 Go 运行时不可 recover 的 fatal error。
func TestCloneMapIsDeep(t *testing.T) {
	shared := map[string]any{
		"proxy_runtime": map[string]any{
			"clearance": map[string]any{"enabled": false},
			"layers":    []any{map[string]any{"name": "inner"}},
		},
		"proxy_groups": []map[string]any{
			{"id": "group-one", "nodes": []any{map[string]any{"id": "node-one"}}},
		},
		"names": []string{"a"},
	}

	cloned := CloneMap(shared)

	nested := cloned["proxy_runtime"].(map[string]any)
	nested["injected"] = true
	clearance := nested["clearance"].(map[string]any)
	clearance["enabled"] = true
	nested["layers"].([]any)[0].(map[string]any)["name"] = "mutated"

	groups := cloned["proxy_groups"].([]map[string]any)
	groups[0]["id"] = "mutated-group"
	groups[0]["nodes"].([]any)[0].(map[string]any)["id"] = "mutated-node"

	cloned["names"].([]string)[0] = "z"

	sourceRuntime := shared["proxy_runtime"].(map[string]any)
	if _, leaked := sourceRuntime["injected"]; leaked {
		t.Fatal("嵌套 map 被共享：写入 clone 污染了来源")
	}
	if sourceRuntime["clearance"].(map[string]any)["enabled"] != false {
		t.Fatal("二层嵌套 map 被共享")
	}
	if sourceRuntime["layers"].([]any)[0].(map[string]any)["name"] != "inner" {
		t.Fatal("slice 内的 map 被共享")
	}
	if shared["proxy_groups"].([]map[string]any)[0]["id"] != "group-one" {
		t.Fatal("[]map[string]any 的元素被共享")
	}
	if shared["proxy_groups"].([]map[string]any)[0]["nodes"].([]any)[0].(map[string]any)["id"] != "node-one" {
		t.Fatal("[]map[string]any 内的深层的 map 被共享")
	}
	if shared["names"].([]string)[0] != "a" {
		t.Fatal("[]string 被共享")
	}
}

// nil 输入必须返回 nil：调用方用 nil 判断"没有这一项"。
func TestCloneMapPreservesNil(t *testing.T) {
	if CloneMap(nil) != nil {
		t.Fatal("nil 输入应返回 nil，而不是空 map")
	}
}
