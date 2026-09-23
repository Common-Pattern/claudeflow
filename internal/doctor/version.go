package doctor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Latest reports the newest released version of a project, as a tag.
//
// It is a function on Options so the checks can be tested without a network,
// and so a host that cannot reach GitHub degrades to "installed X" rather than
// failing a check over something that is not broken.
type Latest func(ctx context.Context, repo string) (string, error)

// Upstreams are the repositories each dependency releases from.
//
// A dependency is listed here only if its releases are the authority on what
// "current" means, and only if it actually publishes releases — git.git has
// tags and no releases, and a host's git comes from its distribution anyway, so
// git is reported without being judged.
const (
	upstreamSelf   = "Common-Pattern/claudeflow"
	upstreamGH     = "cli/cli"
	upstreamDocker = "docker/compose"
	upstreamPodman = "containers/podman"
)

// composeUpstream picks the project a compose implementation releases from,
// by what it calls itself.
func composeUpstream(version string) string {
	switch {
	case strings.Contains(strings.ToLower(version), "podman"):
		return upstreamPodman
	case strings.Contains(strings.ToLower(version), "docker"):
		return upstreamDocker
	}
	return ""
}

// GHLatest asks the GitHub CLI for a repository's latest release.
//
// gh is already a hard dependency and already carries the credentials, so this
// adds no new way for the doctor to fail.
func GHLatest(run Runner) Latest {
	return func(ctx context.Context, repo string) (string, error) {
		out, err := run(ctx, "gh", "release", "view", "--repo", repo, "--json", "tagName", "-q", ".tagName")
		if err != nil {
			return "", fmt.Errorf("ask %s for its latest release: %w", repo, err)
		}
		return strings.TrimSpace(out), nil
	}
}

// Version is a parsed dotted version.
type Version []int

// ParseVersion pulls the first dotted number out of a string.
//
// Every tool here announces itself differently — "git version 2.51.0", "gh
// version 2.83.0 (2026-09-04)", "claudeflow 0.2.2 (abc, built ...)", a bare
// "v5.5.1" — and all that matters is the first x.y.z in the line.
func ParseVersion(s string) (Version, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			continue
		}
		// Only accept a digit that starts a number, so the "2" of "2026-09-04"
		// inside a date is not read as a version on its own.
		if i > 0 && (s[i-1] == '.' || s[i-1] == '-' || (s[i-1] >= '0' && s[i-1] <= '9')) {
			continue
		}
		v, end := scanVersion(s[i:])
		if len(v) >= 2 {
			return v, true
		}
		if end > 1 {
			i += end - 1
		}
	}
	return nil, false
}

// scanVersion reads a dotted number from the front of s.
func scanVersion(s string) (Version, int) {
	var v Version
	i := 0
	for {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			break
		}
		n, err := strconv.Atoi(s[start:i])
		if err != nil {
			break
		}
		v = append(v, n)
		if i < len(s) && s[i] == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
			i++
			continue
		}
		break
	}
	return v, i
}

// Compare orders two versions, shorter ones padded with zeroes so 2.1 and
// 2.1.0 are the same version.
func Compare(a, b Version) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// String renders a version the way it is usually written.
func (v Version) String() string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// judge compares what is installed against the latest release and folds the
// answer into a result.
//
// Being behind is a warning, never a failure: an old tool works until it does
// not, and a doctor that fails the host over a point release is a doctor people
// stop running. A lookup that does not answer — no network, not logged in, a
// repository with no releases — leaves the result exactly as it was, with a
// note. Not knowing is not a finding.
func judge(ctx context.Context, o Options, r Result, installed, repo string) Result {
	if o.Latest == nil || installed == "" {
		return r
	}
	have, ok := ParseVersion(installed)
	if !ok {
		return r
	}
	raw, err := o.Latest(ctx, repo)
	if err != nil {
		r.Detail += " — could not check for a newer release"
		return r
	}
	want, ok := ParseVersion(raw)
	if !ok {
		return r
	}
	switch Compare(have, want) {
	case -1:
		if r.Status == OK {
			r.Status = Warn
		}
		r.Detail += fmt.Sprintf(" — %s is out, this is %s", want, have)
		if r.Fix == "" {
			r.Fix = "upgrade it: " + upgradeHint(repo)
		}
	default:
		r.Detail += " — current"
	}
	return r
}

// note reports how the installed version compares without judging it.
//
// git and the compose implementation are installed by a package manager on
// most hosts, and a distribution ships what it has tested. Being a release
// behind git.git is normal and not a finding, so this says what is current and
// leaves the status alone.
func note(ctx context.Context, o Options, r Result, installed, repo string) Result {
	if o.Latest == nil || repo == "" || installed == "" {
		return r
	}
	have, ok := ParseVersion(installed)
	if !ok {
		return r
	}
	raw, err := o.Latest(ctx, repo)
	if err != nil {
		return r
	}
	want, ok := ParseVersion(raw)
	if !ok {
		return r
	}
	if Compare(have, want) < 0 {
		r.Detail += fmt.Sprintf(" — %s is out", want)
	} else {
		r.Detail += " — current"
	}
	return r
}

// upgradeHint names how each dependency is upgraded, because "you are behind"
// without "here is how" is half a report.
func upgradeHint(repo string) string {
	switch repo {
	case upstreamSelf:
		return "https://github.com/" + upstreamSelf + "/releases/latest"
	case upstreamGH:
		return "see https://github.com/cli/cli#installation, or your package manager"
	}
	return "https://github.com/" + repo + "/releases/latest"
}
