package main

// emailcheck.go — whether an address can receive the platform's email, beyond
// its spelling (validMailbox): its domain has to exist and take mail (an MX
// record, or an address record, which mail falls back to; a "null MX" says it
// takes none). Mistyped big-provider domains get a suggestion.
//
//   GET /email-check?email=..  -> {"email","valid","problem","suggestion"}
//
// Used where an address will be sent notices or a sign-in: the other side of a
// dispute (single and bulk filings) and the contact on a TPA or plan
// registration. A DNS failure on our side (timeout, resolver down) does not
// refuse an address; only an answer that the domain does not exist or takes
// no mail does. EMAIL_TEST_DOMAINS lists domain suffixes accepted without a
// lookup (dev: ".test", which the test suites use).

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Common misspellings of the large mail providers' domains.
var emailDomainTypos = map[string]string{
	"gmial.com": "gmail.com", "gmai.com": "gmail.com", "gmal.com": "gmail.com", "gmail.co": "gmail.com", "gmail.con": "gmail.com",
	"gnail.com": "gmail.com", "gamil.com": "gmail.com", "gmaill.com": "gmail.com", "gmail.cm": "gmail.com",
	"yaho.com": "yahoo.com", "yahooo.com": "yahoo.com", "yahoo.co": "yahoo.com", "yahoo.con": "yahoo.com",
	"hotmial.com": "hotmail.com", "hotmai.com": "hotmail.com", "hotmail.co": "hotmail.com", "hotmail.con": "hotmail.com",
	"outlok.com": "outlook.com", "outloo.com": "outlook.com", "outlook.co": "outlook.com", "outlook.con": "outlook.com",
	"iclod.com": "icloud.com", "icloud.co": "icloud.com", "aol.co": "aol.com",
}

type emailDomainVerdict struct {
	problem string
	at      time.Time
}

var emailDomainCache sync.Map // domain -> emailDomainVerdict

const emailDomainCacheFor = 10 * time.Minute

func emailTestDomain(domain string) bool {
	for _, suf := range strings.Split(os.Getenv("EMAIL_TEST_DOMAINS"), ",") {
		if suf = strings.ToLower(strings.TrimSpace(suf)); suf != "" && (domain == strings.TrimPrefix(suf, ".") || strings.HasSuffix(domain, suf)) {
			return true
		}
	}
	return false
}

// emailDomainProblem: why mail to this domain cannot be delivered, or "".
func emailDomainProblem(ctx context.Context, domain string) string {
	if v, ok := emailDomainCache.Load(domain); ok && time.Since(v.(emailDomainVerdict).at) < emailDomainCacheFor {
		return v.(emailDomainVerdict).problem
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	problem, settled := "", true
	mx, err := net.DefaultResolver.LookupMX(ctx, domain)
	switch {
	case err == nil && len(mx) == 1 && (mx[0].Host == "." || mx[0].Host == ""):
		problem = domain + " does not accept email"
	case err == nil && len(mx) > 0:
	default:
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound || err == nil {
			// No MX: mail goes to the domain's own address, if it has one.
			if _, herr := net.DefaultResolver.LookupHost(ctx, domain); herr != nil {
				var he *net.DNSError
				if errors.As(herr, &he) && he.IsNotFound {
					problem = "the domain " + domain + " does not exist or does not receive email"
				} else {
					settled = false
				}
			}
		} else {
			settled = false // our DNS failed; do not refuse on that
		}
	}
	if settled {
		emailDomainCache.Store(domain, emailDomainVerdict{problem, time.Now()})
	}
	return problem
}

// emailProblem: why addr will not receive email ("" when it can), and a
// corrected address when the domain looks mistyped.
func emailProblem(ctx context.Context, addr string) (problem, suggestion string) {
	addr = strings.TrimSpace(addr)
	if !validMailbox(addr) {
		return "this is not a valid email address", ""
	}
	local, domain, _ := strings.Cut(addr, "@")
	domain = strings.ToLower(domain)
	if fix, ok := emailDomainTypos[domain]; ok {
		return "check the spelling of " + domain, local + "@" + fix
	}
	if emailTestDomain(domain) {
		return "", ""
	}
	return emailDomainProblem(ctx, domain), ""
}

// emailReachMsg: the refusal for an address that cannot receive email, "" when it can.
func emailReachMsg(ctx context.Context, who, addr string) string {
	problem, suggestion := emailProblem(ctx, addr)
	if problem == "" {
		return ""
	}
	msg := who + " (" + strings.TrimSpace(addr) + "): " + problem
	if suggestion != "" {
		msg += " — did you mean " + suggestion + "?"
	}
	return msg
}

func (s *server) emailCheck(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimSpace(r.URL.Query().Get("email"))
	if addr == "" || len(addr) > 320 {
		http.Error(w, `{"error":"email required"}`, http.StatusBadRequest)
		return
	}
	problem, suggestion := emailProblem(r.Context(), addr)
	writeJSON(w, http.StatusOK, map[string]any{"email": addr, "valid": problem == "", "problem": problem, "suggestion": suggestion})
}
