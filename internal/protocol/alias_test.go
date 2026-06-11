package protocol_test

import (
	"testing"

	"github.com/claude-code-launch/ccl/internal/protocol"
)

func TestModelAliases(t *testing.T) {
	testCases := []struct {
		realModel string
		expected  string
	}{
		{"deepseek-v4-pro", "claude-ds-v4-pro"},
		{"gemini-3.5-flash", "claude-gm-3.5-flash"},
		{"qwen3.6-plus", "claude-qw3.6-plus"},
		{"bailian-glm-5.1", "claude-bailian-g-5.1"},
		{"gpt-4o", "claude-gp-4o"},
		{"claude-3-5-sonnet", "claude-3-5-sonnet"}, // should keep original if valid and safe
	}

	available := []string{
		"deepseek-v4-pro",
		"gemini-3.5-flash",
		"qwen3.6-plus",
		"bailian-glm-5.1",
		"gpt-4o",
		"claude-3-5-sonnet",
	}

	for _, tc := range testCases {
		alias := protocol.ToGatewayModelAlias(tc.realModel)
		if alias != tc.expected {
			t.Errorf("ToGatewayModelAlias(%q) = %q, expected %q", tc.realModel, alias, tc.expected)
		}

		restored := protocol.FromGatewayModelAlias(alias, available)
		if restored != tc.realModel {
			t.Errorf("FromGatewayModelAlias(%q) = %q, expected %q", alias, restored, tc.realModel)
		}
	}
}
