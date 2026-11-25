package main

// filing_checks.go — what every way of filing a dispute checks the same way.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxDisputedCents: the same ceiling a TPA's own edit of a filing has (tpa_edit.go, $1 billion). A single FL claim
// dispute is far below it; an amount above is a typo (extra zeros, cents typed as dollars), and the database and the
// fee arithmetic shouldn't be the ones to find out.
const maxDisputedCents = int64(maxDisputedAmountCents)

// disputedAmountProblem: what is wrong with a disputed amount, or "". required: the filing must state one (a TPA's
// or a health plan's filing); the public intake may leave it for the filing packet.
func disputedAmountProblem(cents int64, required bool) string {
	switch {
	case cents < 0:
		return "the amount in dispute can't be negative"
	case cents == 0 && required:
		return "give the amount in dispute (more than $0)"
	case cents > maxDisputedCents:
		return fmt.Sprintf("the amount in dispute (%s) is over %s — check it for an extra digit", usd(cents), usd(maxDisputedCents))
	}
	return ""
}

// decodeProblem: a request body that couldn't be read, in words -- which field has the wrong kind of value --
// instead of a later "x required" that hides the real mistake.
func decodeProblem(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Field != "" {
		want := te.Type.String()
		if strings.Contains(want, "int") || strings.Contains(want, "float") {
			want = "a number"
		}
		return fmt.Sprintf("%s must be %s", te.Field, want)
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return "the request isn't valid JSON"
	}
	return "the request couldn't be read"
}

// malformedRequest: a request no route answers sensibly -- a NUL byte in the path or a query value (Postgres
// refuses it, which surfaced as a 500 "db" error), or a ".." path segment (an id like "../../etc/passwd" reached
// the database the same way). Refused up front.
func malformedRequest(r *http.Request) bool {
	if strings.ContainsRune(r.URL.Path, 0) || strings.Contains(strings.ToLower(r.URL.RawQuery), "%00") {
		return true
	}
	for _, vs := range r.URL.Query() {
		for _, v := range vs {
			if strings.ContainsRune(v, 0) {
				return true
			}
		}
	}
	for _, p := range []string{r.URL.Path, r.URL.RawPath} {
		decoded, err := url.PathUnescape(p)
		if err != nil {
			return true
		}
		for _, seg := range strings.Split(decoded, "/") {
			if seg == ".." {
				return true
			}
		}
	}
	return false
}

func rejectMalformed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if malformedRequest(r) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
