package main

import (
	"strings"
	"testing"
)

func TestTPAWelcomeMail(t *testing.T) {
	subject, body := tpaWelcomeMail("Acme Revenue Cycle", "Dana Lee", "dana.lee", "Tmp-9f2c!x", "https://idre.example.gov/")
	if !strings.Contains(subject, "Acme Revenue Cycle") {
		t.Errorf("subject should name the organization: %q", subject)
	}
	for _, want := range []string{"Hello Dana Lee,", "https://idre.example.gov/app.html", "Username:  dana.lee", "Password:  Tmp-9f2c!x", "choose a new one"} {
		if !strings.Contains(body, want) {
			t.Errorf("email body is missing %q", want)
		}
	}
	// What the mail log keeps must carry no sign-in detail at all.
	for _, secret := range []string{"dana.lee", "Tmp-9f2c!x", "Username", "Password", "app.html"} {
		if strings.Contains(tpaWelcomeLogNote, secret) {
			t.Errorf("mail-log note must not contain %q", secret)
		}
	}
	if _, generic := tpaWelcomeMail("Acme Revenue Cycle", "", "dana.lee", "x", "https://idre.example.gov"); !strings.HasPrefix(generic, "Hello,") {
		t.Errorf("no contact name should greet generically: %q", generic[:20])
	}
}

func TestValidMailbox(t *testing.T) {
	for _, ok := range []string{"dana.lee@acme-rcm.com", "billing+idr@northgate.health", "a@b.co"} {
		if !validMailbox(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", "no-at-sign", "bad address@nowhere", "double..dot@resend.dev", "trailingdot.@resend.dev",
		".lead@x.com", "user@localhost", "user@.com", "Dana <dana@x.com>", "a@x.com, b@y.com"} {
		if validMailbox(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
