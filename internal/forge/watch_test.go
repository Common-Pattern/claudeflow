package forge

import "testing"

func TestParseHead(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		status int
		etag   string
	}{
		{"ok", "HTTP/2.0 200 OK\r\nEtag: W/\"abc\"\r\nX-Ratelimit-Remaining: 4984\r\n\r\n[{\"etag\": \"not this\"}]", 200, `W/"abc"`},
		{"not modified", "HTTP/2.0 304 Not Modified\nCache-Control: private\nETag: \"abc\"\n\n", 304, `"abc"`},
		{"no etag", "HTTP/1.1 200 OK\n\n[]", 200, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, etag, err := parseHead([]byte(c.out))
			if err != nil {
				t.Fatal(err)
			}
			if status != c.status || etag != c.etag {
				t.Errorf("parseHead = %d %q, want %d %q", status, etag, c.status, c.etag)
			}
		})
	}
}

// An error gh prints before any response — no network, not logged in — has no
// status line, and must be an error rather than a reading of "unchanged".
func TestParseHeadRejectsNonResponse(t *testing.T) {
	for _, out := range []string{"", "error connecting to api.github.com\n"} {
		if _, _, err := parseHead([]byte(out)); err == nil {
			t.Errorf("parseHead(%q) succeeded, want an error", out)
		}
	}
}
