package monetdroid

import (
	"encoding/json"

	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// buildAskUserResponse creates the updatedInput by merging answers into the
// raw JSON, preserving all original fields for the CLI round-trip.
func buildAskUserResponse(input *protocol.ToolInput, answers map[string]string) *protocol.ToolInput {
	// Merge "answers" into Raw so MarshalJSON includes it
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input.Raw, &m); err != nil {
		m = make(map[string]json.RawMessage)
	}
	answersJSON, _ := json.Marshal(answers)
	m["answers"] = json.RawMessage(answersJSON)
	merged, _ := json.Marshal(m)

	ask := &protocol.AskInput{Answers: answers}
	if input.Ask != nil {
		ask.Questions = input.Ask.Questions
	}
	return &protocol.ToolInput{Raw: merged, Ask: ask}
}
