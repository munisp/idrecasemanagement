// client_ip.go — the address a request really came from, for rate limits and logs (audit F-08, 2026-11-13).
//
// Every proxy on the way appends the address it received the request from to X-Forwarded-For: Cloudflare appends
// the visitor, the gateway appends Cloudflare's edge, the portal's nginx appends the gateway. The left of the header
// is whatever the client sent, so it is never trusted: the client is the right-most address that is not one of our
// own hops (private and cluster ranges, Cloudflare's published edge ranges). A request sent straight to the origin,
// skipping Cloudflare, still resolves to its real sender, because the first proxy appends it.
package main

import (
	"net"
	"net/http"
	"strings"
)

// trustedProxyNets: our own hops. Cloudflare's ranges are its published list (https://www.cloudflare.com/ips/).
var trustedProxyNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "100.64.0.0/10", "::1/128", "fc00::/7", "fe80::/10",
		"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18", "108.162.192.0/18",
		"190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
		"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
		"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32", "2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
	} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

func trustedProxy(ip net.IP) bool {
	for _, n := range trustedProxyNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIPFrom: the right-most untrusted address of the forwarding chain ending at remoteAddr. Pure, so it is tested.
func clientIPFrom(xff, remoteAddr string) string {
	hops := []string{}
	for _, h := range strings.Split(xff, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hops = append(hops, h)
		}
	}
	remote := remoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		remote = host
	}
	hops = append(hops, remote)
	origin := ""
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.Trim(hops[i], "[]"))
		if ip == nil {
			continue
		}
		origin = ip.String()
		if !trustedProxy(ip) {
			return origin
		}
	}
	// Every hop is ours (a call from inside the cluster): the one it started from.
	if origin != "" {
		return origin
	}
	return remote
}

func clientIP(r *http.Request) string {
	return clientIPFrom(r.Header.Get("X-Forwarded-For"), r.RemoteAddr)
}
