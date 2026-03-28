package monetdroid_test

import (
	"testing"

	"github.com/anupcshan/monetdroid/pkg/monetdroid"
)

// Compile-time check that StartProcessWithConfig is callable with the expected signature.
var _ func(*monetdroid.Session, string, func(monetdroid.ServerMsg), string, *monetdroid.ProcessConfig) (*monetdroid.ClaudeProcess, error) = monetdroid.StartProcessWithConfig

// TestPermissionHandlerAPI verifies that PermissionRequest, PermissionHandler,
// PermResponse, and ProcessConfig compose correctly from an external package —
// ensuring the library's public API surface stays usable without importing
// internal wire types.
func TestPermissionHandlerAPI(t *testing.T) {
	handler := func(req monetdroid.PermissionRequest) monetdroid.PermResponse {
		if req.ToolName == "mcp__assistant__send_message" {
			return monetdroid.PermResponse{Allow: true}
		}
		return monetdroid.PermResponse{Allow: false}
	}

	cfg := &monetdroid.ProcessConfig{
		PermissionHandler:  handler,
		AppendSystemPrompt: "You are a helpful assistant.",
		AllowedTools:       "mcp__assistant__*",
		MaxTurns:           10,
	}

	// Verify the handler is callable with the config.
	resp := cfg.PermissionHandler(monetdroid.PermissionRequest{
		ToolName: "mcp__assistant__send_message",
	})
	if !resp.Allow {
		t.Fatal("expected allow for MCP tool")
	}

	resp = cfg.PermissionHandler(monetdroid.PermissionRequest{
		ToolName: "Bash",
	})
	if resp.Allow {
		t.Fatal("expected deny for non-MCP tool")
	}
}

func TestCommandAndExtraArgs(t *testing.T) {
	cfg := &monetdroid.ProcessConfig{
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
