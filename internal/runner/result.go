package runner

import (
	"encoding/json"
	"strings"
)

// EndedCleanly reports whether a transcript closes with the agent CLI's own
// successful result: the agent ended its turn, rather than being killed at its
// wall clock, crashing, or erroring out of the API.
//
// A run killed from outside writes no result event at all, so the absence of
// one is the answer for those. Only the last result counts: a resumed session
// writes one per turn.
func EndedCleanly(transcript string) bool {
	clean := false
	for _, line := range strings.Split(transcript, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type != "result" {
			continue
		}
		clean = ev.Subtype == "success" && !ev.IsError
	}
	return clean
}
