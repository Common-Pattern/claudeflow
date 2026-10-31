package runner

import (
	"encoding/json"
	"regexp"
	"strings"
)

// UsageLimitPatterns are the transcript phrases taken to mean the agent CLI
// refused on plan or usage limits. A caller with a CLI that words it
// differently can append to this slice before starting any run.
var UsageLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)usage limit`),
	regexp.MustCompile(`(?i)rate limit (reached|exceeded)`),
	regexp.MustCompile(`(?i)rate[_-]limit[_-]error`),
	regexp.MustCompile(`(?i)too many requests`),
	regexp.MustCompile(`(?i)quota exceeded`),
	regexp.MustCompile(`(?i)exceeded your (current )?quota`),
	regexp.MustCompile(`(?i)limit will reset at`),
}

// HitUsageLimit reports whether a transcript shows the agent CLI refusing on
// plan or usage limits rather than failing on the work. The two need different
// answers: a refusal is retried later untouched, a failure is the run's result.
//
// This is a heuristic over one CLI's human-readable output, matched against
// UsageLimitPatterns. That output is not a contract and carries no status
// code, so the phrasing changes without warning — when a limit stops being
// detected, the patterns are what to revisit, and a run that matches nothing
// is reported as a plain failure rather than retried.
func HitUsageLimit(transcript string) bool {
	said := cliOutput(transcript)
	for _, p := range UsageLimitPatterns {
		if p.MatchString(said) {
			return true
		}
	}
	return false
}

// cliOutput reduces a transcript to what the CLI itself said.
//
// A transcript is an event stream holding every file the agent read and every
// command it ran, so matching all of it would call a run limited for reading a
// test log that mentions one, or this file. What the CLI says on its own
// account is its closing result, a rate limit event that was refused, and any
// line that is not an event at all, which is where stderr lands.
func cliOutput(transcript string) string {
	var said strings.Builder
	for _, line := range strings.Split(transcript, "\n") {
		var ev struct {
			Type          string `json:"type"`
			Result        string `json:"result"`
			RateLimitInfo struct {
				Status string `json:"status"`
			} `json:"rate_limit_info"`
		}
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &ev) != nil || ev.Type == "" {
			said.WriteString(line + "\n")
			continue
		}
		switch {
		case ev.Type == "result":
			said.WriteString(ev.Result + "\n")
		case ev.Type == "rate_limit_event" && ev.RateLimitInfo.Status == "rejected":
			said.WriteString("usage limit\n")
		}
	}
	return said.String()
}
