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
// A log is the agent CLI's event stream, so the link opens a page that renders
// it and follows it while the run is going; ?raw is the file as it lies.
//
// The server is read-only, has no routes but a listing, a file and the fonts
// its pages are set in, and answers nobody outside the configured CIDR blocks.
package transcripts

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// fonts holds IBM Plex Sans and Mono (SIL OFL 1.1, fonts/OFL.txt). They are
// served from the binary rather than a font CDN: the pages are read on a
// private network, and a reader's browser should not ask a third party for
// anything to show one.
//
//go:embed fonts/*.woff2 fonts/OFL.txt
var fonts embed.FS

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
	if font, ok := strings.CutPrefix(name, "fonts/"); ok {
		serveFont(w, r, font)
		return
	}
	s.file(w, r, name)
}

// serveFont answers for one embedded font file and nothing else. The name is
// held to a single path element, so the request cannot reach the license file
// or anything outside the font directory by way of the embedded tree.
func serveFont(w http.ResponseWriter, r *http.Request, name string) {
	if name != path.Base(name) || !strings.HasSuffix(name, ".woff2") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	data, err := fs.ReadFile(fonts, "fonts/"+name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The files change only with the binary, and their names are not
	// versioned, so a week rather than forever.
	w.Header().Set("Cache-Control", "public, max-age=604800")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// fontCSS declares the embedded fonts for both pages. The generic family after
// each is for glyphs Plex does not draw, not a substitute for it.
const fontCSS = `
  @font-face { font-family: "IBM Plex Mono"; font-weight: 400; font-display: swap; src: url("/fonts/IBMPlexMono-Regular.woff2") format("woff2"); }
  @font-face { font-family: "IBM Plex Mono"; font-weight: 600; font-display: swap; src: url("/fonts/IBMPlexMono-SemiBold.woff2") format("woff2"); }
  @font-face { font-family: "IBM Plex Sans"; font-weight: 400; font-display: swap; src: url("/fonts/IBMPlexSans-Regular.woff2") format("woff2"); }
  @font-face { font-family: "IBM Plex Sans"; font-weight: 600; font-display: swap; src: url("/fonts/IBMPlexSans-SemiBold.woff2") format("woff2"); }
  :root { color-scheme: light dark; }
  body { font: 14px/1.5 "IBM Plex Mono", monospace; }
  b, strong { font-weight: 600; }
`

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

	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A transcript is rewritten while its run is live and is deleted by
	// housekeeping; nothing should hold a copy of one.
	w.Header().Set("Cache-Control", "no-store")

	query := r.URL.Query()
	switch {
	case query.Has("raw"):
		// Set before ServeContent, which otherwise sniffs — and a transcript
		// that began with markup would then be served as a document to render
		// rather than as the text it is. nosniff stops the browser
		// second-guessing that.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.ServeContent(w, r, name, info.ModTime(), f)
	case query.Has("from"):
		s.follow(w, f, query.Get("from"))
	default:
		s.view(w, f, name)
	}
}

// consume renders what the log holds past an offset, and returns the offset to
// ask from next. It stops at the last line break: a run may be halfway through
// writing a line, and that line is read whole on the next request.
func consume(f io.ReadSeeker, from int64) (template.HTML, int64, bool, error) {
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return "", from, false, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return "", from, false, err
	}
	end := bytes.LastIndexByte(raw, '\n')
	if end < 0 {
		return "", from, false, nil
	}
	fragment, done := render(raw[:end+1])
	return fragment, from + int64(end) + 1, done, nil
}

// follow answers the page's poll with the events since its last one.
func (s Server) follow(w http.ResponseWriter, f io.ReadSeeker, from string) {
	offset, err := strconv.ParseInt(from, 10, 64)
	if err != nil || offset < 0 {
		http.Error(w, "bad offset", http.StatusBadRequest)
		return
	}
	fragment, next, done, err := consume(f, offset)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Next-Offset", strconv.FormatInt(next, 10))
	if done {
		w.Header().Set("X-Run-Done", "1")
	}
	if _, err := io.WriteString(w, string(fragment)); err != nil {
		s.logf("transcripts: write update: %v", err)
	}
}

// view renders the page for one transcript: the run so far, and the script
// that keeps it current.
func (s Server) view(w http.ResponseWriter, f io.ReadSeeker, name string) {
	fragment, next, done, err := consume(f, 0)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := viewTmpl.Execute(w, struct {
		Name, Repo string
		Events     template.HTML
		Next       int64
		Done       bool
	}{name, s.Repo, fragment, next, done}); err != nil {
		s.logf("transcripts: render %s: %v", name, err)
	}
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
<style>` + fontCSS + `
  body { margin: 2rem auto; max-width: 48rem; padding: 0 1rem; }
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

// viewTmpl shows one transcript. A page opened on a finished run does not poll.
// A run that is killed never sends its closing event, so a page left open on
// one keeps polling; that costs one small request every two seconds.
var viewTmpl = template.Must(template.New("view").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}}</title>
<style>` + fontCSS + `
  body { margin: 2rem auto; max-width: 64rem; padding: 0 1rem; }
  header { display: flex; flex-wrap: wrap; gap: 1rem; font-weight: 600; margin-bottom: 1.5rem; }
  header a, header span, header button { font-weight: 400; opacity: .6; }
  header button { font-family: inherit; font-size: inherit; color: inherit; background: none; border: 0; padding: 0; cursor: pointer; text-decoration: underline dotted; }
  time { cursor: pointer; }
  header .name { flex: 1; font-weight: 600; opacity: 1; overflow-wrap: anywhere; }
  .ev { display: grid; grid-template-columns: 5em minmax(0, 1fr); column-gap: .75rem; padding: .2rem 0; }
  .t { font-family: "IBM Plex Mono", monospace; opacity: .45; white-space: nowrap; }
  .b { white-space: pre-wrap; overflow-wrap: anywhere; }
  .sub .b { border-left: 2px solid color-mix(in srgb, currentColor 25%, transparent); padding-left: .75rem; }
  .say { font-family: "IBM Plex Sans", sans-serif; padding: .6rem 0; }
  @media (max-width: 40rem) {
    .ev { display: block; }
    .t { display: block; font-size: .85em; }
    .t:empty { display: none; }
  }
  .plain, .note, .output { opacity: .65; }
  .error { color: #d1242f; opacity: 1; }
  summary { cursor: pointer; }
  pre { font: inherit; margin: .25rem 0 .5rem; max-height: 32rem; overflow: auto; padding: .5rem; white-space: pre-wrap;
        background: color-mix(in srgb, currentColor 7%, transparent); }
</style>
<header><span class="name">{{.Name}}</span>{{with .Repo}}<span>{{.}}</span>{{end}}<span id="state">{{if .Done}}finished{{else}}following{{end}}</span><button id="zone" type="button" title="Switch times between UTC and local time">UTC</button><a href="?raw">raw</a><a href="/">all runs</a></header>
<main id="log" data-next="{{.Next}}"{{if .Done}} data-done{{end}}>
{{.Events}}</main>
<script>
  const log = document.getElementById('log'), state = document.getElementById('state');
  let next = Number(log.dataset.next);
  // The server writes times in UTC. The page shows them in the reader's zone,
  // or in UTC after a click on any time or on the switch in the header; the
  // choice is kept per browser, where storage allows it.
  const zoneButton = document.getElementById('zone');
  let utc = false;
  try { utc = localStorage.getItem('claudeflow-zone') === 'utc'; } catch {}
  function show(t) {
    const d = new Date(t.dateTime);
    if (isNaN(d)) return;
    const timeZone = utc ? 'UTC' : undefined;
    t.textContent = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23', timeZone });
    t.title = d.toLocaleString([], { dateStyle: 'medium', timeStyle: 'long', timeZone }) + ' (click for ' + (utc ? 'local time' : 'UTC') + ')';
    t.dataset.shown = '';
  }
  function localize(all) {
    for (const t of log.querySelectorAll(all ? 'time' : 'time:not([data-shown])')) show(t);
    zoneButton.textContent = utc ? 'UTC' : 'local time';
  }
  function switchZone() {
    utc = !utc;
    try { localStorage.setItem('claudeflow-zone', utc ? 'utc' : 'local'); } catch {}
    localize(true);
  }
  zoneButton.addEventListener('click', switchZone);
  log.addEventListener('click', (e) => { if (e.target.closest('time')) switchZone(); });
  localize(true);
  async function poll() {
    try {
      const res = await fetch(location.pathname + '?from=' + next);
      if (res.status === 404) { state.textContent = 'removed'; return; }
      if (res.ok) {
        const page = document.documentElement;
        const atEnd = innerHeight + scrollY >= page.scrollHeight - 80;
        log.insertAdjacentHTML('beforeend', await res.text());
        localize(false);
        next = Number(res.headers.get('X-Next-Offset'));
        if (atEnd) scrollTo(0, page.scrollHeight);
        if (res.headers.has('X-Run-Done')) { state.textContent = 'finished'; return; }
      }
    } catch { /* The next poll tries again: a host restart should not end the page. */ }
    setTimeout(poll, 2000);
  }
  if (!('done' in log.dataset)) { scrollTo(0, document.documentElement.scrollHeight); setTimeout(poll, 2000); }
</script>
`))
