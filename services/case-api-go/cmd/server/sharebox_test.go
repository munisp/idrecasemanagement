// Author: Patrick Munis at newwave
package main

import "testing"

func TestShareLinkUses(t *testing.T) {
	for _, c := range []struct {
		kind      string
		requested int
		want      int
	}{
		{"upload", 0, uploadLinkFiles}, {"upload", 1, uploadLinkFiles}, {"upload", 10, uploadLinkFiles},
		{"upload", 200, 200}, {"download", 0, 1}, {"download", 3, 3}, {"download", 10, 10},
	} {
		if got := shareLinkUses(c.kind, c.requested); got != c.want {
			t.Errorf("shareLinkUses(%q, %d) = %d, want %d", c.kind, c.requested, got, c.want)
		}
	}
}
