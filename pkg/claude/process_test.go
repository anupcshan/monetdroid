package claude_test

import (
	"testing"

	"github.com/anupcshan/monetdroid/pkg/claude"
	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// Compile-time check that StartProcessWithConfig is callable with the expected signature.
var _ func(string, func(protocol.StreamEvent), string, *claude.ProcessConfig) (*claude.ClaudeProcess, error) = claude.StartProcessWithConfig

// TestPermissionHandlerAPI verifies that PermissionRequest, PermissionHandler,
// PermResponse, and ProcessConfig compose correctly from an external package —
// ensuring the library's public API surface stays usable without importing
// internal wire protocol.
func TestPermissionHandlerAPI(t *testing.T) {
	handler := func(req protocol.PermissionRequest) protocol.PermResponse {
		if req.ToolName == "mcp__assistant__send_message" {
			return protocol.PermResponse{Allow: true}
		}
		return protocol.PermResponse{Allow: false}
	}

	cfg := &claude.ProcessConfig{
		PermissionHandler:  handler,
		AppendSystemPrompt: "You are a helpful assistant.",
		AllowedTools:       "mcp__assistant__*",
		MaxTurns:           10,
	}

	resp := cfg.PermissionHandler(protocol.PermissionRequest{
		ToolName: "mcp__assistant__send_message",
	})
	if !resp.Allow {
		t.Fatal("expected allow for MCP tool")
	}

	resp = cfg.PermissionHandler(protocol.PermissionRequest{
		ToolName: "Bash",
	})
	if resp.Allow {
		t.Fatal("expected deny for non-MCP tool")
	}
}

func TestCommandAndExtraArgs(t *testing.T) {
	cfg := &claude.ProcessConfig{
		Command:   []string{"podman", "run", "-i", "--rm", "container", "claude"},
		ExtraArgs: []string{"--mcp-config", `{"assistant":{"type":"http"}}`, "--strict-mcp-config"},
	}
	if cfg.Command[0] != "podman" {
		t.Fatal("expected podman as base command")
	}
	if cfg.ExtraArgs[0] != "--mcp-config" {
		t.Fatal("expected --mcp-config in extra args")
	}
}
