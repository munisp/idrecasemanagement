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
	// The copy kept in the mail log must not contain the password.
	_, logged := tpaWelcomeMail("Acme Revenue Cycle", "", "dana.lee", "", "https://idre.example.gov")
	if strings.Contains(logged, "Tmp-9f2c!x") || !strings.Contains(logged, "not stored here") {
		t.Errorf("logged copy must carry a placeholder, not a password: %q", logged)
	}
	if !strings.HasPrefix(logged, "Hello,") {
		t.Errorf("no contact name should greet generically: %q", logged[:20])
	}
}
