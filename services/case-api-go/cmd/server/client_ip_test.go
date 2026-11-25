package main

import "testing"

func TestClientIPIgnoresForgedLeftEntries(t *testing.T) {
	for _, c := range []struct{ name, xff, remote, want string }{
		{"through Cloudflare, forged left entries", "1.2.3.4, 6.6.6.6, 203.0.113.9, 162.158.1.1, 10.244.1.5", "10.244.2.9:51000", "203.0.113.9"},
		{"through Cloudflare, no forgery", "203.0.113.9, 162.158.1.1, 10.244.1.5", "10.244.2.9:51000", "203.0.113.9"},
		{"straight to the origin, forged header", "1.2.3.4, 198.51.100.7", "10.244.2.9:51000", "198.51.100.7"},
		{"no header", "", "198.51.100.7:4000", "198.51.100.7"},
		{"inside the cluster", "10.244.1.5", "10.244.2.9:51000", "10.244.1.5"},
		{"garbage entries", "not-an-ip, 203.0.113.9, junk", "10.0.0.1:1", "203.0.113.9"},
		{"IPv6 visitor through Cloudflare", "2001:db8::1, 2606:4700::1", "10.244.2.9:1", "2001:db8::1"},
	} {
		if got := clientIPFrom(c.xff, c.remote); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
