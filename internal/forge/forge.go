// Package forge is claudeflow's view of GitHub.
//
// Access goes through the `gh` CLI rather than the REST API directly. That is
// deliberate: gh already holds the operator's credentials, refreshes them, and
// resolves hosts, so claudeflow needs no token of its own and works
// identically against github.com and an enterprise host.
package forge

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Target distinguishes the two comment surfaces. A pull request carries three
// separate comment streams and review comments are the ones most often missed.
type Target string

const (
	// TargetIssue is an issue's comment thread.
	TargetIssue Target = "issue"
	// TargetPR is a pull request: issue comments, review comments and reviews.
	TargetPR Target = "pr"
)

// Issue is the subset of an issue claudeflow reasons about.
type Issue struct {
	Number    int
	Title     string
	Labels    []string
	CreatedAt time.Time
}

// HasLabel reports whether the issue carries name.
func (i Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if l == name {
			return true
		}
	}
	return false
}

// HasAnyLabel reports whether the issue carries any of names.
func (i Issue) HasAnyLabel(names ...string) bool {
	for _, n := range names {
		if i.HasLabel(n) {
			return true
		}
	}
	return false
}

// Check is one status check on a commit.
type Check struct {
	Name   string
	Passed bool
	Done   bool
}

// Errors a caller is expected to branch on.
var (
	// ErrNoStandingPR means no open pull request joins the integration branch
	// to the base branch.
	ErrNoStandingPR = errors.New("no standing pull request")
	// ErrManyStandingPRs means more than one does.
	//
	// This is fatal rather than resolved by picking the first. With two, the
	// agent would read review comments from one while landing work gated by
	// the other, and choosing silently is how that stays invisible.
	ErrManyStandingPRs = errors.New("more than one standing pull request")
)

// AgentMarker is appended to every comment claudeflow or its agent writes, and
// any comment carrying it is ignored when deciding whether there is a new
// instruction.
//
// This exists because the agent speaks through the operator's own `gh`
// credentials, so its comments are authored by the configured user and an
// author filter cannot tell them apart. Without the marker the system answers
// its own question, reads that answer as a fresh instruction, and runs again —
// a loop that spends plan capacity until someone notices.
//
// An HTML comment is invisible in rendered Markdown, so it costs the reader
// nothing.
const AgentMarker = "<!-- claudeflow:agent -->"

// SignComment appends the marker if it is not already present.
func SignComment(body string) string {
	if strings.Contains(body, AgentMarker) {
		return body
	}
	return strings.TrimRight(body, "\n") + "\n\n" + AgentMarker + "\n"
}

// IsAgentComment reports whether a body was written by claudeflow or its agent.
func IsAgentComment(body string) bool { return strings.Contains(body, AgentMarker) }

// closingRef matches GitHub's issue-closing keywords.
var closingRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

// ClosingRefs returns the issue numbers a body asks GitHub to close.
func ClosingRefs(body string) []int {
	var out []int
	seen := map[int]struct{}{}
	for _, m := range closingRef.FindAllStringSubmatch(body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// ClosesMarker heads the block of closing references claudeflow maintains on
// the standing pull request.
const ClosesMarker = "<!-- claudeflow:closes -->"

// WithClosingRefs returns body with refs added to its closing block.
//
// The block lives on the STANDING pull request, not on the one that merged the
// work. GitHub only closes an issue when a pull request merges into the
// repository's default branch, so `Closes #N` on a branch that merges into an
// integration branch closes nothing — the issue stays open after the work has
// shipped. Carrying the references forward onto the integration pull request is
// what makes them close when the operator finally merges it.
//
// Existing references are preserved, so several landings accumulate.
func WithClosingRefs(body string, refs []int) string {
	all := append(ClosingRefs(body), refs...)
	sort.Ints(all)

	seen := map[int]struct{}{}
	var lines []string
	for _, n := range all {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		lines = append(lines, fmt.Sprintf("Fixes #%d", n))
	}
	if len(lines) == 0 {
		return body
	}

	block := ClosesMarker + "\n" + strings.Join(lines, "\n")
	if i := strings.Index(body, ClosesMarker); i >= 0 {
		// Replace the managed block, leaving anything after it alone.
		rest := body[i+len(ClosesMarker):]
		if j := strings.Index(rest, "\n\n"); j >= 0 {
			return body[:i] + block + rest[j:]
		}
		return body[:i] + block + "\n"
	}
	return strings.TrimRight(body, "\n") + "\n\n" + block + "\n"
}

// Client is everything claudeflow needs from the forge. It exists so the
// orchestration can be tested without a network.
type Client interface {
	// PRBody returns a pull request's body.
	PRBody(ctx context.Context, number int) (string, error)
	// UpdatePRBody replaces it.
	UpdatePRBody(ctx context.Context, number int, body string) error
	// OpenIssues returns open issues carrying all of the given labels.
	OpenIssues(ctx context.Context, withLabels ...string) ([]Issue, error)
	// TouchedIssues returns issues in any state carrying at least one label
	// with the given prefix, so replies on closed issues are still seen.
	TouchedIssues(ctx context.Context, labelPrefix string, limit int) ([]Issue, error)
	// Labels returns one issue's current labels.
	Labels(ctx context.Context, number int) ([]string, error)
	// EditLabels adds and removes labels in one call. Removing a label the
	// issue does not carry must not fail the call.
	EditLabels(ctx context.Context, number int, add, remove []string) error
	// Comment posts a comment on an issue or pull request.
	Comment(ctx context.Context, number int, body string) error
	// LatestCommentBy returns when user last gave an instruction on a thread,
	// across every comment stream the target has. Comments carrying
	// AgentMarker are skipped, because the agent posts as the same account. A
	// thread with no instruction on it returns the zero time and no error.
	LatestCommentBy(ctx context.Context, target Target, number int, user string) (time.Time, error)
	// StandingPR returns the single open pull request from head into base.
	StandingPR(ctx context.Context, base, head string) (int, error)
	// EnsureLabel creates or updates a label definition.
	EnsureLabel(ctx context.Context, name, color, description string) error
	// Checks returns the status checks on a pull request.
	Checks(ctx context.Context, number int) ([]Check, error)
	// PRHeadSHA returns the commit a pull request currently points at.
	PRHeadSHA(ctx context.Context, number int) (string, error)
	// Merge merges a pull request, refusing if its head has moved away from
	// wantSHA.
	Merge(ctx context.Context, number int, wantSHA string) error
	// CreatePR opens a pull request and returns its number.
	CreatePR(ctx context.Context, base, head, title, body string) (int, error)
}

// AllPassed reports whether every required check is present and green.
// A required check that is absent counts as not passed: an absent check and a
// failed one are the same answer to "may this merge".
func AllPassed(checks []Check, required ...string) bool {
	byName := make(map[string]Check, len(checks))
	for _, c := range checks {
		byName[c.Name] = c
	}
	for _, name := range required {
		c, ok := byName[name]
		if !ok || !c.Done || !c.Passed {
			return false
		}
	}
	return true
}

// Pending reports the checks that have not finished.
func Pending(checks []Check) []string {
	var out []string
	for _, c := range checks {
		if !c.Done {
			out = append(out, c.Name)
		}
	}
	return out
}

// Failed reports the checks that finished without passing.
func Failed(checks []Check) []string {
	var out []string
	for _, c := range checks {
		if c.Done && !c.Passed {
			out = append(out, c.Name)
		}
	}
	return out
}
