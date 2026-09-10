package httpapi

import (
	"testing"
)

func TestSanitizeDiagnosticText(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{
			input:    "failed to connect to http://admin:secret123@1.2.3.4:8080/v1",
			expected: "failed to connect to http://***@1.2.3.4:8080/v1",
		},
		{
			input:    "upstream error with Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
			expected: "upstream error with Bearer [credential]",
		},
		{
			input:    "request failed: access_token=secret-token-xyz&other=1",
			expected: "request failed: access_token=[credential]&other=1",
		},
		{
			input:    `{"error": "invalid token", "refresh_token": "rt-12345678"}`,
			expected: `{"error": "invalid token", "refresh_token": "[credential]"}`,
		},
		{
			input:    "normal error message without sensitive info",
			expected: "normal error message without sensitive info",
		},
	}

	for _, c := range cases {
		got := SanitizeDiagnosticText(c.input)
		if got != c.expected {
			t.Errorf("SanitizeDiagnosticText(%q) = %q, want %q", c.input, got, c.expected)
		}
	}
}

func TestMaskProxyURL(t *testing.T) {
	got := MaskProxyURL("http://user:password@proxy.example.com:3128")
	want := "http://***@proxy.example.com:3128"
	if got != want {
		t.Errorf("MaskProxyURL() = %q, want %q", got, want)
	}
}
