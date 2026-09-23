package doctor

import "testing"

// Every tool announces itself differently, and a version parser that trips on
// one of them reports a dependency as current forever.
func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"git version 2.53.0", "2.53.0", true},
		{"gh version 2.98.0 (2026-08-20)", "2.98.0", true},
		{"claudeflow 0.2.2 (efa4249, built 2026-09-22T17:12:20Z)", "0.2.2", true},
		{"Docker Compose version v5.5.1", "5.5.1", true},
		{"podman version 5.7.0", "5.7.0", true},
		{"v0.3.0", "0.3.0", true},
		{"2.1.280 (Claude Code)", "2.1.280", true},
		// A bare date must not read as a version.
		{"built 2026-09-22", "", false},
		{"no numbers here", "", false},
	}
	for _, c := range cases {
		got, ok := ParseVersion(c.in)
		if ok != c.ok || (ok && got.String() != c.want) {
			t.Errorf("ParseVersion(%q) = %v, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.2.1", "0.2.2", -1},
		{"0.3.0", "0.2.9", 1},
		{"2.1", "2.1.0", 0},
		{"2.1.0", "2.1", 0},
		{"10.0.0", "9.9.9", 1},
	}
	for _, c := range cases {
		a, _ := ParseVersion(c.a)
		b, _ := ParseVersion(c.b)
		if got := Compare(a, b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestComposeUpstream(t *testing.T) {
	if got := composeUpstream("Docker Compose version v5.5.1"); got != upstreamDocker {
		t.Errorf("docker compose -> %q", got)
	}
	if got := composeUpstream("podman version 5.7.0"); got != upstreamPodman {
		t.Errorf("podman -> %q", got)
	}
	if got := composeUpstream("some other thing 1.0"); got != "" {
		t.Errorf("unknown implementation -> %q, want no upstream to guess at", got)
	}
}
