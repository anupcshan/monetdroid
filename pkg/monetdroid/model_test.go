package monetdroid

import (
	"testing"
)

type activityStep struct {
	msg           ServerMsg
	wantActivity  bool
	wantInterrupt bool
}

func newTestModel() *SessionModel {
	return &SessionModel{
		BgTasks:           make(map[string]*BgTaskState),
		ToolUseIndexes:    make(map[string]int),
		ToolResultIndexes: make(map[string]int),
		ToolResults:       make(map[string]ServerMsg),
		SuppressedIDs:     make(map[string]bool),
		PendingPerms:      make(map[string]ServerMsg),
		SubagentSections:  make(map[string]*SubagentSection),
		LastCompact:       -1,
		pendingCommands:   make(map[string]string),
	}
}

func TestActivityStateTransitions(t *testing.T) {
	tests := []struct {
		name  string
		steps []activityStep
	}{
		{
			name: "basic turn",
			steps: []activityStep{
				{ServerMsg{Type: "session_started"}, false, false},
				{ServerMsg{Type: "running"}, true, true},
				{ServerMsg{Type: "done"}, false, false},
			},
		},
		{
			name: "permission flow",
			steps: []activityStep{
				{ServerMsg{Type: "session_started"}, false, false},
				{ServerMsg{Type: "running"}, true, true},
				{ServerMsg{Type: "permission_request", PermTool: "Bash", ToolUseID: "perm1"}, true, true},
				{ServerMsg{Type: "tool_result", ToolUseID: "perm1"}, true, true},
				{ServerMsg{Type: "done"}, false, false},
			},
		},
		{
			name: "bg task outlives turn",
			steps: []activityStep{
				{ServerMsg{Type: "session_started"}, false, false},
				{ServerMsg{Type: "running"}, true, true},
				{ServerMsg{Type: "tool_result", ToolUseID: "bg1", Output: "Output is being written to: /tmp/out.output"}, true, true},
				{ServerMsg{Type: "done"}, true, false},
				{ServerMsg{Type: "task_done", ToolUseID: "bg1"}, false, false},
			},
		},
		{
			name: "sub-agent outlives turn",
			steps: []activityStep{
				{ServerMsg{Type: "session_started"}, false, false},
				{ServerMsg{Type: "running"}, true, true},
				{ServerMsg{Type: "subagent_started", AgentID: "agent1"}, true, true},
				{ServerMsg{Type: "done"}, true, false},
				{ServerMsg{Type: "subagent_stopped", AgentID: "agent1"}, false, false},
			},
		},
		{
			name: "session restart clears stale bg tasks",
			steps: []activityStep{
				{ServerMsg{Type: "session_started"}, false, false},
				{ServerMsg{Type: "running"}, true, true},
				{ServerMsg{Type: "tool_result", ToolUseID: "bg1", Output: "Output is being written to: /tmp/out.output"}, true, true},
				{ServerMsg{Type: "done"}, true, false},
				// New session starts without task_done arriving.
				{ServerMsg{Type: "session_started"}, false, false},
			},
		},
		{
			name: "session restart clears stale sub-agents",
			steps: []activityStep{
				{ServerMsg{Type: "session_started"}, false, false},
				{ServerMsg{Type: "running"}, true, true},
				{ServerMsg{Type: "subagent_started", AgentID: "agent1"}, true, true},
				{ServerMsg{Type: "done"}, true, false},
				// New session starts without subagent_stopped arriving.
				{ServerMsg{Type: "session_started"}, false, false},
			},
		},
		{
			name: "session restart clears stale turn state",
			steps: []activityStep{
				{ServerMsg{Type: "session_started"}, false, false},
				{ServerMsg{Type: "running"}, true, true},
				// New session starts without done arriving.
				{ServerMsg{Type: "session_started"}, false, false},
			},
		},
		{
			name: "process death clears activity",
			steps: []activityStep{
				{ServerMsg{Type: "session_started"}, false, false},
				{ServerMsg{Type: "running"}, true, true},
				{ServerMsg{Type: "session_ended"}, false, false},
			},
		},
		{
			name: "permission request alone without turn",
			steps: []activityStep{
				{ServerMsg{Type: "session_started"}, false, false},
				{ServerMsg{Type: "permission_request", PermTool: "Bash", ToolUseID: "perm1"}, true, true},
				{ServerMsg{Type: "tool_result", ToolUseID: "perm1"}, false, false},
			},
		},
		{
			name: "ask user question does not count as permission",
			steps: []activityStep{
				{ServerMsg{Type: "session_started"}, false, false},
				{ServerMsg{Type: "permission_request", PermTool: "AskUserQuestion", ToolUseID: "perm1"}, false, false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestModel()
			for i, step := range tt.steps {
				m.Apply(step.msg)
				if got := m.HasActivity(); got != step.wantActivity {
					t.Errorf("step %d (%s): HasActivity() = %v, want %v", i, step.msg.Type, got, step.wantActivity)
				}
				if got := m.CanInterrupt(); got != step.wantInterrupt {
					t.Errorf("step %d (%s): CanInterrupt() = %v, want %v", i, step.msg.Type, got, step.wantInterrupt)
				}
			}
		})
	}
}
