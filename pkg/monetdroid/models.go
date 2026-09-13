package monetdroid

// Models are claude CLI invocations a session can run under. The set comes
// from the -claude-bin default plus an optional directory scan
// (-model-dir, filtered by -model-pattern). The wrapper scripts in the
// scan dir are the single source of truth for what a model is. monetdroid
// only lists them and records which invocation a session uses, so the
// model is fixed per session and resumes respawn it across restarts.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const DefaultModelPattern = `^claude-`

// ModelEntry is one selectable claude invocation.
type ModelEntry struct {
	PickerLabel string
	Command     string
	// IsDefault marks the entry used when no model is chosen, which is
	// the -claude-bin value or plain "claude" when unset.
	IsDefault bool
}

// ModelScanSpec configures model discovery from a directory.
type ModelScanSpec struct {
	// Dir is scanned for model executables. Empty disables scanning.
	Dir string
	// Pattern is a regex applied to file base names. Empty selects
	// DefaultModelPattern.
	Pattern string
}

// discoverModels lists executable files in dir whose base name matches
// pattern. The picker label is the base name. Entries are ordered by label.
func discoverModels(dir, pattern string) ([]ModelEntry, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %w", pattern, err)
	}
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var models []ModelEntry
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
		models = append(models, ModelEntry{PickerLabel: de.Name(), Command: full})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].PickerLabel < models[j].PickerLabel })
	return models, nil
}

// defaultModelEntry derives the picker entry for the -claude-bin value.
// An empty value resolves to the plain "claude" in PATH, spelled out so
// an empty session command always means "no model chosen yet".
func defaultModelEntry(command string) ModelEntry {
	cmd := command
	if cmd == "" {
		cmd = "claude"
	}
	return ModelEntry{PickerLabel: filepath.Base(cmd), Command: cmd, IsDefault: true}
}

// mergeModels returns one entry per command, ordered by label.
func mergeModels(def ModelEntry, scanned []ModelEntry) []ModelEntry {
	out := []ModelEntry{def}
	for _, s := range scanned {
		if s.Command != def.Command {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PickerLabel < out[j].PickerLabel })
	return out
}

// resolveModel maps a picker label to its entry. An empty label selects
// the default entry. The model list is fixed at construction, so no lock
// is needed.
func (h *Hub) resolveModel(label string) (ModelEntry, bool) {
	for _, m := range h.selectableModels {
		if label == "" && m.IsDefault {
			return m, true
		}
		if m.PickerLabel == label {
			return m, true
		}
	}
	return ModelEntry{}, false
}

// labelForCommand returns the picker label naming cmd, or "" when no entry
// matches.
func (h *Hub) labelForCommand(cmd string) string {
	for _, m := range h.selectableModels {
		if m.Command == cmd {
			return m.PickerLabel
		}
	}
	return ""
}

// ModelCommandStore persists the invocation each session runs under
// (ClaudeID to executable path) across server restarts, keyed like
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

// renderModelSelect renders the model select for #model-row. The value
// commits with the /send form. The unset variant adds a required
// placeholder, so the browser blocks submission until a model is chosen,
// and no entry is preselected. Otherwise the preselect entry, or the
// default when preselect is empty or names nothing, is selected. An empty
// string renders nothing, hiding the row.
func (h *Hub) renderModelSelect(preselect string, unset bool) string {
	if len(h.selectableModels) <= 1 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<select name="model" id="model-select" class="model-select"`)
	if unset {
		b.WriteString(` required><option value="" selected disabled>Model…</option>`)
	} else {
		b.WriteString(`>`)
	}
	for _, m := range h.selectableModels {
		selected := false
		if !unset {
			selected = m.IsDefault
			if preselect != "" {
				selected = m.PickerLabel == preselect
			}
		}
		fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`, Esc(m.PickerLabel), selectedAttr(selected), Esc(m.PickerLabel))
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
