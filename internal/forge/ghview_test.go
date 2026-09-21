package forge

import (
	"encoding/json"
	"testing"
	"time"
)

// gh's subcommands return a different shape from the REST API: the author is
// nested under "author", not "user", and a review carries submittedAt where a
// comment carries createdAt.
func TestGhViewCommentShape(t *testing.T) {
	raw := []byte(`{
	  "comments": [{"author": {"login": "sudhirj"}, "createdAt": "2026-08-25T10:14:12Z"}],
	  "reviews":  [{"author": {"login": "sudhirj"}, "submittedAt": "2026-09-05T09:12:18Z"}]
	}`)
	var view struct {
		Comments []ghViewComment `json:"comments"`
		Reviews  []ghViewComment `json:"reviews"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(view.Comments) != 1 || len(view.Reviews) != 1 {
		t.Fatalf("decoded %d comments and %d reviews, want 1 each", len(view.Comments), len(view.Reviews))
	}
	if view.Comments[0].Author.Login != "sudhirj" {
		t.Errorf("comment author = %q, want sudhirj", view.Comments[0].Author.Login)
	}
	wantComment := time.Date(2026, 8, 25, 10, 14, 12, 0, time.UTC)
	if got := view.Comments[0].at(); !got.Equal(wantComment) {
		t.Errorf("comment at() = %v, want %v", got, wantComment)
	}
	// A review has a null createdAt; reading only that field would miss it.
	wantReview := time.Date(2026, 9, 5, 9, 12, 18, 0, time.UTC)
	if got := view.Reviews[0].at(); !got.Equal(wantReview) {
		t.Errorf("review at() = %v, want %v — submittedAt is the review's timestamp", got, wantReview)
	}
}

func TestGhViewCommentMissingTimestamps(t *testing.T) {
	var c ghViewComment
	if got := c.at(); !got.IsZero() {
		t.Errorf("at() = %v on an empty comment, want zero", got)
	}
}

// gh normalises bot logins, reporting "vercel" where REST reports "vercel[bot]".
// Both spellings must fail to match a human's login.
func TestGhViewBotLoginsDoNotMatchAUser(t *testing.T) {
	raw := []byte(`{"comments": [
	  {"author": {"login": "vercel"}, "createdAt": "2026-09-10T00:00:00Z"},
	  {"author": {"login": "copilot-pull-request-reviewer"}, "createdAt": "2026-09-11T00:00:00Z"}
	]}`)
	var view struct {
		Comments []ghViewComment `json:"comments"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, c := range view.Comments {
		if c.Author.Login == "sudhirj" {
			t.Errorf("bot login %q matched the configured user", c.Author.Login)
		}
	}
}
