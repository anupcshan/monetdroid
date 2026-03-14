package monetdroid

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// JSONL schema types — minimal subset of fields we need from Claude session files.

type jsonlEntry struct {
	CWD         string                    `json:"cwd"`
	Type        string                    `json:"type"`
	IsSidechain bool                      `json:"isSidechain"`
	SessionID   string                    `json:"sessionId"`
	ResultSID   string                    `json:"session_id"`
	TotalCost   float64                   `json:"total_cost_usd"`
	ModelUsage  map[string]modelUsageInfo `json:"modelUsage"`
	Message     jsonlMessage              `json:"message"`
}

type jsonlMessage struct {
	Content messageContent `json:"content"`
	Usage   *jsonlUsage    `json:"usage,omitempty"`
}

// messageContent handles the polymorphic content field: plain string or array of blocks.
type messageContent struct {
	Text   string         // set when content is a plain string
	Blocks []contentBlock // set when content is an array
}

func (c *messageContent) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, &c.Text) == nil {
		return nil
	}
	return json.Unmarshal(data, &c.Blocks)
}

// FirstText returns the first text content — either the plain string or the first text block.
func (c *messageContent) FirstText() string {
	if c.Text != "" {
		return c.Text
	}
	for _, b := range c.Blocks {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}

type jsonlUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type modelUsageInfo struct {
	ContextWindow int `json:"contextWindow"`
}

type contentBlock struct {
	Type      string       `json:"type"`
	Text      string       `json:"text,omitempty"`
	Name      string       `json:"name,omitempty"`
	ID        string       `json:"id,omitempty"`
	Input     *ToolInput   `json:"input,omitempty"`
	ToolUseID string       `json:"tool_use_id,omitempty"`
	Content   blockContent `json:"content,omitempty"`
	Source    *imageSource `json:"source,omitempty"`
}

// blockContent handles the polymorphic tool_result content: plain string or complex object.
type blockContent struct {
	Text string // set when content is a plain string
	Raw  string // JSON string fallback for non-string content
}

func (c *blockContent) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, &c.Text) == nil {
		return nil
	}
	c.Raw = string(data)
	return nil
}

func (c *blockContent) String() string {
	if c.Text != "" {
		return c.Text
	}
	return c.Raw
}

type imageSource struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// Cache

type cachedSessionInfo struct {
	modTime time.Time
	summary string
	cwd     string
	numMsgs int
}

type sessionInfoCache struct {
	mu      sync.Mutex
	entries map[string]cachedSessionInfo
}

var historyCache = &sessionInfoCache{
	entries: make(map[string]cachedSessionInfo),
}

func (c *sessionInfoCache) get(fpath string, modTime time.Time) (summary, cwd string, numMsgs int, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, hit := c.entries[fpath]; hit && e.modTime.Equal(modTime) {
		return e.summary, e.cwd, e.numMsgs, true
	}

	summary, cwd, numMsgs, err := parseSessionInfo(fpath)
	if err != nil {
		return "", "", 0, false
	}
	c.entries[fpath] = cachedSessionInfo{modTime: modTime, summary: summary, cwd: cwd, numMsgs: numMsgs}
	return summary, cwd, numMsgs, true
}

func ScanHistory() ([]HistoryGroup, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, err
	}

	var groups []HistoryGroup
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirPath := filepath.Join(projectsDir, entry.Name())
		sessionFiles, err := filepath.Glob(filepath.Join(dirPath, "*.jsonl"))
		if err != nil || len(sessionFiles) == 0 {
			continue
		}

		var cwd string
		var sessions []HistorySession
		for _, sf := range sessionFiles {
			info, err := os.Stat(sf)
			if err != nil {
				continue
			}
			sessionID := strings.TrimSuffix(filepath.Base(sf), ".jsonl")
			summary, sessionCwd, numMsgs, ok := historyCache.get(sf, info.ModTime())
			if !ok {
				continue
			}
			if cwd == "" && sessionCwd != "" {
				cwd = sessionCwd
			}
			if numMsgs == 0 {
				continue
			}
			sessions = append(sessions, HistorySession{
				ID: sessionID, Summary: summary, NumMsgs: numMsgs,
				ModTime: info.ModTime().Format(time.RFC3339), ModUnix: info.ModTime().Unix(),
			})
		}
		if cwd == "" {
			cwd = "/" + strings.ReplaceAll(entry.Name(), "-", "/")
		}
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].ModUnix > sessions[j].ModUnix })
		groups = append(groups, HistoryGroup{Dir: cwd, DirKey: entry.Name(), Sessions: sessions})
	}
	sort.Slice(groups, func(i, j int) bool {
		if len(groups[i].Sessions) == 0 {
			return false
		}
		if len(groups[j].Sessions) == 0 {
			return true
		}
		return groups[i].Sessions[0].ModUnix > groups[j].Sessions[0].ModUnix
	})
	return groups, nil
}

func FindJSONLByClaudeID(claudeID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", claudeID+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func parseSessionInfo(fpath string) (summary string, cwd string, numMsgs int, err error) {
	f, err := os.Open(fpath)
	if err != nil {
		return "", "", 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		var entry jsonlEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.CWD != "" {
			cwd = entry.CWD
		}
		switch entry.Type {
		case "user", "assistant", "result":
			numMsgs++
			if summary == "" && entry.Type == "user" {
				if t := entry.Message.Content.FirstText(); t != "" {
					summary = Truncate(t, 120)
				}
			}
		}
	}
	return summary, cwd, numMsgs, scanner.Err()
}

func ParseSessionMessages(jsonlPath string) (msgs []HistoryMessage, claudeID string, cwd string, usage SessionUsage, err error) {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return nil, "", "", usage, err
	}
	defer f.Close()
	toolNames := make(map[string]string) // tool_use id → tool name
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var entry jsonlEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.CWD != "" {
			cwd = entry.CWD
		}
		if entry.IsSidechain {
			continue
		}
		switch entry.Type {
		case "user":
			if entry.SessionID != "" {
				claudeID = entry.SessionID
			}
			c := entry.Message.Content
			if c.Text != "" && len(c.Blocks) == 0 {
				msgs = append(msgs, HistoryMessage{Type: "user", Text: c.Text})
				continue
			}
			var userText string
			var userImages []ImageData
			for _, b := range c.Blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						userText = b.Text
					}
				case "image":
					if b.Source != nil && b.Source.MediaType != "" && b.Source.Data != "" {
						userImages = append(userImages, ImageData{MediaType: b.Source.MediaType, Data: b.Source.Data})
					}
				case "tool_result":
					output := b.Content.String()
					toolName := toolNames[b.ToolUseID]
					if !isBoringResult(output) {
						msgs = append(msgs, HistoryMessage{Type: "tool_result", Tool: toolName, ToolUseID: b.ToolUseID, Output: Truncate(output, 2000)})
					}
				}
			}
			if userText != "" || len(userImages) > 0 {
				msgs = append(msgs, HistoryMessage{Type: "user", Text: userText, Images: userImages})
			}
		case "assistant":
			if u := entry.Message.Usage; u != nil {
				ctx := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens
				if ctx > 0 {
					usage.ContextUsed = ctx
				}
			}
			for _, b := range entry.Message.Content.Blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						msgs = append(msgs, HistoryMessage{Type: "assistant", Text: b.Text})
					}
				case "tool_use":
					if b.ID != "" {
						toolNames[b.ID] = b.Name
					}
					msgs = append(msgs, HistoryMessage{Type: "tool_use", Tool: b.Name, ToolUseID: b.ID, Input: b.Input})
				}
			}
		case "result":
			if entry.ResultSID != "" {
				claudeID = entry.ResultSID
			}
			if entry.TotalCost > 0 {
				usage.TotalCostUSD = entry.TotalCost
			}
			for _, info := range entry.ModelUsage {
				if info.ContextWindow > 0 {
					usage.ContextWindow = info.ContextWindow
				}
				break
			}
		}
	}
	return msgs, claudeID, cwd, usage, scanner.Err()
}
