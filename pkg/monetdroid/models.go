package monetdroid

// A model is a claude executable a session can run under. Models come from
// the -claude-bin value plus an optional scan of -model-dir, filtered by
// -model-pattern. The model dir is expected to be on PATH, so a name is all
// a session needs to run one. monetdroid lists the names and records which
// one a session uses, so a session's model is fixed and resumes respawn it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

const DefaultModelPattern = `^claude-`

// ModelScanSpec configures model discovery from a directory.
type ModelScanSpec struct {
	// Dir is scanned for model executables. Empty disables scanning.
	Dir string
	// Pattern is a regex applied to file base names. Empty selects
	// DefaultModelPattern.
	Pattern string
}

// discoverModels returns the names of the executable files in dir whose
// base name matches pattern. The caller orders them.
func discoverModels(dir, pattern string) ([]string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", pattern, err)
	}
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		full := filepath.Join(dir, de.Name())
		// os.Stat follows symlinks, so a symlinked model (claude-opus
		// pointing at the real claude) lists when its target is executable.
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			continue
		}
		if !re.MatchString(de.Name()) {
			continue
		}
		names = append(names, de.Name())
	}
	return names, nil
}

// renderModelSelect renders the model select for #model-row. The value
// commits with the /send form. The unset variant adds a required
// placeholder, so the browser blocks submission until a model is chosen,
// and no entry is preselected. Otherwise the preselect, or the default when
// preselect is empty or names nothing, is selected. An empty string renders
// nothing, hiding the row.
func (h *Hub) renderModelSelect(preselect string, unset bool) string {
	if len(h.sortedModels) <= 1 {
		return ""
	}
	selected := h.defaultModel
	if preselect != "" && slices.Contains(h.sortedModels, preselect) {
		selected = preselect
	}
	var b strings.Builder
	b.WriteString(`<select name="model" id="model-select" class="model-select"`)
	if unset {
		b.WriteString(` required><option value="" selected disabled>Model…</option>`)
	} else {
		b.WriteString(`>`)
	}
	for _, name := range h.sortedModels {
		fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`, Esc(name), selectedAttr(!unset && name == selected), Esc(name))
	}
	b.WriteString(`</select>`)
	return b.String()
}

func selectedAttr(selected bool) string {
	if selected {
		return " selected"
	}
	return ""
}

// ModelCommandStore persists the invocation each session runs under
// (ClaudeID to executable name) across server restarts, keyed like
// LabelStore.
type ModelCommandStore struct {
	path     string
	commands map[string]string
	mu       sync.Mutex
}

func NewModelCommandStore(dir string) *ModelCommandStore {
	os.MkdirAll(dir, 0o755)
	ms := &ModelCommandStore{
		path:     filepath.Join(dir, "model-commands.json"),
		commands: make(map[string]string),
	}
	data, err := os.ReadFile(ms.path)
	if err == nil {
		json.Unmarshal(data, &ms.commands)
	}
	return ms
}

func (ms *ModelCommandStore) save() {
	data, _ := json.MarshalIndent(ms.commands, "", "  ")
	os.WriteFile(ms.path, data, 0o644)
}

// GetOrSet returns the command recorded for a session, recording cmd and
// returning it when none is. The first writer wins, so a session's model
// never changes once recorded. Callers must spawn under the returned
// command, keeping the process and the record in agreement.
func (ms *ModelCommandStore) GetOrSet(claudeID, cmd string) string {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if existing, ok := ms.commands[claudeID]; ok {
		return existing
	}
	ms.commands[claudeID] = cmd
	ms.save()
	return cmd
}

// Get returns the command recorded for a session, or empty when none is.
func (ms *ModelCommandStore) Get(claudeID string) string {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.commands[claudeID]
}
