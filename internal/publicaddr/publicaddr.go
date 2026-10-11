// Package publicaddr finds the address a machine reaches the internet from, as a service on the
// internet sees it: the gravel CLI admits the operator's SSH from it, and the hub pins its game
// VMs' firewalls to it (ADR-0003) when hub.yaml does not name it.
package publicaddr

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// TraceURL answers with the caller's public address as Cloudflare's edge sees it ("ip=…"). It is
// an IPv4 literal, so the request leaves over IPv4: a firewall rule is for the address later
// connections come from, and a gravel VM is reached over IPv4.
const TraceURL = "https://1.1.1.1/cdn-cgi/trace"

// Detect asks url (TraceURL, or a test server) for this machine's public address. A nil client is
// http.DefaultClient; the request gives up after ten seconds.
func Detect(ctx context.Context, c *http.Client, url string) (netip.Addr, error) {
	if c == nil {
		c = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("find this machine's public address: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<16))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "ip="); ok {
			if a, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
				return a.Unmap(), nil
			}
		}
	}
	return netip.Addr{}, fmt.Errorf("find this machine's public address: no ip= in %s's answer", url)
}
