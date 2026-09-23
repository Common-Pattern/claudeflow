package doctor

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// checkTranscripts reports whether the transcript server will be reachable at
// the address its links will name.
//
// The failure it exists to catch is quiet and total: `host: bigone` on a
// Tailscale machine resolves through /etc/hosts to 127.0.1.1, so the server
// binds loopback and answers nobody, while the links it hands out are followed
// from another device that resolves the same name through MagicDNS to the
// tailnet address. Nothing errors. The server is up, the links are well-formed,
// and every one of them times out. Resolving the name here and comparing it to
// the networks the operator said should reach it turns that into one line of
// output before the first link is posted.
func checkTranscripts(ctx context.Context, o Options) Result {
	r := Result{Name: "transcripts", Status: OK}
	t := o.Cfg.Transcripts
	if !t.Enabled() {
		r.Detail = "not served — runs report a path on this host"
		return r
	}

	prefixes, err := t.Prefixes()
	if err != nil {
		return Result{Name: "transcripts", Status: Fail, Detail: err.Error()}
	}
	r.Detail = fmt.Sprintf("http://%s/ to %s", t.Addr(), summarise(prefixes))

	if t.Host == "" {
		// Validation rejects this, so reaching it means the config was not
		// loaded through Parse. Report it as what it is rather than as a mode.
		r.Status = Fail
		r.Detail += " — no host: this would bind every interface"
		r.Fix = "set transcripts.host to the address runs should be reachable at"
		return r
	}

	addrs, err := o.resolve(ctx, t.Host)
	if err != nil || len(addrs) == 0 {
		r.Status = Warn
		r.Detail += fmt.Sprintf(" — %s does not resolve here", t.Host)
		r.Fix = "the server will not bind until it does; use a name this host resolves, or an address"
		return r
	}

	// Reachability is decided by where the name points, not by where the
	// operator meant it to point.
	var loopback, matched bool
	for _, a := range addrs {
		if a.IsLoopback() {
			loopback = true
		}
		for _, p := range prefixes {
			if p.Contains(a) {
				matched = true
			}
		}
	}
	switch {
	case loopback && !matched:
		r.Status = Warn
		r.Detail += fmt.Sprintf(" — %s resolves to %s here, which no allowed network contains", t.Host, join(addrs))
		r.Fix = "links would be followed to a different address than the server binds; use the fully qualified name (a Tailscale host's MagicDNS name, not its short hostname, which /etc/hosts maps to 127.0.1.1)"
	case !matched:
		r.Status = Warn
		r.Detail += fmt.Sprintf(" — %s resolves to %s, outside every allowed network", t.Host, join(addrs))
		r.Fix = "a reader on an allowed network would be refused; widen transcripts.allow or bind an address inside it"
	default:
		r.Detail += fmt.Sprintf(" — %s is %s", t.Host, join(addrs))
	}
	return r
}

// resolve looks a host up, using Options.Resolve where a test supplies one.
func (o Options) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if o.Resolve != nil {
		return o.Resolve(ctx, host)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.Unmap())
	}
	return out, nil
}

func join(addrs []netip.Addr) string {
	parts := make([]string, len(addrs))
	for i, a := range addrs {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", ")
}

func summarise(prefixes []netip.Prefix) string {
	parts := make([]string, len(prefixes))
	for i, p := range prefixes {
		parts[i] = p.String()
	}
	return strings.Join(parts, ", ")
}
