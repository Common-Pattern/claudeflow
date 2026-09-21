package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GH talks to GitHub through the gh CLI.
type GH struct {
	Repo string
	// Bin is the gh executable. Empty means "gh" on PATH.
	Bin string
}

// NewGH returns a client for a repository in "owner/name" form.
func NewGH(repo string) *GH { return &GH{Repo: repo} }

func (g *GH) bin() string {
	if g.Bin != "" {
		return g.Bin
	}
	return "gh"
}

func (g *GH) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, g.bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("gh %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

type ghIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (gi ghIssue) toIssue() Issue {
	labels := make([]string, 0, len(gi.Labels))
	for _, l := range gi.Labels {
		labels = append(labels, l.Name)
	}
	return Issue{Number: gi.Number, Title: gi.Title, Labels: labels, CreatedAt: gi.CreatedAt}
}

// OpenIssues implements Client.
func (g *GH) OpenIssues(ctx context.Context, withLabels ...string) ([]Issue, error) {
	args := []string{"issue", "list", "--repo", g.Repo, "--state", "open",
		"--limit", "200", "--json", "number,title,labels,createdAt"}
	for _, l := range withLabels {
		args = append(args, "--label", l)
	}
	raw, err := g.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return decodeIssues(raw)
}

// TouchedIssues implements Client.
func (g *GH) TouchedIssues(ctx context.Context, labelPrefix string, limit int) ([]Issue, error) {
	if limit <= 0 {
		limit = 100
	}
	raw, err := g.run(ctx, "issue", "list", "--repo", g.Repo, "--state", "all",
		"--limit", strconv.Itoa(limit), "--json", "number,title,labels,createdAt")
	if err != nil {
		return nil, err
	}
	all, err := decodeIssues(raw)
	if err != nil {
		return nil, err
	}
	var out []Issue
	for _, i := range all {
		for _, l := range i.Labels {
			if strings.HasPrefix(l, labelPrefix) {
				out = append(out, i)
				break
			}
		}
	}
	return out, nil
}

func decodeIssues(raw []byte) ([]Issue, error) {
	var gis []ghIssue
	if err := json.Unmarshal(raw, &gis); err != nil {
		return nil, fmt.Errorf("decode issues: %w", err)
	}
	out := make([]Issue, 0, len(gis))
	for _, gi := range gis {
		out = append(out, gi.toIssue())
	}
	return out, nil
}

// Labels implements Client.
func (g *GH) Labels(ctx context.Context, number int) ([]string, error) {
	raw, err := g.run(ctx, "issue", "view", strconv.Itoa(number), "--repo", g.Repo,
		"--json", "labels", "--jq", "[.labels[].name]")
	if err != nil {
		return nil, err
	}
	var labels []string
	if err := json.Unmarshal(raw, &labels); err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	return labels, nil
}

// EditLabels implements Client.
//
// Labels to remove are intersected with what the issue actually carries first:
// gh fails the whole edit when asked to remove one that is absent, which would
// leave an issue looking re-queued while still carrying a resolution label.
func (g *GH) EditLabels(ctx context.Context, number int, add, remove []string) error {
	if len(remove) > 0 {
		current, err := g.Labels(ctx, number)
		if err != nil {
			return err
		}
		remove = intersect(remove, current)
	}
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	args := []string{"issue", "edit", strconv.Itoa(number), "--repo", g.Repo}
	if len(add) > 0 {
		args = append(args, "--add-label", strings.Join(add, ","))
	}
	if len(remove) > 0 {
		args = append(args, "--remove-label", strings.Join(remove, ","))
	}
	_, err := g.run(ctx, args...)
	return err
}

func intersect(want, have []string) []string {
	set := make(map[string]struct{}, len(have))
	for _, h := range have {
		set[h] = struct{}{}
	}
	var out []string
	for _, w := range want {
		if _, ok := set[w]; ok {
			out = append(out, w)
		}
	}
	return out
}

// Comment implements Client.
func (g *GH) Comment(ctx context.Context, number int, body string) error {
	cmd := exec.CommandContext(ctx, g.bin(), "issue", "comment", strconv.Itoa(number),
		"--repo", g.Repo, "--body-file", "-")
	cmd.Stdin = strings.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh issue comment %d: %s", number, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// LatestCommentBy implements Client.
//
// Two of the three comment streams have a gh subcommand and are read through
// it; only inline review comments have none, and that one call is the reason
// `gh api` appears in this package at all. Subcommands carry gh's own field
// resolution and stay correct across API versions, where a hand-built endpoint
// path does not.
func (g *GH) LatestCommentBy(ctx context.Context, target Target, number int, user string) (time.Time, error) {
	var latest time.Time
	note := func(at time.Time) {
		if at.After(latest) {
			latest = at
		}
	}

	verb := "issue"
	fields := "comments"
	if target == TargetPR {
		verb, fields = "pr", "comments,reviews"
	}
	raw, err := g.run(ctx, verb, "view", strconv.Itoa(number), "--repo", g.Repo, "--json", fields)
	if err != nil {
		return time.Time{}, err
	}
	var view struct {
		Comments []ghViewComment `json:"comments"`
		Reviews  []ghViewComment `json:"reviews"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return time.Time{}, fmt.Errorf("decode %s view: %w", verb, err)
	}
	for _, c := range append(view.Comments, view.Reviews...) {
		if c.Author.Login == user {
			note(c.at())
		}
	}

	if target != TargetPR {
		return latest, nil
	}

	// Inline review comments: no subcommand exposes these, so the raw endpoint
	// is the escape hatch rather than a shortcut.
	rawInline, err := g.run(ctx, "api", "--paginate",
		fmt.Sprintf("repos/%s/pulls/%d/comments", g.Repo, number))
	if err != nil {
		return time.Time{}, err
	}
	at, err := latestBy(rawInline, user)
	if err != nil {
		return time.Time{}, fmt.Errorf("inline review comments: %w", err)
	}
	note(at)
	return latest, nil
}

// ghViewComment is the shape gh's own subcommands return, which differs from
// the REST shape: the author is nested under "author" rather than "user", and
// a review carries submittedAt where a comment carries createdAt.
//
// gh also normalises bot logins, reporting "vercel" where the REST API reports
// "vercel[bot]". That does not affect matching a human's login, but it means
// the two code paths below cannot share a filter.
type ghViewComment struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	CreatedAt   *time.Time `json:"createdAt"`
	SubmittedAt *time.Time `json:"submittedAt"`
}

func (c ghViewComment) at() time.Time {
	if c.CreatedAt != nil && !c.CreatedAt.IsZero() {
		return *c.CreatedAt
	}
	if c.SubmittedAt != nil {
		return *c.SubmittedAt
	}
	return time.Time{}
}

type ghComment struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt *time.Time `json:"created_at"`
	// A review carries submitted_at and a null created_at, so reading only
	// created_at silently misses every review the user left.
	SubmittedAt *time.Time `json:"submitted_at"`
}

func (c ghComment) at() time.Time {
	if c.CreatedAt != nil && !c.CreatedAt.IsZero() {
		return *c.CreatedAt
	}
	if c.SubmittedAt != nil {
		return *c.SubmittedAt
	}
	return time.Time{}
}

func latestBy(raw []byte, user string) (time.Time, error) {
	// --paginate concatenates one JSON array per page.
	dec := json.NewDecoder(bytes.NewReader(raw))
	var latest time.Time
	for {
		var page []ghComment
		if err := dec.Decode(&page); err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			return time.Time{}, fmt.Errorf("decode comments: %w", err)
		}
		for _, c := range page {
			if c.User.Login != user {
				continue
			}
			if at := c.at(); at.After(latest) {
				latest = at
			}
		}
	}
	return latest, nil
}

// StandingPR implements Client.
func (g *GH) StandingPR(ctx context.Context, base, head string) (int, error) {
	raw, err := g.run(ctx, "pr", "list", "--repo", g.Repo, "--state", "open",
		"--base", base, "--head", head, "--json", "number", "--jq", "[.[].number]")
	if err != nil {
		return 0, err
	}
	var nums []int
	if err := json.Unmarshal(raw, &nums); err != nil {
		return 0, fmt.Errorf("decode standing pr: %w", err)
	}
	switch len(nums) {
	case 0:
		return 0, ErrNoStandingPR
	case 1:
		return nums[0], nil
	default:
		return 0, fmt.Errorf("%w: %v", ErrManyStandingPRs, nums)
	}
}

// EnsureLabel implements Client.
func (g *GH) EnsureLabel(ctx context.Context, name, color, description string) error {
	_, err := g.run(ctx, "label", "create", name, "--repo", g.Repo,
		"--color", color, "--description", description, "--force")
	return err
}

type ghCheck struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Bucket string `json:"bucket"`
}

// Checks implements Client.
func (g *GH) Checks(ctx context.Context, number int) ([]Check, error) {
	raw, err := g.run(ctx, "pr", "checks", strconv.Itoa(number), "--repo", g.Repo,
		"--json", "name,state,bucket")
	if err != nil {
		// gh exits non-zero when checks are failing or still running, which is
		// information rather than an error. Fall through on parseable output.
		if len(raw) == 0 {
			return nil, err
		}
	}
	var gcs []ghCheck
	if err := json.Unmarshal(raw, &gcs); err != nil {
		return nil, fmt.Errorf("decode checks: %w", err)
	}
	out := make([]Check, 0, len(gcs))
	for _, c := range gcs {
		done := c.Bucket != "pending"
		out = append(out, Check{Name: c.Name, Passed: c.Bucket == "pass" || c.Bucket == "skipping", Done: done})
	}
	return out, nil
}

// PRHeadSHA implements Client.
func (g *GH) PRHeadSHA(ctx context.Context, number int) (string, error) {
	raw, err := g.run(ctx, "pr", "view", strconv.Itoa(number), "--repo", g.Repo,
		"--json", "headRefOid", "--jq", ".headRefOid")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// Merge implements Client.
//
// --merge only: squash and rebase availability is a repository setting, and a
// merge commit is the one mode that is always allowed where it is enabled.
// --match-head-commit makes the tool answer "did the checks run on what I am
// merging" instead of the caller answering it by eye.
//
// --delete-branch is deliberately absent. It fails when the branch is checked
// out in a worktree, and it fails *after* the merge has already succeeded on
// the server, which reads like the merge itself failed.
func (g *GH) Merge(ctx context.Context, number int, wantSHA string) error {
	args := []string{"pr", "merge", strconv.Itoa(number), "--repo", g.Repo, "--merge"}
	if wantSHA != "" {
		args = append(args, "--match-head-commit", wantSHA)
	}
	_, err := g.run(ctx, args...)
	return err
}

// CreatePR implements Client.
func (g *GH) CreatePR(ctx context.Context, base, head, title, body string) (int, error) {
	cmd := exec.CommandContext(ctx, g.bin(), "pr", "create", "--repo", g.Repo,
		"--base", base, "--head", head, "--title", title, "--body-file", "-")
	cmd.Stdin = strings.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("gh pr create: %s", strings.TrimSpace(stderr.String()))
	}
	return prNumberFromURL(strings.TrimSpace(stdout.String()))
}

func prNumberFromURL(url string) (int, error) {
	idx := strings.LastIndex(url, "/")
	if idx < 0 {
		return 0, fmt.Errorf("no pull request number in %q", url)
	}
	n, err := strconv.Atoi(strings.TrimSpace(url[idx+1:]))
	if err != nil {
		return 0, fmt.Errorf("no pull request number in %q", url)
	}
	return n, nil
}

var _ Client = (*GH)(nil)
