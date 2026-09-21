package runner

import "regexp"

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
	for _, p := range UsageLimitPatterns {
		if p.MatchString(transcript) {
			return true
		}
	}
	return false
}
