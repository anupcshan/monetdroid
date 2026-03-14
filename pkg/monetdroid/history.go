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
		var obj map[string]any
		if err := json.Unmarshal([]byte(scanner.Text()), &obj); err != nil {
			continue
		}
		if c, ok := obj["cwd"].(string); ok && cwd == "" {
			cwd = c
		}
		msgType, _ := obj["type"].(string)
		switch msgType {
		case "user", "assistant", "result":
			numMsgs++
			if summary == "" && msgType == "user" {
				if msg, ok := obj["message"].(map[string]any); ok {
					summary = Truncate(ExtractTextContent(msg["content"]), 120)
				}
			}
		}
	}
	return summary, cwd, numMsgs, scanner.Err()
}

func ExtractTextContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		for _, block := range c {
			if b, ok := block.(map[string]any); ok {
				if b["type"] == "text" {
					if t, ok := b["text"].(string); ok && t != "" {
						return t
					}
				}
			}
		}
	}
	return ""
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
		line := scanner.Text()
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		if c, ok := obj["cwd"].(string); ok && cwd == "" {
			cwd = c
		}
		if sc, ok := obj["isSidechain"].(bool); ok && sc {
			continue
		}
		msgType, _ := obj["type"].(string)
		switch msgType {
		case "user":
			if sid, ok := obj["sessionId"].(string); ok && sid != "" {
				claudeID = sid
			}
			msg, ok := obj["message"].(map[string]any)
			if !ok {
				continue
			}
			switch c := msg["content"].(type) {
			case string:
				if c != "" {
					msgs = append(msgs, HistoryMessage{Type: "user", Text: c})
				}
			case []any:
				var userText string
				var userImages []ImageData
				for _, block := range c {
					b, ok := block.(map[string]any)
					if !ok {
						continue
					}
					bt, _ := b["type"].(string)
					switch bt {
					case "text":
						if t, _ := b["text"].(string); t != "" {
							userText = t
						}
					case "image":
						src, _ := b["source"].(map[string]any)
						if src != nil {
							mt, _ := src["media_type"].(string)
							data, _ := src["data"].(string)
							if mt != "" && data != "" {
								userImages = append(userImages, ImageData{MediaType: mt, Data: data})
							}
						}
					case "tool_result":
						output := ""
						switch rc := b["content"].(type) {
						case string:
							output = rc
						default:
							j, _ := json.Marshal(rc)
							output = string(j)
						}
						tuID, _ := b["tool_use_id"].(string)
						toolName := toolNames[tuID]
						if !isBoringResult(output) {
							msgs = append(msgs, HistoryMessage{Type: "tool_result", Tool: toolName, ToolUseID: tuID, Output: Truncate(output, 2000)})
						}
					}
				}
				if userText != "" || len(userImages) > 0 {
					msgs = append(msgs, HistoryMessage{Type: "user", Text: userText, Images: userImages})
				}
			}
		case "assistant":
			msg, ok := obj["message"].(map[string]any)
			if !ok {
				continue
			}
			if u, ok := msg["usage"].(map[string]any); ok {
				inTok, _ := u["input_tokens"].(float64)
				outTok, _ := u["output_tokens"].(float64)
				cacheRead, _ := u["cache_read_input_tokens"].(float64)
				cacheCreate, _ := u["cache_creation_input_tokens"].(float64)
				ctx := int(inTok + cacheRead + cacheCreate + outTok)
				if ctx > 0 {
					usage.ContextUsed = ctx
				}
			}
			content, ok := msg["content"].([]any)
			if !ok {
				continue
			}
			for _, block := range content {
				b, ok := block.(map[string]any)
				if !ok {
					continue
				}
				blockType, _ := b["type"].(string)
				switch blockType {
				case "text":
					if text, _ := b["text"].(string); text != "" {
						msgs = append(msgs, HistoryMessage{Type: "assistant", Text: text})
					}
				case "tool_use":
					name, _ := b["name"].(string)
					id, _ := b["id"].(string)
					if id != "" {
						toolNames[id] = name
					}
					msgs = append(msgs, HistoryMessage{Type: "tool_use", Tool: name, ToolUseID: id, Input: b["input"]})
				}
			}
		case "result":
			if sid, ok := obj["session_id"].(string); ok && sid != "" {
				claudeID = sid
			}
			if totalCost, ok := obj["total_cost_usd"].(float64); ok {
				usage.TotalCostUSD = totalCost
			}
			if mu, ok := obj["modelUsage"].(map[string]any); ok {
				for _, v := range mu {
					if info, ok := v.(map[string]any); ok {
						if cw, ok := info["contextWindow"].(float64); ok && cw > 0 {
							usage.ContextWindow = int(cw)
						}
					}
					break
				}
			}
		}
	}
	return msgs, claudeID, cwd, usage, scanner.Err()
}
