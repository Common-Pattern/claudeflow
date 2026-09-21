package forge

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Fake is an in-memory Client for tests. It records mutations so a test can
// assert on what claudeflow tried to do, not only on what it returned.
type Fake struct {
	mu sync.Mutex

	Issues   map[int]*Issue
	Comments map[int][]string
	// LastSpoke keys a target and number to when the watched user last posted.
	LastSpoke map[string]time.Time
	Standing  []int
	// StandingErr, when set, is returned by StandingPR instead of Standing.
	StandingErr error
	ChecksBy    map[int][]Check
	HeadSHA     map[int]string
	Created     map[string]string

	// Merged records pull requests merged, in order.
	Merged []int
	// MergeErr, when set, fails every Merge.
	MergeErr error
	// Err, when set, fails every call. Used to assert error propagation.
	Err error
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{
		Issues: map[int]*Issue{}, Comments: map[int][]string{},
		LastSpoke: map[string]time.Time{}, ChecksBy: map[int][]Check{},
		HeadSHA: map[int]string{}, Created: map[string]string{},
	}
}

// AddIssue registers an issue.
func (f *Fake) AddIssue(number int, created time.Time, labels ...string) *Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := &Issue{Number: number, Title: fmt.Sprintf("issue %d", number), Labels: labels, CreatedAt: created}
	f.Issues[number] = i
	return i
}

// SetLastSpoke records when the watched user last posted on a thread.
func (f *Fake) SetLastSpoke(target Target, number int, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LastSpoke[fmt.Sprintf("%s-%d", target, number)] = at
}

// OpenIssues implements Client.
func (f *Fake) OpenIssues(_ context.Context, withLabels ...string) ([]Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	var out []Issue
	for _, i := range f.Issues {
		match := true
		for _, l := range withLabels {
			if !i.HasLabel(l) {
				match = false
				break
			}
		}
		if match {
			out = append(out, *i)
		}
	}
	slices.SortFunc(out, func(a, b Issue) int { return a.Number - b.Number })
	return out, nil
}

// TouchedIssues implements Client.
func (f *Fake) TouchedIssues(_ context.Context, prefix string, _ int) ([]Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	var out []Issue
	for _, i := range f.Issues {
		for _, l := range i.Labels {
			if len(l) >= len(prefix) && l[:len(prefix)] == prefix {
				out = append(out, *i)
				break
			}
		}
	}
	slices.SortFunc(out, func(a, b Issue) int { return a.Number - b.Number })
	return out, nil
}

// Labels implements Client.
func (f *Fake) Labels(_ context.Context, number int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	i, ok := f.Issues[number]
	if !ok {
		return nil, fmt.Errorf("no issue %d", number)
	}
	return slices.Clone(i.Labels), nil
}

// EditLabels implements Client.
func (f *Fake) EditLabels(_ context.Context, number int, add, remove []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	i, ok := f.Issues[number]
	if !ok {
		return fmt.Errorf("no issue %d", number)
	}
	for _, r := range remove {
		i.Labels = slices.DeleteFunc(i.Labels, func(l string) bool { return l == r })
	}
	for _, a := range add {
		if !i.HasLabel(a) {
			i.Labels = append(i.Labels, a)
		}
	}
	return nil
}

// Comment implements Client.
func (f *Fake) Comment(_ context.Context, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.Comments[number] = append(f.Comments[number], body)
	return nil
}

// LatestCommentBy implements Client.
func (f *Fake) LatestCommentBy(_ context.Context, target Target, number int, _ string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return time.Time{}, f.Err
	}
	return f.LastSpoke[fmt.Sprintf("%s-%d", target, number)], nil
}

// StandingPR implements Client.
func (f *Fake) StandingPR(_ context.Context, _, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StandingErr != nil {
		return 0, f.StandingErr
	}
	switch len(f.Standing) {
	case 0:
		return 0, ErrNoStandingPR
	case 1:
		return f.Standing[0], nil
	default:
		return 0, fmt.Errorf("%w: %v", ErrManyStandingPRs, f.Standing)
	}
}

// EnsureLabel implements Client.
func (f *Fake) EnsureLabel(_ context.Context, name, _, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.Created[name] = description
	return nil
}

// Checks implements Client.
func (f *Fake) Checks(_ context.Context, number int) ([]Check, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	return f.ChecksBy[number], nil
}

// PRHeadSHA implements Client.
func (f *Fake) PRHeadSHA(_ context.Context, number int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return "", f.Err
	}
	return f.HeadSHA[number], nil
}

// Merge implements Client.
func (f *Fake) Merge(_ context.Context, number int, wantSHA string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MergeErr != nil {
		return f.MergeErr
	}
	if want, ok := f.HeadSHA[number]; ok && wantSHA != "" && want != wantSHA {
		return fmt.Errorf("head moved: have %s, want %s", want, wantSHA)
	}
	f.Merged = append(f.Merged, number)
	return nil
}

// CreatePR implements Client.
func (f *Fake) CreatePR(_ context.Context, _, _, title, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return 0, f.Err
	}
	n := 1000 + len(f.Issues)
	f.Issues[n] = &Issue{Number: n, Title: title}
	f.Standing = append(f.Standing, n)
	return n, nil
}

var _ Client = (*Fake)(nil)
