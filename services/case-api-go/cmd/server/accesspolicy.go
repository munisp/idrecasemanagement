// accesspolicy.go — who may reach which part of a tenant's API, decided in
// one place and before any handler runs.
//
// Two gaps made this necessary, both found by calling every route as every
// role instead of reading the menu:
//
//  1. A TPA (an outside organization filing on behalf of others) could read
//     ANY case in the tenant by id — its record, its documents, its timeline
//     — and could find those ids through search. The case list refused a
//     TPA; the routes underneath it did not, because each relied on the list
//     being the only way in.
//  2. Several tenant-wide lists (intake, deliverables, call logs) had no
//     role check at all: the page was hidden from a role's menu, and the
//     data behind it was one typed address away.
//
// Hiding a menu item is a courtesy, not a boundary. The boundary is here.
package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// isExternalOnly reports whether the principal is an outside organization's
// account with no staff role: a TPA or a health plan. Such a principal has
// standing only in its own area and in what it filed. (A health plan's login
// is created by the public registration page, so it must never pass as
// staff.)
//
// A filing PARTY (the federal process) is outside too, but is NOT covered
// here: nothing in the platform records which cases belong to a party, so
// there is no "its own cases" to limit it to, and refusing it everything
// would stop the federal filing flow. That gap is open and reported; it
// needs an ownership record before it can be closed.
func isExternalOnly(p principal) bool {
	party := false
	for _, r := range p.Roles {
		switch {
		case tenantStaffRoles[r], r == "FEDERAL_ADMIN", r == "PLATFORM_ADMIN", r == "STATE_AUDITOR", r == serviceRole:
			return false
		case r == "PARTY":
			party = true
		}
	}
	// No office role: outside. That includes a login with no role at all --
	// such an account used to pass as staff, and a public registration whose
	// role assignment failed produced exactly that. Only a PARTY stays out of
	// this guard (the gap above), unless it is also a TPA or a plan.
	return !party || hasAnyRole(p, "TPA", "HEALTH_PLAN")
}

// tpaOpenPaths are the parts of the API a TPA uses that are not about one
// case: its own area, the shell every signed-in page needs, and the two
// handlers that already answer a TPA from its own filings only.
var tpaOpenPaths = []*regexp.Regexp{
	regexp.MustCompile(`^/tpa(/|$)`),
	regexp.MustCompile(`^/calendar$`),        // calendar() answers a TPA from its own filings
	regexp.MustCompile(`^/intake/bulk(/|$)`), // batch filing: the handlers scope to the caller's own batches
}

// hpOpenPaths: a health plan's own area (its profile, parties, packages,
// filings and the disputes against it -- every handler scopes to the plan the
// login belongs to).
var hpOpenPaths = []*regexp.Regexp{
	regexp.MustCompile(`^/hp(/|$)`),
}

// outsideShellPaths are what every signed-in page needs, for any outside account.
var outsideShellPaths = []*regexp.Regexp{
	regexp.MustCompile(`^/program$`),           // the portal reads the program's settings to draw itself
	regexp.MustCompile(`^/prefs$`),             // the caller's own preferences
	regexp.MustCompile(`^/notifications(/|$)`), // the caller's own notifications
	regexp.MustCompile(`^/presence/ping$`),
}

func outsideOpenPaths(p principal) []*regexp.Regexp {
	out := append([]*regexp.Regexp{}, outsideShellPaths...)
	if hasRole(p, "TPA") {
		out = append(out, tpaOpenPaths...)
	}
	if hasRole(p, "HEALTH_PLAN") {
		out = append(out, hpOpenPaths...)
	}
	return out
}

// tpaOwnCasePaths are what a TPA may read of a case IT filed: the record,
// its deadlines and the documents. Nothing internal — no timeline, review
// notes, eligibility evidence, QA drafts or correspondence log.
var tpaOwnCasePaths = regexp.MustCompile(`^/cases/([0-9a-fA-F-]{36})(|/clocks|/documents|/documents/[0-9a-fA-F-]{36}/download)$`)

// pagePolicy is a read endpoint that belongs to one page of the portal, and
// the roles that page is for. The same sets drive the menu (portal/js/app.js
// NAV); a role without the page does not get its data by typing the address.
type pagePolicy struct {
	pattern *regexp.Regexp
	roles   []string
	page    string
}

var (
	intakePageRoles       = []string{"CASE_MANAGER", "ARBITRATOR", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole}
	deliverablesPageRoles = []string{"CASE_MANAGER", "ARBITRATOR", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole}
	voicePageRoles        = []string{"CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR", serviceRole}
)

var pagePolicies = []pagePolicy{
	{regexp.MustCompile(`^/intake$`), intakePageRoles, "Intake"},
	{regexp.MustCompile(`^/deliverables$`), deliverablesPageRoles, "Deliverables"},
	{regexp.MustCompile(`^/voice/(intake|logs)$`), voicePageRoles, "Voice console"},
}

// filingDetailKeys are the parts of a case's details its filer supplied or
// may know. What the office adds while working the case -- the dispute
// summary, eligibility conflicts, fraud-waste-abuse flags, how the provider
// type was worked out -- is left out of what an outside filer receives.
var filingDetailKeys = []string{
	"filed_by_tpa", "filing_notes", "filing_party_type", "initiating_party", "initiating_party_contact",
	"requester_contact_name", "requester_email", "requester_org", "provider_org", "health_plan", "disputed_issue",
}

func filingDetails(details map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range filingDetailKeys {
		if v, ok := details[k]; ok {
			out[k] = v
		}
	}
	return out
}

// accessDecision is the pure core of the guard: given who is asking, the
// method and the path below /v1/tenants/{tenant}, it returns
//
//	"allow"      carry on to the handler (which applies its own checks)
//	"own-case"   allow only if the caller filed the case named in caseID
//	"deny"       refuse, with reason
//
// Pure, so every rule above is tested without a database.
func accessDecision(p principal, method, rel string) (decision, caseID, reason string) {
	if isExternalOnly(p) {
		for _, re := range outsideOpenPaths(p) {
			if re.MatchString(rel) {
				return "allow", "", ""
			}
		}
		if m := tpaOwnCasePaths.FindStringSubmatch(rel); m != nil && method == http.MethodGet {
			return "own-case", m[1], ""
		}
		return "deny", "", "this account can only use its own filing area"
	}
	if method == http.MethodGet {
		for _, pol := range pagePolicies {
			if pol.pattern.MatchString(rel) && !hasAnyRole(p, pol.roles...) {
				return "deny", "", "your role does not have the " + pol.page + " page"
			}
		}
	}
	return "allow", "", ""
}

// accessGuard applies accessDecision to every tenant-scoped request. It runs
// after authentication and the tenant check, and before the handler.
func (s *server) accessGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.Context().Value(ctxPrincipal{}).(principal)
		tenant := r.Context().Value(ctxTenant{}).(string)
		rel := r.URL.Path
		if i := strings.Index(rel, "/v1/tenants/"); i >= 0 {
			rest := rel[i+len("/v1/tenants/"):]
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				rel = rest[j:]
			} else {
				rel = ""
			}
		}
		decision, caseID, reason := accessDecision(p, r.Method, strings.TrimRight(rel, "/"))
		switch decision {
		case "allow":
			next.ServeHTTP(w, r)
		case "own-case":
			var mine bool
			_ = s.db.QueryRow(r.Context(), `
				SELECT EXISTS(SELECT 1 FROM public.case_origin o
				              JOIN public.tpa_users u ON u.tpa_id = o.tpa_id AND u.tenant = o.tenant
				              WHERE o.tenant=$1 AND o.case_id::text=$2 AND u.user_sub=$3)
				    OR EXISTS(SELECT 1 FROM public.case_origin o
				              JOIN public.health_plan_users u ON u.plan_id = o.health_plan_id AND u.tenant = o.tenant
				              WHERE o.tenant=$1 AND o.case_id::text=$2 AND u.user_sub=$3)`,
				tenant, caseID, p.Subject).Scan(&mine)
			if !mine {
				// Not found, not forbidden: an outside account must not learn which case ids exist.
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			next.ServeHTTP(w, r)
		default:
			s.logAudit(r.Context(), tenant, "", "ACCESS_DENIED", map[string]any{
				"by": p.Subject, "method": r.Method, "path": rel, "reason": reason})
			http.Error(w, fmt.Sprintf(`{"error":%q}`, "forbidden: "+reason), http.StatusForbidden)
		}
	})
}
