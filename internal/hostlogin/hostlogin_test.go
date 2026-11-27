package hostlogin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const hostAccount = `{"oauthAccount":{"accountUuid":"acc-1","emailAddress":"op@example.com"},
"userID":"user-1","machineID":"machine-1","hasCompletedOnboarding":true,
"mcpServers":{"private":{"command":"x"}},"projects":{"/home/op/secret":{}},"remoteControlMachineId":"rc-1"}`

func setup(t *testing.T) (from, to string) {
	t.Helper()
	home := t.TempDir()
	from = filepath.Join(home, AccountFile)
	to = filepath.Join(home, ".claude", AccountFile)
	if err := os.WriteFile(from, []byte(hostAccount), 0o600); err != nil {
		t.Fatal(err)
	}
	return from, to
}

func read(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Runs report usage as the operator on this machine, and get nothing else of
// the host's global config.
func TestShareAccountGivesRunsOnlyTheHostsIdentity(t *testing.T) {
	from, to := setup(t)
	if err := ShareAccount(from, to); err != nil {
		t.Fatal(err)
	}
	got := read(t, to)
	if got["userID"] != "user-1" || got["machineID"] != "machine-1" || got["hasCompletedOnboarding"] != true {
		t.Errorf("shared = %v, want the host's identifiers", got)
	}
	if acc, _ := got["oauthAccount"].(map[string]any); acc["emailAddress"] != "op@example.com" {
		t.Errorf("shared oauthAccount = %v", got["oauthAccount"])
	}
	for _, k := range []string{"mcpServers", "projects", "remoteControlMachineId"} {
		if _, ok := got[k]; ok {
			t.Errorf("shared %q, which is the host's alone", k)
		}
	}
	if info, err := os.Stat(to); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

// Running agents write their own state into the shared file; sharing the
// account again must not wipe it.
func TestShareAccountKeepsWhatRunsWrote(t *testing.T) {
	from, to := setup(t)
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, []byte(`{"numStartups":3,"userID":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ShareAccount(from, to); err != nil {
		t.Fatal(err)
	}
	if got := read(t, to); got["numStartups"] != 3.0 || got["userID"] != "user-1" {
		t.Fatalf("merged = %v, want the runs' numStartups and the host's userID", got)
	}
}

func TestShareAccountWaitsForTheCLIsLock(t *testing.T) {
	from, to := setup(t)
	if err := os.MkdirAll(to+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ShareAccount(from, to) }()
	select {
	case err := <-done:
		t.Fatalf("wrote while the lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.Remove(to + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(to + ".lock"); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

// A lock whose holder died stops being refreshed; it must not block forever.
func TestShareAccountBreaksAStaleLock(t *testing.T) {
	from, to := setup(t)
	if err := os.MkdirAll(to+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(to+".lock", old, old); err != nil {
		t.Fatal(err)
	}
	if err := ShareAccount(from, to); err != nil {
		t.Fatal(err)
	}
}

func TestShareAccountWithNothingToShare(t *testing.T) {
	from, to := setup(t)
	if err := ShareAccount(from, from); err != nil {
		t.Fatalf("same file: %v", err)
	}
	if got := read(t, from); got["mcpServers"] == nil {
		t.Fatalf("rewrote the host's own file: %v", got)
	}
	if err := ShareAccount(filepath.Join(t.TempDir(), AccountFile), to); err != nil {
		t.Fatalf("no host config: %v", err)
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Errorf("created %s from nothing", to)
	}
}

func TestParse(t *testing.T) {
	if _, err := Parse([]byte(`{"mcpOAuth":{}}`)); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("Parse = %v, want ErrNoLogin", err)
	}
	ends := time.UnixMilli(time.Now().Add(48 * time.Hour).UnixMilli())
	raw := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-a","expiresAt":1,"refreshTokenExpiresAt":%d}}`, ends.UnixMilli())
	l, err := Parse([]byte(raw))
	if err != nil || !l.LoginExpires.Equal(ends) {
		t.Fatalf("Parse = %+v, %v", l, err)
	}
}
