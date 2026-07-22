// Package kbmcp exposes kb as a Model Context Protocol server. It binds
// the kb tools to pkg/kb so every mutation flows through kb's own API
// and inherits its per-change checkpointing. mcp-go is imported only
// here, never by the pkg/kb engine.
package kbmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/anupcshan/monetdroid/pkg/kb"
)

const serverVersion = "0.1.0"

// noStoreResult is returned when no kb store resolves for the server's
// working directory. The server still starts and advertises its tools so
// a globally registered kb server does not error out in a directory that
// has no store.
func noStoreResult() *mcp.CallToolResult {
	return mcp.NewToolResultError("no kb store for this directory")
}

// NewServer builds the kb MCP server with all tools bound to k. k may be
// nil, in which case tool calls report no store.
func NewServer(k *kb.KB) *server.MCPServer {
	var mu sync.Mutex
	s := server.NewMCPServer("kb", serverVersion,
		server.WithToolCapabilities(true),
		// pkg/kb commits each mutation against one shared git repo with no
		// locking. Serialize handler calls so concurrent tool dispatch
		// within this process cannot race the git index. The lock is per
		// process, so separate kb mcp processes on the same store can
		// still race its git index.
		server.WithToolHandlerMiddleware(func(h server.ToolHandlerFunc) server.ToolHandlerFunc {
			return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				mu.Lock()
				defer mu.Unlock()
				return h(ctx, req)
			}
		}),
	)
	registerTools(s, k)
	return s
}

// Serve runs the kb MCP server over stdio. It blocks until the client
// disconnects.
func Serve(k *kb.KB) error {
	return server.ServeStdio(NewServer(k))
}

func registerTools(s *server.MCPServer, k *kb.KB) {
	s.AddTool(listTool(), makeListHandler(k))
	s.AddTool(searchTool(), makeSearchHandler(k))
	s.AddTool(readTool(), makeReadHandler(k))
	s.AddTool(writeTool(), makeWriteHandler(k))
	s.AddTool(editTool(), makeEditHandler(k))
	s.AddTool(appendTool(), makeAppendHandler(k))
	s.AddTool(removeTool(), makeRemoveHandler(k))
	s.AddTool(moveTool(), makeMoveHandler(k))
}

func listTool() mcp.Tool {
	return mcp.NewTool("list",
		mcp.WithDescription("List every file in this repo's kb (knowledge base). kb is a persistent, per-repo store of plain-text notes shared across Claude sessions. Use this to see what has been recorded. Entries typically live under paths like projects/<slug>.md."),
	)
}

func makeListHandler(k *kb.KB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if k == nil {
			return noStoreResult(), nil
		}
		files, err := k.List()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("kb list: %v", err)), nil
		}
		if files == nil {
			files = []string{}
		}
		b, err := json.Marshal(files)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("kb list: %v", err)), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	}
}

func searchTool() mcp.Tool {
	return mcp.NewTool("search",
		mcp.WithDescription("Search kb file contents with a regular expression (git grep). Use this to find notes related to a topic, for example to locate a project entry the user refers to by name."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Regular expression to search for")),
	)
}

func makeSearchHandler(k *kb.KB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if k == nil {
			return noStoreResult(), nil
		}
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: query"), nil
		}
		out, err := k.Search(query)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("kb search: %v", err)), nil
		}
		return mcp.NewToolResultText(out), nil
	}
}

func readTool() mcp.Tool {
	return mcp.NewTool("read",
		mcp.WithDescription("Read a kb file by path. When resuming work the user refers to by name, search or list to find the entry (often projects/<slug>.md), then read it to recover context before doing anything else."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Path within the kb store")),
		mcp.WithNumber("offset", mcp.Description("Starting line number, 0-indexed (default 0)")),
		mcp.WithNumber("limit", mcp.Description("Number of lines to read, 0 for the rest of the file (default 0)")),
	)
}

func makeReadHandler(k *kb.KB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if k == nil {
			return noStoreResult(), nil
		}
		path, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: path"), nil
		}
		content, err := k.Read(path, req.GetInt("offset", 0), req.GetInt("limit", 0))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("kb read: %v", err)), nil
		}
		return mcp.NewToolResultText(content), nil
	}
}

func writeTool() mcp.Tool {
	return mcp.NewTool("write",
		mcp.WithDescription("Create or overwrite a kb file. When the user asks you to plan or build something new, create projects/<slug>.md with a short plan and a Status section (what is done, what is next) so a future session can resume. Keep one file per project. Use kb for project plans and progress instead of built-in plan mode."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Path within the kb store")),
		mcp.WithString("content", mcp.Required(), mcp.Description("Full file content")),
	)
}

func makeWriteHandler(k *kb.KB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if k == nil {
			return noStoreResult(), nil
		}
		path, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: path"), nil
		}
		content, err := req.RequireString("content")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: content"), nil
		}
		if err := k.Write(path, content); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("kb write: %v", err)), nil
		}
		return mcp.NewToolResultText("wrote " + path), nil
	}
}

func editTool() mcp.Tool {
	return mcp.NewTool("edit",
		mcp.WithDescription("Replace an exact string in a kb file. old_string must appear exactly once unless replace_all is set. The call is rejected otherwise. Use this for targeted updates such as advancing a Status section, rather than overwriting the whole file. Keep kb current as work progresses: when a phase ships or a decision changes, update the relevant kb file immediately rather than waiting to be asked."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Path within the kb store")),
		mcp.WithString("old_string", mcp.Required(), mcp.Description("The exact string to replace")),
		mcp.WithString("new_string", mcp.Required(), mcp.Description("The replacement string")),
		mcp.WithBoolean("replace_all", mcp.Description("Replace every occurrence instead of requiring a unique match (default false)")),
	)
}

func makeEditHandler(k *kb.KB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if k == nil {
			return noStoreResult(), nil
		}
		path, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: path"), nil
		}
		oldStr, err := req.RequireString("old_string")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: old_string"), nil
		}
		newStr, err := req.RequireString("new_string")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: new_string"), nil
		}
		if err := k.Edit(path, kb.EditInput{Old: oldStr, New: newStr}, req.GetBool("replace_all", false)); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("kb edit: %v", err)), nil
		}
		return mcp.NewToolResultText("edited " + path), nil
	}
}

func appendTool() mcp.Tool {
	return mcp.NewTool("append",
		mcp.WithDescription("Append content to a kb file, creating it if it does not exist. Useful for checkpointing progress onto an existing entry."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Path within the kb store")),
		mcp.WithString("content", mcp.Required(), mcp.Description("Content to append")),
	)
}

func makeAppendHandler(k *kb.KB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if k == nil {
			return noStoreResult(), nil
		}
		path, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: path"), nil
		}
		content, err := req.RequireString("content")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: content"), nil
		}
		if err := k.Append(path, content); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("kb append: %v", err)), nil
		}
		return mcp.NewToolResultText("appended " + path), nil
	}
}

func removeTool() mcp.Tool {
	return mcp.NewTool("rm",
		mcp.WithDescription("Delete a kb file."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Path within the kb store")),
	)
}

func makeRemoveHandler(k *kb.KB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if k == nil {
			return noStoreResult(), nil
		}
		path, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: path"), nil
		}
		if err := k.Remove(path); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("kb rm: %v", err)), nil
		}
		return mcp.NewToolResultText("removed " + path), nil
	}
}

func moveTool() mcp.Tool {
	return mcp.NewTool("mv",
		mcp.WithDescription("Move or rename a kb file."),
		mcp.WithString("old_path", mcp.Required(), mcp.Description("Current path within the kb store")),
		mcp.WithString("new_path", mcp.Required(), mcp.Description("New path within the kb store")),
	)
}

func makeMoveHandler(k *kb.KB) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if k == nil {
			return noStoreResult(), nil
		}
		oldPath, err := req.RequireString("old_path")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: old_path"), nil
		}
		newPath, err := req.RequireString("new_path")
		if err != nil {
			return mcp.NewToolResultError("missing required parameter: new_path"), nil
		}
		if err := k.Move(oldPath, newPath); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("kb mv: %v", err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("moved %s to %s", oldPath, newPath)), nil
	}
}
