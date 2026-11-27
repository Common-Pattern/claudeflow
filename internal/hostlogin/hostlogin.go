// Package hostlogin shares the operator's Claude Code state with sandboxed
// runs.
//
// A sandboxed run mounts the host CLI's config directory, ~/.claude, at the
// same path and points the agent's CLAUDE_CONFIG_DIR at it. That directory
// holds the login and the locks the CLI takes to refresh and rewrite it, so a
// run refreshes the login exactly as another process on the host would, and
// every process sees the refreshed login on its next request. The session
// transcripts the CLI writes there are the host's history as soon as they are
// written.
//
// The one thing outside the directory is the CLI's global config, which the
// host keeps in ~/.claude.json, next to a lock in the home directory that a
// container cannot share. A CLI with CLAUDE_CONFIG_DIR set keeps its global
// config inside the directory instead, so sandboxed runs share one of their
// own there, and ShareAccount gives it the host's account and identifiers so
// runs report as the host does.
package hostlogin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// File is the name of the CLI's credentials file in its config directory.
const File = ".credentials.json"

// AccountFile is the CLI's global config: in the config directory when
// CLAUDE_CONFIG_DIR is set, in the home directory when it is not.
const AccountFile = ".claude.json"

// ConfigDir is the host CLI's config directory: $CLAUDE_CONFIG_DIR, else
// ~/.claude.
func ConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// Path is the host's credentials file.
func Path() string { return filepath.Join(ConfigDir(), File) }

// AccountPath is the host CLI's global config.
func AccountPath() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, AccountFile)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, AccountFile)
}

// SharedAccountPath is the global config sandboxed runs share: the one a CLI
// with CLAUDE_CONFIG_DIR set to the host's config directory reads.
func SharedAccountPath() string { return filepath.Join(ConfigDir(), AccountFile) }

// Credentials are the variables that give the agent CLI a credential of its
// own, which it uses in preference to the shared login.
var Credentials = []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// Passed is the first of Credentials among names that is set, or "".
func Passed(names []string) string {
	for _, name := range names {
		if slices.Contains(Credentials, name) && os.Getenv(name) != "" {
			return name
		}
	}
	return ""
}

// Login is what claudeflow needs to know about a stored login.
type Login struct {
	// Expires is when the access token stops working.
	Expires time.Time
	// LoginExpires is when the login itself ends and has to be renewed in a
	// browser; zero when the file does not say.
	LoginExpires time.Time
}

// ErrNoLogin means the file holds no claude.ai login.
var ErrNoLogin = errors.New("no claude.ai login in the credentials file")

// Parse reads a credentials file's contents.
func Parse(raw []byte) (Login, error) {
	var s struct {
		OAuth map[string]json.RawMessage `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Login{}, fmt.Errorf("parse credentials: %w", err)
	}
	var token string
	if v, ok := s.OAuth["accessToken"]; !ok || json.Unmarshal(v, &token) != nil || token == "" {
		return Login{}, ErrNoLogin
	}
	return Login{Expires: millis(s.OAuth["expiresAt"]), LoginExpires: millis(s.OAuth["refreshTokenExpiresAt"])}, nil
}

func millis(v json.RawMessage) time.Time {
	var ms int64
	if v == nil || json.Unmarshal(v, &ms) != nil || ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// accountKeys are what sandboxed runs are given of the host's global config:
// who the login belongs to, and the identifiers the CLI reports usage under.
// With them a run reports as the operator on this machine; without them it
// reports as a new, anonymous install.
//
// The rest of the host's global config — its MCP servers, per-project state,
// Remote Control identity and caches — stays the host's.
var accountKeys = []string{"oauthAccount", "userID", "machineID", "hasCompletedOnboarding"}

// ShareAccount writes the host's account and identifiers from the global
// config at from into the one at to, keeping everything else the runs' CLIs
// have written there. It holds the lock the CLI takes on that file, so it
// cannot lose a write a running agent makes at the same moment.
//
// It does nothing when from and to are the same file — a host with
// CLAUDE_CONFIG_DIR set already shares its global config — or when the host
// has none.
func ShareAccount(from, to string) error {
	if filepath.Clean(from) == filepath.Clean(to) {
		return nil
	}
	raw, err := os.ReadFile(from)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var host map[string]json.RawMessage
	if err := json.Unmarshal(raw, &host); err != nil {
		return fmt.Errorf("parse %s: %w", from, err)
	}

	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	release, err := lock(to)
	if err != nil {
		return err
	}
	defer release()

	cur := map[string]json.RawMessage{}
	raw, err = os.ReadFile(to)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &cur); err != nil {
			return fmt.Errorf("parse %s: %w", to, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	changed := false
	for _, k := range accountKeys {
		if v, ok := host[k]; ok && string(cur[k]) != string(v) {
			cur[k] = v
			changed = true
		}
	}
	if !changed {
		return nil
	}
	merged, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	return replace(to, merged)
}

// lock takes the lock the CLI takes before rewriting path: a directory beside
// it named path.lock, which a holder refreshes while it holds it. One left
// behind by a holder that died is stale once it stops being refreshed.
func lock(path string) (release func(), err error) {
	dir := path + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			return func() { _ = os.Remove(dir) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if info, serr := os.Stat(dir); serr == nil && time.Since(info.ModTime()) > 30*time.Second {
			_ = os.Remove(dir)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is held by another process", dir)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// replace writes path in one rename, so a CLI reading it never sees half.
func replace(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".claudeflow-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
