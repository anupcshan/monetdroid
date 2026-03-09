package monetdroid

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

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
			summary, sessionCwd := GetSessionInfo(sf)
			if cwd == "" && sessionCwd != "" {
				cwd = sessionCwd
			}
			sessions = append(sessions, HistorySession{
				ID: sessionID, Summary: summary,
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

func GetSessionInfo(fpath string) (summary, cwd string) {
	f, err := os.Open(fpath)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for i := 0; i < 30 && scanner.Scan(); i++ {
		var obj map[string]any
		if err := json.Unmarshal([]byte(scanner.Text()), &obj); err != nil {
			continue
		}
		if c, ok := obj["cwd"].(string); ok && cwd == "" {
			cwd = c
		}
		if obj["type"] == "user" {
			if msg, ok := obj["message"].(map[string]any); ok {
				summary = ExtractTextContent(msg["content"])
				if summary != "" {
					summary = Truncate(summary, 120)
					return
				}
			}
		}
	}
	return
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

func ParseSessionMessages(jsonlPath string) (msgs []HistoryMessage, claudeID string, usage SessionUsage, err error) {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return nil, "", usage, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
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
				for _, block := range c {
					b, ok := block.(map[string]any)
					if !ok {
						continue
					}
					bt, _ := b["type"].(string)
					switch bt {
					case "text":
						if t, _ := b["text"].(string); t != "" {
							msgs = append(msgs, HistoryMessage{Type: "user", Text: t})
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
						msgs = append(msgs, HistoryMessage{Type: "tool_result", Output: Truncate(output, 2000)})
					}
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
					msgs = append(msgs, HistoryMessage{Type: "tool_use", Tool: name, Input: b["input"]})
				}
			}
		case "result":
			if sid, ok := obj["session_id"].(string); ok && sid != "" {
				claudeID = sid
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
	return msgs, claudeID, usage, scanner.Err()
}
