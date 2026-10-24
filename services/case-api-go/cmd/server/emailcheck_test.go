// Author: Patrick Munis at newwave
package main

import (
	"context"
	"testing"
)

func TestEmailProblem(t *testing.T) {
	t.Setenv("EMAIL_TEST_DOMAINS", ".test")
	ctx := context.Background()
	for _, c := range []struct {
		addr       string
		ok         bool
		suggestion string
	}{
		{"not-an-email", false, ""},
		{"a b@x.com", false, ""},
		{"claims@gmial.com", false, "claims@gmail.com"},
		{"claims@Hotmial.com", false, "claims@hotmail.com"},
		{"qa-1@idre.test", true, ""},
	} {
		p, s := emailProblem(ctx, c.addr)
		if (p == "") != c.ok || s != c.suggestion {
			t.Errorf("%s: problem=%q suggestion=%q", c.addr, p, s)
		}
	}
	if emailTestDomain("idre.com") {
		t.Error("idre.com is not a test domain")
	}
}
