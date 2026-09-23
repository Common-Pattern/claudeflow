// Package scaffold writes a project's first configuration.
//
// The file it emits is a commented template rather than a serialised
// config.Default(). Serialising the struct would guarantee every key is real,
// but it would strip the comments — and the comments are the part that is hard
// to reconstruct: which settings are load-bearing, what a value costs, and why
// a default is what it is. A test parses the template strictly instead, so a
// key that does not exist cannot ship.
package scaffold

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Name is the file claudeflow looks for.
const Name = "claudeflow.yaml"

// The values written where a fact could not be detected. They are exported so
// that `doctor` can recognise a configuration nobody has finished filling in,
// and say so at the configuration itself rather than leaving it to surface
// later as "cannot read OWNER/NAME".
const (
	PlaceholderRepo = "OWNER/NAME"
	PlaceholderUser = "YOUR-GITHUB-LOGIN"
)

// Unfilled names the scaffold placeholders a configuration still carries.
func Unfilled(repo, user string) []string {
	var out []string
	if repo == PlaceholderRepo {
		out = append(out, "repo")
	}
	if user == PlaceholderUser {
		out = append(out, "user")
	}
	return out
}

// Runner executes a command and returns its output, so detection can be
// tested without a repository.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// Facts are what can be read off a checkout rather than asked for.
type Facts struct {
	// Repo is owner/name.
	Repo string
	// User is the GitHub login whose comments the agent must not answer.
	User string
	// Base is the branch the project releases from.
	Base string
}

// Detect fills in what the checkout and gh already know.
//
// Nothing here fails: a fact that cannot be read is left empty and written as
// a placeholder, because a config with three blanks to fill in beats no config
// and an error message.
func Detect(ctx context.Context, run Runner, dir string) Facts {
	var f Facts
	if out, err := run(ctx, "gh", "repo", "view", "--json", "nameWithOwner,defaultBranchRef",
		"-q", ".nameWithOwner + \"\\n\" + .defaultBranchRef.name"); err == nil {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) > 0 {
			f.Repo = strings.TrimSpace(lines[0])
		}
		if len(lines) > 1 {
			f.Base = strings.TrimSpace(lines[1])
		}
	}
	if f.Repo == "" {
		// gh needs a remote it recognises; a plain git remote is the fallback.
		if out, err := run(ctx, "git", "-C", dir, "remote", "get-url", "origin"); err == nil {
			f.Repo = repoFromRemote(strings.TrimSpace(out))
		}
	}
	if out, err := run(ctx, "gh", "api", "user", "-q", ".login"); err == nil {
		f.User = strings.TrimSpace(out)
	}
	return f
}

// repoFromRemote reduces a remote URL to owner/name.
func repoFromRemote(url string) string {
	url = strings.TrimSuffix(strings.TrimSpace(url), ".git")
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
		if at := strings.Index(url, "@"); at >= 0 {
			url = url[at+1:]
		}
	}
	// scp-style: git@github.com:owner/name
	if i := strings.Index(url, ":"); i >= 0 && !strings.Contains(url[:i], "/") {
		url = url[i+1:]
	}
	parts := strings.Split(strings.Trim(url, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

// ErrExists means there is already a configuration, which init will not touch.
var ErrExists = errors.New("a configuration already exists")

// Write renders the template into dir, refusing to overwrite.
//
// Refusing matters more than it looks: the file it would replace carries the
// project's own reasoning in its comments, and a regenerated default loses all
// of it while still starting successfully.
func Write(dir string, f Facts) (string, error) {
	path := filepath.Join(dir, Name)
	if _, err := os.Stat(path); err == nil {
		return path, fmt.Errorf("%w: %s", ErrExists, path)
	}
	if err := os.WriteFile(path, Render(f), 0o644); err != nil {
		return path, err
	}
	return path, nil
}

// Missing lists the facts that were written as placeholders.
func (f Facts) Missing() []string {
	var out []string
	if f.Repo == "" {
		out = append(out, "repo")
	}
	if f.User == "" {
		out = append(out, "user")
	}
	return out
}

// Render produces the configuration file.
func Render(f Facts) []byte {
	repo, user, base := f.Repo, f.User, f.Base
	if repo == "" {
		repo = PlaceholderRepo
	}
	if user == "" {
		user = PlaceholderUser
	}
	if base == "" {
		base = "main"
	}
	return []byte(fmt.Sprintf(template, repo, user, base, base))
}

// template is the file itself. Every key in it is checked against the parser
// by a test, so a setting that no longer exists cannot be scaffolded.
const template = `# claudeflow — unattended runs driven by GitHub issue labels.
#
# See github.com/Common-Pattern/claudeflow. Unknown keys are an error, so a
# typo here stops the next run rather than being silently ignored: run
# ` + "`claudeflow doctor`" + ` after editing.

repo: %s
# The account claudeflow posts as. Without it the agent reads its own comments
# as new instructions and answers itself.
user: %s

branches:
  base: %s
  # Where finished work lands. Set this to a separate branch to collect runs
  # behind one standing pull request into %s; leave it out to land directly.
  # integration: preview
  prefix: claude/

limits:
  # How many runs may be in flight. Each build holds a slot, a container stack
  # and a model session, and this box has other work to do.
  maxBuilds: 2
  maxPlanning: 2
  buildTimeout: 150m
  planTimeout: 30m
  rateLimitBackoff: 1h
  fixAttempts: 3

slots:
  min: 1
  max: 4

compose:
  # Required: every project ships a Compose file describing the environment a
  # run gets. Relative to paths.root.
  file: claudeflow/compose.yaml
  # bin: [docker, compose]   # name the implementation where the default is
                             # not the one you want. An absolute path is safest
                             # on a host that runs more than one.
  # env:                     # environment for the compose command itself
  #   DOCKER_HOST: unix:///run/user/1000/podman/podman.sock
  projectPrefix: cf
  # Generous by default: the first boot of a run migrates an empty database
  # from cold.
  readyTimeout: 10m
  # Publish ephemeral ports in the Compose file and name the services here;
  # claudeflow reads back what each one landed on and passes the agent
  # CLAUDEFLOW_PORT_<SERVICE>, CLAUDEFLOW_HOST_<SERVICE> and
  # CLAUDEFLOW_URL_<SERVICE>. Nothing does port arithmetic.
  # expose:
  #   web: 3000
  #   postgres: 5432
  # host: localhost          # where those URLs point, if not localhost

hooks:
  # Run in the worktree before the stack comes up — worktrees do not share a
  # node_modules, and a Compose file that bind-mounts one needs it in place.
  # install: npm ci
  # The project's full check suite, named here so the run instructions can
  # refer to it without knowing the project.
  # verify: npm test

agent:
  command: claude
  # An alias tracks the latest model in that family; a full name pins it.
  model: opus
  # extraArgs: []
  # skillsDir: ./my-skills   # overrides the skills shipped in the binary
  # Extra environment for the run, expanded against the addresses above. This
  # is how a project points its own tooling at the run's containers — a test
  # harness that refuses to guess its database has nothing to be told without
  # it.
  # env:
  #   DATABASE_URL: postgresql://app:app@localhost:${CLAUDEFLOW_PORT_POSTGRES}/app_test

paths:
  root: .
  state: .claudeflow
`
