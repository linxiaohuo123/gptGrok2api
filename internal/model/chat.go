// [INPUT]: 仅标准库（strings）与 model.IsImageModel
// [OUTPUT]: 聊天路由：ResolveChat、ChatRoute
// [POS]: 判定一个模型走 OpenAI 池还是本地链路，含全系图像模型 chat 兼容通道。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package model

import "strings"

type ChatRoute struct {
	PoolCandidates []string
	OpenAI         bool
	Image          bool
}

func ResolveChat(id string) (ChatRoute, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return ChatRoute{}, false
	}
	// Keep the chat-completions compatibility path for image clients,
	// while the model remains hidden from the normal chat catalog.
	if IsImageModel(id) {
		return ChatRoute{OpenAI: true, Image: true, PoolCandidates: []string{"basic", "super", "heavy"}}, true
	}
	if isOpenAIChatModel(id) {
		return ChatRoute{OpenAI: true, PoolCandidates: []string{"basic", "super", "heavy"}}, true
	}
	return ChatRoute{}, false
}

func isOpenAIChatModel(id string) bool {
	switch strings.TrimSpace(id) {
	case "auto",
		"gpt-5",
		"gpt-5-1",
		"gpt-5-2",
		"gpt-5-3",
		"gpt-5-3-mini",
		"gpt-5-5",
		"gpt-5-6",
		"gpt-5-6-sol",
		"gpt-5-6-terra",
		"gpt-5-6-luna",
		"gpt-5-mini":
		return true
	default:
		return false
	}
}
