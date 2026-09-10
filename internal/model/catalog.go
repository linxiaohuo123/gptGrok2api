// [INPUT]: 仅标准库（strings, time）
// [OUTPUT]: 模型目录：Catalog、Find、Spec、IsImageModel
// [POS]: 对外模型清单的唯一来源，含 gpt-image-2.5 与 Codex 图像模型家族。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package model

import (
	"strings"
	"time"
)

type Capability uint8

const (
	Chat Capability = 1 << iota
	Image
	ImageEdit
)

type Spec struct {
	ID         string
	Name       string
	OwnedBy    string
	Created    int64
	Capability Capability
	Enabled    bool
}

func (s Spec) Public() map[string]any {
	return map[string]any{
		"id":       s.ID,
		"object":   "model",
		"created":  s.Created,
		"owned_by": s.OwnedBy,
		"name":     s.Name,
	}
}

func Catalog() []Spec {
	created := time.Now().Unix()
	items := []Spec{
		{"auto", "Auto", "openai", created, Chat, true},
		{"gpt-5", "GPT-5", "openai", created, Chat, true},
		{"gpt-5-1", "GPT-5.1", "openai", created, Chat, true},
		{"gpt-5-2", "GPT-5.2", "openai", created, Chat, true},
		{"gpt-5-3", "GPT-5.3", "openai", created, Chat, true},
		{"gpt-5-3-mini", "GPT-5.3 Mini", "openai", created, Chat, true},
		{"gpt-5-5", "GPT-5.5", "openai", created, Chat, true},
		{"gpt-5-6", "GPT-5.6", "openai", created, Chat, true},
		{"gpt-5-6-sol", "GPT-5.6 Sol", "openai", created, Chat, true},
		{"gpt-5-6-terra", "GPT-5.6 Terra", "openai", created, Chat, true},
		{"gpt-5-6-luna", "GPT-5.6 Luna", "openai", created, Chat, true},
		{"gpt-5-mini", "GPT-5 Mini", "openai", created, Chat, true},
		{"gpt-image-2", "GPT Image 2", "openai-compatible", created, Image, true},
		{"gpt-image-2.5", "GPT Image 2.5", "openai-compatible", created, Image, true},
		{"gpt-image-2.5-flare", "GPT Image 2.5 Flare", "openai-compatible", created, Image, true},
		{"gpt-image-2.5-sunburst", "GPT Image 2.5 Sunburst", "openai-compatible", created, Image, true},
		{"codex-gpt-image-2", "Codex GPT Image 2", "openai-compatible", created, Image, true},
		{"plus-codex-gpt-image-2", "Plus Codex GPT Image 2", "openai-compatible", created, Image, true},
		{"team-codex-gpt-image-2", "Team Codex GPT Image 2", "openai-compatible", created, Image, true},
		{"pro-codex-gpt-image-2", "Pro Codex GPT Image 2", "openai-compatible", created, Image, true},
	}
	return items
}

func Find(items []Spec, id string) (Spec, bool) {
	for _, item := range items {
		if item.Enabled && item.ID == id {
			return item, true
		}
	}
	return Spec{}, false
}

func IsImageModel(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "gpt-image-2",
		"gpt-image-2.5",
		"gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst",
		"codex-gpt-image-2",
		"plus-codex-gpt-image-2",
		"team-codex-gpt-image-2",
		"pro-codex-gpt-image-2":
		return true
	default:
		return false
	}
}
