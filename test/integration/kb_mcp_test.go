package integration

// kb_mcp_test.go exercises the `kb mcp` server inside a container. kb
// operations must never run uncontainerized in tests. A resolution mistake
// would clobber the host's real kb store. Each test starts an isolated
// container, points kb at a store inside it, and drives `kb mcp` over its
// real stdio transport with mcp-go's stdio client.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// setupKBContainer starts a detached container with the current test binary
// mounted at /test and a git repo at /work. It deliberately avoids the LLM
// replayer, cassette, and browser that SetupWithContainer wires up. A server
// correctness test has no agent in the loop. The kb mcp server is driven
// directly over stdio by mcpClient.
func setupKBContainer(t *testing.T) *ContainerFixture {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found; kb mcp integration tests require docker")
	}
	buildDockerImage(t)

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	out, err := exec.Command("docker", "run", "--rm", "-d",
		"-v", testBinary+":/test:ro",
		dockerImage,
		"timeout", fmt.Sprintf("%d", containerTimeout), "sleep", "infinity",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	containerID := strings.TrimSpace(string(out))
	t.Logf("started kb mcp container %s", containerID[:12])
	t.Cleanup(func() { exec.Command("docker", "stop", "-t", "5", containerID).Run() })

	// kb commits each mutation in its own git repo under /root/.monetdroid/kb,
	// so the container needs a git identity. Set it globally like the LLM
	// harness does.
	for _, cfg := range [][]string{
		{"git", "config", "--global", "user.email", "test@test.com"},
		{"git", "config", "--global", "user.name", "Test"},
	} {
		if out, err := exec.Command("docker", append([]string{"exec", containerID}, cfg...)...).CombinedOutput(); err != nil {
			t.Fatalf("git config %v: %v\n%s", cfg, err, out)
		}
	}

	f := &ContainerFixture{T: t, containerID: containerID}
	initGitRepo(t, f, containerWorkdir)
	return f
}

// mcpClient spawns `kb mcp` inside the container at cwd and returns an
// initialized MCP client talking to it over stdio. Closing the client ends
// the kb mcp process. The container itself is cleaned up by setupKBContainer.
func mcpClient(t *testing.T, f *ContainerFixture, cwd string) *client.Client {
	t.Helper()
	c, err := client.NewStdioMCPClient("docker", nil,
		"exec", "-i", "-w", cwd, f.containerID, "kb", "mcp")
	if err != nil {
		t.Fatalf("NewStdioMCPClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("kb mcp client start: %v", err)
	}
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		t.Fatalf("kb mcp initialize: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func mcpCall(t *testing.T, c *client.Client, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: args},
	})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

func mcpText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	for _, content := range res.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatalf("result has no text content: %+v", res)
	return ""
}

func TestKBMCP(t *testing.T) {
	f := setupKBContainer(t)
	t.Run("round_trip", func(t *testing.T) { testKBMCPRoundTrip(t, f) })
	t.Run("edit_uniqueness", func(t *testing.T) { testKBMCPEditUniqueness(t, f) })
	t.Run("move_remove", func(t *testing.T) { testKBMCPMoveRemove(t, f) })
	t.Run("no_store", func(t *testing.T) { testKBMCPNoStore(t, f) })
}

func testKBMCPRoundTrip(t *testing.T, f *ContainerFixture) {
	c := mcpClient(t, f, containerWorkdir)

	// All eight tools are advertised.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	have := make(map[string]bool)
	for _, tool := range tools.Tools {
		have[tool.Name] = true
	}
	for _, name := range []string{"list", "search", "read", "write", "edit", "append", "rm", "mv"} {
		if !have[name] {
			t.Errorf("server did not advertise %s; tools: %v", name, have)
		}
	}

	if res := mcpCall(t, c, "write", map[string]any{
		"path":    "projects/alpha.md",
		"content": "# Alpha\n\nStatus: starting.\n",
	}); res.IsError {
		t.Fatalf("kb_write: %s", mcpText(t, res))
	}

	res := mcpCall(t, c, "read", map[string]any{"path": "projects/alpha.md"})
	if res.IsError {
		t.Fatalf("kb_read: %s", mcpText(t, res))
	}
	if text := mcpText(t, res); !strings.Contains(text, "# Alpha") {
		t.Errorf("kb_read missing content, got %q", text)
	}

	if res := mcpCall(t, c, "list", nil); res.IsError {
		t.Fatalf("kb_list: %s", mcpText(t, res))
	} else if !strings.Contains(mcpText(t, res), "projects/alpha.md") {
		t.Errorf("kb_list missing projects/alpha.md, got %q", mcpText(t, res))
	}

	if res := mcpCall(t, c, "edit", map[string]any{
		"path":       "projects/alpha.md",
		"old_string": "Status: starting.",
		"new_string": "Status: done.",
	}); res.IsError {
		t.Fatalf("kb_edit: %s", mcpText(t, res))
	}
	res = mcpCall(t, c, "read", map[string]any{"path": "projects/alpha.md"})
	if text := mcpText(t, res); !strings.Contains(text, "Status: done.") || strings.Contains(text, "starting.") {
		t.Errorf("kb_edit did not apply, got %q", text)
	}

	if res := mcpCall(t, c, "append", map[string]any{
		"path":    "projects/alpha.md",
		"content": "More lines.\n",
	}); res.IsError {
		t.Fatalf("kb_append: %s", mcpText(t, res))
	}
	res = mcpCall(t, c, "read", map[string]any{"path": "projects/alpha.md"})
	if !strings.Contains(mcpText(t, res), "More lines.") {
		t.Errorf("kb_append did not apply, got %q", mcpText(t, res))
	}

	if res := mcpCall(t, c, "search", map[string]any{"query": "Alpha"}); res.IsError {
		t.Fatalf("kb_search: %s", mcpText(t, res))
	} else if !strings.Contains(mcpText(t, res), "alpha.md") {
		t.Errorf("kb_search missing hit, got %q", mcpText(t, res))
	}
}

func testKBMCPEditUniqueness(t *testing.T, f *ContainerFixture) {
	c := mcpClient(t, f, containerWorkdir)

	if res := mcpCall(t, c, "write", map[string]any{
		"path":    "dup.md",
		"content": "tag: x\ntag: x\n",
	}); res.IsError {
		t.Fatalf("kb_write: %s", mcpText(t, res))
	}

	// Non-unique old_string without replace_all is rejected.
	res := mcpCall(t, c, "edit", map[string]any{
		"path":       "dup.md",
		"old_string": "tag: x",
		"new_string": "tag: y",
	})
	if !res.IsError {
		t.Errorf("kb_edit accepted a non-unique old_string")
	}

	// replace_all applies to every occurrence.
	res = mcpCall(t, c, "edit", map[string]any{
		"path":        "dup.md",
		"old_string":  "tag: x",
		"new_string":  "tag: z",
		"replace_all": true,
	})
	if res.IsError {
		t.Errorf("kb_edit replace_all failed: %s", mcpText(t, res))
	}
	res = mcpCall(t, c, "read", map[string]any{"path": "dup.md"})
	if text := mcpText(t, res); strings.Contains(text, "tag: x") || !strings.Contains(text, "tag: z") {
		t.Errorf("kb_edit replace_all did not apply, got %q", text)
	}
}

func testKBMCPMoveRemove(t *testing.T, f *ContainerFixture) {
	c := mcpClient(t, f, containerWorkdir)

	if res := mcpCall(t, c, "write", map[string]any{"path": "a.md", "content": "body"}); res.IsError {
		t.Fatalf("kb_write: %s", mcpText(t, res))
	}
	if res := mcpCall(t, c, "mv", map[string]any{"old_path": "a.md", "new_path": "b.md"}); res.IsError {
		t.Fatalf("kb_mv: %s", mcpText(t, res))
	}
	res := mcpCall(t, c, "read", map[string]any{"path": "b.md"})
	if res.IsError || !strings.Contains(mcpText(t, res), "body") {
		t.Errorf("kb_mv did not move content")
	}
	if res := mcpCall(t, c, "rm", map[string]any{"path": "b.md"}); res.IsError {
		t.Fatalf("kb_rm: %s", mcpText(t, res))
	}
	if res := mcpCall(t, c, "read", map[string]any{"path": "b.md"}); !res.IsError {
		t.Errorf("kb_rm did not remove the file")
	}
}

// testKBMCPNoStore runs `kb mcp` from a non-git directory (/tmp). kb cannot
// resolve a store, so every tool reports no store rather than touching the
// host's real kb.
func testKBMCPNoStore(t *testing.T, f *ContainerFixture) {
	c := mcpClient(t, f, "/tmp")
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"list", nil},
		{"read", map[string]any{"path": "x.md"}},
		{"write", map[string]any{"path": "x.md", "content": "x"}},
	} {
		res := mcpCall(t, c, tc.name, tc.args)
		if !res.IsError {
			t.Errorf("%s on a directory with no store succeeded; expected no-store error", tc.name)
		} else if !strings.Contains(mcpText(t, res), "no kb store") {
			t.Errorf("%s no-store case gave unexpected error %q", tc.name, mcpText(t, res))
		}
	}
}
