// [INPUT]: 仅标准库
// [OUTPUT]: CloneMap——JSON 形状数据的深拷贝，store 与 httpapi 共用的唯一实现
// [POS]: 数据复制的单一真相源。此前 store/json.go 与 httpapi/server.go 各有一份
//         逐字相同的浅拷贝，修一份必漏另一份，故合并至此。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package store

// CloneMap 深拷贝一份 JSON 形状的数据。
//
// 浅拷贝只复制顶层，嵌套的 map/slice 仍与来源共享内存。当来源是一份活缓存
// （例如 Store.configCache）时，调用方在锁外写嵌套字段，就会与并发读者撞成
// Go 运行时不可 recover 的 fatal error。
//
// nil 输入返回 nil，而非空 map——调用方用 nil 判断"没有这一项"的语义必须保住。
func CloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = cloneValue(value)
	}
	return output
}

// cloneValue 递归复制容器类型；标量不可变，直接返回。
//
// 除 JSON 反序列化天然产出的 map[string]any / []any 外，还要覆盖代码写入配置
// 后残留的强类型容器（如 proxy_groups 的 []map[string]any），否则它们会重新
// 变成共享引用——深拷贝只做一半等于没做。
func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return CloneMap(typed)
	case []any:
		items := make([]any, len(typed))
		for index, item := range typed {
			items[index] = cloneValue(item)
		}
		return items
	case []map[string]any:
		items := make([]map[string]any, len(typed))
		for index, item := range typed {
			items[index] = CloneMap(item)
		}
		return items
	case []string:
		return append([]string(nil), typed...)
	case map[string]string:
		output := make(map[string]string, len(typed))
		for key, item := range typed {
			output[key] = item
		}
		return output
	default:
		return value
	}
}
