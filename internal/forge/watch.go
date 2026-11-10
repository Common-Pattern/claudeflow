package forge

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// IssuesChanged reports whether anything on the repository's issues or pull
// requests has changed since the request that returned etag, and returns the
// ETag to pass next time. An empty etag always reports a change.
//
// It asks for the single most recently updated issue. Opening an issue,
// labelling one and commenting on one all bump its updated_at, which moves it
// to the top of that list and so changes the response — one request covers
// every event a tick acts on. Asked with If-None-Match, an unchanged list is
// answered 304 Not Modified, which GitHub does not count against the rate
// limit. That is what makes asking every few seconds affordable where a full
// tick is not.
//
// GitHub has no long-poll for any of this: a request returns at once whether
// or not anything changed. This is the cheapest answer short of a webhook, and
// a webhook needs an address GitHub can reach.
func (g *GH) IssuesChanged(ctx context.Context, etag string) (string, bool, error) {
	args := []string{"api", "-i",
		"repos/" + g.Repo + "/issues?state=all&sort=updated&direction=desc&per_page=1"}
	if etag != "" {
		args = append(args, "-H", "If-None-Match: "+etag)
	}
	cmd := exec.CommandContext(ctx, g.bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// gh exits non-zero on a 304 but still prints the response head, so the
	// status line, not the exit code, says what happened.
	status, next, err := parseHead(stdout.Bytes())
	if err != nil {
		if runErr != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = runErr.Error()
			}
			return etag, false, fmt.Errorf("gh %s: %s", strings.Join(args, " "), msg)
		}
		return etag, false, err
	}
	switch {
	case status == 304:
		return etag, false, nil
	case status >= 200 && status < 300:
		return next, next == "" || next != etag, nil
	}
	return etag, false, fmt.Errorf("issues changed: HTTP %d: %s", status, strings.TrimSpace(stderr.String()))
}

// parseHead reads the status code and ETag out of `gh api -i` output.
func parseHead(out []byte) (int, string, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	if !sc.Scan() {
		return 0, "", errors.New("issues changed: empty response")
	}
	var proto string
	var status int
	if _, err := fmt.Sscanf(sc.Text(), "%s %d", &proto, &status); err != nil || !strings.HasPrefix(proto, "HTTP/") {
		return 0, "", fmt.Errorf("issues changed: no status line in %q", sc.Text())
	}
	var etag string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "etag") {
			etag = strings.TrimSpace(value)
		}
	}
	return status, etag, nil
}
