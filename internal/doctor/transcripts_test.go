package doctor

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/Common-Pattern/claudeflow/internal/config"
)

// transcriptOpts builds options whose only interesting part is the transcript
// server and what its host resolves to.
func transcriptOpts(t config.Transcripts, resolved map[string][]string) Options {
	return Options{
		HaveConfig: true,
		Cfg:        config.Config{Transcripts: t},
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			names, ok := resolved[host]
			if !ok {
				return nil, errors.New("no such host")
			}
			out := make([]netip.Addr, len(names))
			for i, n := range names {
				out[i] = netip.MustParseAddr(n)
			}
			return out, nil
		},
	}
}

var tailnet = []string{"100.64.0.0/10"}

// The trap this check exists for: the short hostname is in /etc/hosts, so the
// server binds loopback while every link it hands out is followed to the
// tailnet address. Nothing errors; the links simply time out.
func TestShortHostnameBindingLoopbackIsAWarning(t *testing.T) {
	o := transcriptOpts(
		config.Transcripts{Host: "bigone", Port: 8787, Allow: tailnet},
		map[string][]string{"bigone": {"127.0.1.1"}},
	)
	r := checkTranscripts(context.Background(), o)
	if r.Status != Warn {
		t.Fatalf("status = %s, want warn", r.Status)
	}
	if !strings.Contains(r.Detail, "127.0.1.1") {
		t.Errorf("detail does not say what it resolved to: %s", r.Detail)
	}
	if !strings.Contains(r.Fix, "MagicDNS") {
		t.Errorf("fix does not name the remedy: %s", r.Fix)
	}
}

func TestTailnetHostnameIsFine(t *testing.T) {
	o := transcriptOpts(
		config.Transcripts{Host: "bigone.quaver-galaxy.ts.net", Port: 8787, Allow: tailnet},
		map[string][]string{"bigone.quaver-galaxy.ts.net": {"100.101.83.64"}},
	)
	r := checkTranscripts(context.Background(), o)
	if r.Status != OK {
		t.Fatalf("status = %s (%s), want ok", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "100.101.83.64") {
		t.Errorf("detail = %s, want the resolved address", r.Detail)
	}
}

// A host outside every allowed network binds somewhere nobody permitted can
// reach — the mirror of the loopback case.
func TestHostOutsideTheAllowListIsAWarning(t *testing.T) {
	o := transcriptOpts(
		config.Transcripts{Host: "box.lan", Port: 8787, Allow: tailnet},
		map[string][]string{"box.lan": {"192.168.1.20"}},
	)
	r := checkTranscripts(context.Background(), o)
	if r.Status != Warn || !strings.Contains(r.Detail, "outside every allowed network") {
		t.Fatalf("status = %s, detail = %s", r.Status, r.Detail)
	}
}

func TestUnresolvableHostIsAWarning(t *testing.T) {
	o := transcriptOpts(config.Transcripts{Host: "nowhere.invalid", Port: 8787, Allow: tailnet}, nil)
	r := checkTranscripts(context.Background(), o)
	if r.Status != Warn || !strings.Contains(r.Detail, "does not resolve") {
		t.Fatalf("status = %s, detail = %s", r.Status, r.Detail)
	}
}

// Binding every interface is not a supported mode: validation rejects it, and
// a config that reached here without going through Parse is a failure.
func TestEmptyHostIsAFailure(t *testing.T) {
	o := transcriptOpts(config.Transcripts{Port: 8787, Allow: tailnet}, nil)
	r := checkTranscripts(context.Background(), o)
	if r.Status != Fail || !strings.Contains(r.Detail, "every interface") {
		t.Fatalf("status = %s, detail = %s", r.Status, r.Detail)
	}
}

func TestTranscriptsOffIsNotAFinding(t *testing.T) {
	r := checkTranscripts(context.Background(), Options{HaveConfig: true})
	if r.Status != OK {
		t.Fatalf("status = %s, want ok when serving is not configured", r.Status)
	}
}
