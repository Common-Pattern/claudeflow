// Package transcripts serves run logs over HTTP on a private network.
//
// It exists so that a comment on an issue can link to what the run actually
// printed. The alternative — naming a path on the host — is only useful to a
// reader who is already logged in to that host, and the alternatives to that
// all involve copying the transcript somewhere: a gist, which is unlisted
// rather than private, or a branch in the repository, which puts run output
// into the project's history. Serving the file where it already lies copies
// nothing and expires nothing that housekeeping does not already expire.
//
// The server is read-only, has no routes but a listing and a file, and answers
// nobody outside the configured CIDR blocks.
package transcripts

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Server publishes a directory of run transcripts.
type Server struct {
	// Dir is the log directory. Nothing outside it is ever opened.
	Dir string
	// Addr is the listen address, host:port.
	Addr string
	// Allow lists the networks permitted to read.
	Allow []netip.Prefix
	// Repo is shown in the listing, so that a tab opened days later says which
	// project it belongs to.
	Repo string
	// Log receives the startup line and every refusal.
	Log func(format string, args ...any)
}

func (s Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// Serve listens and serves until ctx is cancelled.
//
// It returns the listen error rather than handling it, because whether a
// supervisor should stop when its transcript server cannot bind is the
// caller's decision, not this package's.
func (s Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("transcripts: listen on %s: %w", s.Addr, err)
	}

	srv := &http.Server{
		Handler: s.Handler(),
		// A transcript is read by a person with a browser open. These bound the
		// damage from a connection that stops reading rather than the size of
		// what may be read.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	s.logf("transcripts: serving %s on http://%s/ to %s", s.Dir, ln.Addr(), describe(s.Allow))

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// describe renders the allow list for the startup line.
func describe(allow []netip.Prefix) string {
	if len(allow) == 0 {
		return "nobody"
	}
	parts := make([]string, len(allow))
	for i, p := range allow {
		parts[i] = p.String()
	}
	return strings.Join(parts, ", ")
}

// Handler returns the routes, so tests need no socket.
func (s Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.route)
	return s.guard(mux)
}

// guard refuses anything from outside the allow list.
//
// The peer address is taken from the connection and nothing else. An
// X-Forwarded-For header is a claim made by the client, and this server sits
// behind no proxy that could make it a fact; trusting it here would let any
// permitted network be asserted by whoever could reach the port at all.
func (s Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, err := peer(r.RemoteAddr)
		if err != nil || !s.permitted(addr) {
			s.logf("transcripts: refused %s for %s", r.RemoteAddr, r.URL.Path)
			// No detail: a refused caller learns that something is listening
			// and nothing else about what it holds.
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// peer parses the connecting address, discarding the zone an IPv6 link-local
// address carries — netip keeps it, and a prefix never matches with it.
func peer(remote string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return netip.Addr{}, err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, err
	}
	return addr.Unmap().WithZone(""), nil
}

func (s Server) permitted(addr netip.Addr) bool {
	for _, p := range s.Allow {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func (s Server) route(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		s.index(w, r)
		return
	}
	s.file(w, r, name)
}

// safeName reports whether a request names a transcript in the log directory
// and nothing else.
//
// The check is a whitelist rather than a search for "..": the request is one
// path element, it ends in .log, and it is exactly its own base name. A name
// that is not all three is refused without the server deciding what it meant.
func safeName(name string) bool {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return false
	}
	return strings.HasSuffix(name, ".log")
}

func (s Server) file(w http.ResponseWriter, r *http.Request, name string) {
	if !safeName(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	full := filepath.Join(s.Dir, name)
	f, err := os.Open(full)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Set before ServeContent, which otherwise sniffs — and a transcript that
	// began with markup would then be served as a document to render rather
	// than as the text it is. nosniff stops the browser second-guessing that.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A transcript is rewritten while its run is live and is deleted by
	// housekeeping; nothing should hold a copy of one.
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// entry is one transcript in the listing.
type entry struct {
	Name     string
	Size     int64
	Modified time.Time
}

// Age renders how long ago a run wrote to its transcript.
func (e entry) Age() string {
	d := time.Since(e.Modified).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// KB renders a size in whole kilobytes, which is the only precision anyone
// reading a list of logs wants.
func (e entry) KB() string {
	if e.Size < 1024 {
		return fmt.Sprintf("%d B", e.Size)
	}
	return fmt.Sprintf("%d KB", e.Size/1024)
}

func (s Server) index(w http.ResponseWriter, _ *http.Request) {
	items, err := os.ReadDir(s.Dir)
	if err != nil {
		http.Error(w, "no transcripts", http.StatusNotFound)
		return
	}
	var list []entry
	for _, it := range items {
		if it.IsDir() || !safeName(it.Name()) {
			continue
		}
		info, err := it.Info()
		if err != nil {
			continue
		}
		list = append(list, entry{Name: it.Name(), Size: info.Size(), Modified: info.ModTime()})
	}
	// Newest first: the transcript someone is looking for is almost always the
	// one that was just written.
	sort.Slice(list, func(i, j int) bool { return list[i].Modified.After(list[j].Modified) })

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if err := indexTmpl.Execute(w, struct {
		Repo  string
		Items []entry
	}{s.Repo, list}); err != nil {
		s.logf("transcripts: render listing: %v", err)
	}
}

// indexTmpl is html/template, so a file name is escaped on the way out even
// though claudeflow is the only thing that ever writes one.
var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>claudeflow transcripts{{with .Repo}} — {{.}}{{end}}</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; margin: 2rem auto; max-width: 48rem; padding: 0 1rem; }
  h1 { font-size: 1rem; font-weight: 600; margin-bottom: 1.5rem; }
  ol { list-style: none; padding: 0; }
  li { display: flex; gap: 1rem; padding: .35rem 0; border-bottom: 1px solid color-mix(in srgb, currentColor 12%, transparent); }
  a { flex: 1; text-decoration: none; }
  a:hover { text-decoration: underline; }
  span { opacity: .6; white-space: nowrap; }
  p { opacity: .6; }
</style>
<h1>claudeflow transcripts{{with .Repo}} — {{.}}{{end}}</h1>
{{- if .Items}}
<ol>
{{- range .Items}}
  <li><a href="/{{.Name}}">{{.Name}}</a><span>{{.KB}}</span><span>{{.Age}}</span></li>
{{- end}}
</ol>
{{- else}}
<p>No transcripts yet.</p>
{{- end}}
`))
