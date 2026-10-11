// caselock.go — cases that are frozen except for correspondence.
//
// Two states freeze a case:
//
//	AWAITING_PAYMENT  the initial fee has not been paid. Nothing may be done
//	                  on the case until it is; staff may only write to the
//	                  party (payment reminders).
//	INELIGIBLE        the latest eligibility review found the dispute
//	                  ineligible. Staff may only write to the party (the
//	                  ineligibility notice and follow-ups).
//
// The way OUT of each state stays open, otherwise the freeze would be
// permanent: paying the fee (invoice settlement, checkout and check clearing
// are not case-path routes and are untouched here), and re-running the
// eligibility review (a corrected review or an override is a new, later
// review). Reads are never blocked, and the worker token is exempt: the
// workflow and document pipeline act on behalf of the system, not a person.
package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

const (
	lockAwaitingPayment = "AWAITING_PAYMENT"
	lockIneligible      = "INELIGIBLE"
)

// caseLock returns the reason a case is frozen, or "".
func (s *server) caseLock(ctx context.Context, tenant, caseID string) string {
	var status string
	if err := s.db.QueryRow(ctx, fmt.Sprintf(
		`SELECT status FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&status); err != nil {
		return "" // unknown case: let the handler answer 404
	}
	if status == "AWAITING_PAYMENT" {
		return lockAwaitingPayment
	}
	var latest string
	_ = s.db.QueryRow(ctx, `
		SELECT result FROM public.eligibility_reviews
		WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC LIMIT 1`, tenant, caseID).Scan(&latest)
	if latest == "INELIGIBLE" {
		return lockIneligible
	}
	return ""
}

func caseLockMessage(lock string) string {
	switch lock {
	case lockAwaitingPayment:
		return "this case is awaiting payment of the initial fee: only correspondence is allowed until the fee is paid"
	case lockIneligible:
		return "this dispute was found ineligible: only correspondence is allowed (a new eligibility review can change the result)"
	}
	return ""
}

// caseActionAllowed reports whether a case sub-path (the part after
// /cases/{id}/) may be used while the case is frozen.
func caseActionAllowed(lock, action string) bool {
	first := action
	if i := strings.IndexByte(action, '/'); i >= 0 {
		first = action[:i]
	}
	switch first {
	// Writing to the parties: template emails, generated letters, and the
	// log of calls and inquiries exchanged with them.
	case "correspondence", "lettergen", "letters", "calls", "inquiries":
		return true
	case "eligibility":
		return lock == lockIneligible
	}
	return false
}

// caseLockGuard refuses changes to a frozen case. It parses the path itself
// because it runs before the route (and so before chi has {caseId}).
func (s *server) caseLockGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		i := strings.Index(r.URL.Path, "/cases/")
		if i < 0 {
			next.ServeHTTP(w, r)
			return
		}
		rest := strings.Trim(r.URL.Path[i+len("/cases/"):], "/")
		caseID, action, hasAction := strings.Cut(rest, "/")
		if !hasAction || len(caseID) != 36 { // /cases/initiate, /cases/bulk, /cases/relate
			next.ServeHTTP(w, r)
			return
		}
		p, _ := r.Context().Value(ctxPrincipal{}).(principal)
		if contains(p.Roles, serviceRole) {
			next.ServeHTTP(w, r)
			return
		}
		tenant, _ := r.Context().Value(ctxTenant{}).(string)
		if lock := s.caseLock(r.Context(), tenant, caseID); lock != "" && !caseActionAllowed(lock, action) {
			writeJSON(w, http.StatusLocked, map[string]string{"error": caseLockMessage(lock), "lock": lock})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// caseLockedForRequest is the same check for handlers whose case id arrives
// in the body rather than the path (tasks, bulk actions, relating cases).
func (s *server) caseLockedForRequest(r *http.Request, tenant, caseID string) string {
	if caseID == "" {
		return ""
	}
	if p, ok := r.Context().Value(ctxPrincipal{}).(principal); ok && contains(p.Roles, serviceRole) {
		return ""
	}
	return s.caseLock(r.Context(), tenant, caseID)
}
