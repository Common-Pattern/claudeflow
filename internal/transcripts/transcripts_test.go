package transcripts

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serverOver returns a server over a directory holding one transcript.
func serverOver(t *testing.T) Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "build-252-20260922T165737.log"), []byte("I opened PR #253.\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return Server{
		Dir:   dir,
		Repo:  "acme/widgets",
		Allow: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")},
	}
}

// get issues a request as if it came from remote.
func get(t *testing.T, s Server, remote, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestServesATranscriptToAPermittedNetwork(t *testing.T) {
	s := serverOver(t)
	w := get(t, s, "100.71.59.106:51000", "/build-252-20260922T165737.log")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "I opened PR #253.") {
		t.Errorf("body = %q, want the transcript", w.Body.String())
	}
	// A transcript that opened with markup must not be served as a document to
	// render, and the browser must not second-guess that.
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

// The allow list is the whole access control, so the case that matters is the
// one where the caller can reach the port and is still refused.
func TestRefusesAnAddressOutsideTheAllowList(t *testing.T) {
	s := serverOver(t)
	for _, remote := range []string{"192.168.1.20:51000", "127.0.0.1:51000", "[2001:db8::1]:51000"} {
		w := get(t, s, remote, "/build-252-20260922T165737.log")
		if w.Code != http.StatusNotFound {
			t.Errorf("status for %s = %d, want 404", remote, w.Code)
		}
		if strings.Contains(w.Body.String(), "PR #253") {
			t.Errorf("%s was served the transcript anyway", remote)
		}
	}
}

// X-Forwarded-For is a claim by the caller; this server sits behind no proxy
// that could make it a fact.
func TestForwardedForCannotGrantAccess(t *testing.T) {
	s := serverOver(t)
	r := httptest.NewRequest(http.MethodGet, "/build-252-20260922T165737.log", nil)
	r.RemoteAddr = "192.168.1.20:51000"
	r.Header.Set("X-Forwarded-For", "100.71.59.106")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — a header claimed its way in", w.Code)
	}
}

func TestRefusesAPathOutsideTheLogDirectory(t *testing.T) {
	s := serverOver(t)
	secret := filepath.Join(filepath.Dir(s.Dir), "secret.log")
	if err := os.WriteFile(secret, []byte("not yours"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, path := range []string{
		"/../secret.log",
		"/%2e%2e/secret.log",
		"/..%2fsecret.log",
		"/subdir/build.log",
		"/" + secret,
	} {
		w := get(t, s, "100.71.59.106:51000", path)
		if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "not yours") {
			t.Errorf("%s escaped the log directory", path)
		}
	}
}

// Only transcripts are served. The state directory next door holds run records
// and flags, and nothing should be able to ask for one by name.
func TestServesOnlyLogFiles(t *testing.T) {
	s := serverOver(t)
	if err := os.WriteFile(filepath.Join(s.Dir, "build-1.json"), []byte(`{"pid":1}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if w := get(t, s, "100.71.59.106:51000", "/build-1.json"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a non-transcript", w.Code)
	}
	if w := get(t, s, "100.71.59.106:51000", "/"); strings.Contains(w.Body.String(), "build-1.json") {
		t.Error("the listing offered a file that is not a transcript")
	}
}

func TestIndexListsTranscripts(t *testing.T) {
	s := serverOver(t)
	w := get(t, s, "100.71.59.106:51000", "/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "build-252-20260922T165737.log") {
		t.Errorf("listing does not name the transcript:\n%s", body)
	}
	if !strings.Contains(body, "acme/widgets") {
		t.Error("listing does not say which project it belongs to")
	}
}

func TestReadOnly(t *testing.T) {
	s := serverOver(t)
	r := httptest.NewRequest(http.MethodDelete, "/build-252-20260922T165737.log", nil)
	r.RemoteAddr = "100.71.59.106:51000"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", w.Code)
	}
}

func TestSafeName(t *testing.T) {
	ok := []string{"build-252-20260922T165737.log", "fix-253-20260101T000000.log"}
	bad := []string{"", ".", "..", "../x.log", "a/b.log", `a\b.log`, ".hidden.log", "run.json", "build-252.log.gz"}
	for _, n := range ok {
		if !safeName(n) {
			t.Errorf("safeName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if safeName(n) {
			t.Errorf("safeName(%q) = true, want false", n)
		}
	}
}

// An IPv6 peer arrives with a zone on a link-local address, and a prefix never
// matches one that still carries it.
func TestPeerDropsTheZone(t *testing.T) {
	addr, err := peer("[fe80::1%eth0]:51000")
	if err != nil {
		t.Fatalf("peer: %v", err)
	}
	if addr.Zone() != "" {
		t.Errorf("zone = %q, want it dropped", addr.Zone())
	}
}

// A v4 address arriving over a dual-stack listener is ::ffff:100.71.59.106,
// which no IPv4 prefix contains until it is unmapped.
func TestPeerUnmapsIPv4(t *testing.T) {
	addr, err := peer("[::ffff:100.71.59.106]:51000")
	if err != nil {
		t.Fatalf("peer: %v", err)
	}
	if !netip.MustParsePrefix("100.64.0.0/10").Contains(addr) {
		t.Errorf("%v is not matched by the v4 prefix it belongs to", addr)
	}
}
