// [INPUT]: 标准库 regexp, strings
// [OUTPUT]: 诊断脱敏器：SanitizeDiagnosticText, MaskProxyURL
// [POS]: 全局诊断与错误信息清洗器，截断代理账密、Authorization Token 与会话凭据。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"regexp"
	"strings"
)

var (
	// 匹配代理 URL 中的账密信息，例如 http://user:pass@127.0.0.1:8080
	proxyAuthRegex = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s@:]+(?::[^/\s@]*)?@`)

	// 匹配 Bearer 凭证
	bearerRegex = regexp.MustCompile(`(?i)\b(Bearer\s+)[a-zA-Z0-9._\-]{8,}`)

	// 匹配 URL 查询参数中的敏感 token
	querySecretRegex = regexp.MustCompile(`(?i)\b(access_token|refresh_token|id_token|password|secret_key|api_key)=([^&\s"']+)`)

	// 匹配 JSON 键值对中的敏感 token
	jsonSecretRegex = regexp.MustCompile(`(?i)("(?:access_token|refresh_token|id_token|password|secret_key|api_key)"\s*:\s*)"([^"]+)"`)
)

// SanitizeDiagnosticText 对报错文本、日志与监控事件执行全量敏感信息脱敏清洗。
func SanitizeDiagnosticText(value string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return ""
	}
	// 快速路径：若不含敏感关键词特征，直接跳过正则开销
	hasColonSlash := strings.Contains(text, "://") && strings.Contains(text, "@")
	hasTokenKeyword := strings.Contains(strings.ToLower(text), "token") ||
		strings.Contains(strings.ToLower(text), "bearer") ||
		strings.Contains(strings.ToLower(text), "password") ||
		strings.Contains(strings.ToLower(text), "secret") ||
		strings.Contains(strings.ToLower(text), "key")

	if !hasColonSlash && !hasTokenKeyword {
		return text
	}

	if hasColonSlash {
		text = proxyAuthRegex.ReplaceAllString(text, "${1}***@")
	}
	if hasTokenKeyword {
		text = bearerRegex.ReplaceAllString(text, "${1}[credential]")
		text = querySecretRegex.ReplaceAllString(text, "${1}=[credential]")
		text = jsonSecretRegex.ReplaceAllString(text, `${1}"[credential]"`)
	}
	return text
}

// MaskProxyURL 针对独立代理地址做脱敏。
func MaskProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "@") {
		return proxyAuthRegex.ReplaceAllString(raw, "${1}***@")
	}
	return raw
}
