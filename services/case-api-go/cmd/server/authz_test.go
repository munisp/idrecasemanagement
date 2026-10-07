package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lestrrat-go/jwx/v2/jwk"
)

func TestHasAnyRole(t *testing.T) {
	p := principal{Roles: []string{"ARBITRATOR"}}
	if hasAnyRole(p, "FINANCE", "CASE_MANAGER") {
		t.Fatal("ARBITRATOR must not match a FINANCE/CASE_MANAGER-only gate")
	}
	if !hasAnyRole(p, "FINANCE", "ARBITRATOR") {
		t.Fatal("ARBITRATOR must match when it's one of the allowed roles")
	}
}

// Reproduces the live finding (sibling deployment, main branch, same code):
// before the RBAC floor existed, an ARBITRATOR token reached
// postFeeTransfer/escalateCase/reveal with no role check at all (Permify,
// the only other gate, allows everyone when undeployed).
func TestFeeTransferRoleFloorRejectsWrongRole(t *testing.T) {
	cases := []struct {
		roles []string
		want  bool
	}{
		{[]string{"ARBITRATOR"}, false},
		{[]string{"PARTY"}, false},
		{[]string{"FINANCE"}, true},
		{[]string{"CASE_MANAGER"}, true},
		{[]string{"FEDERAL_ADMIN"}, true},
		{[]string{"PLATFORM_ADMIN"}, true},
	}
	for _, c := range cases {
		got := hasAnyRole(principal{Roles: c.roles}, "FINANCE", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN")
		if got != c.want {
			t.Errorf("roles=%v: got allowed=%v, want %v", c.roles, got, c.want)
		}
	}
}

// Reproduces the second live finding: WORKER_TOKEN was never validated
// anywhere, so the Temporal worker's own service-to-service escalate call
// always 401'd. A valid worker token must authenticate as serviceRole; any
// other value must fall through to (and fail) ordinary JWT parsing.
func TestWorkerTokenAuthenticates(t *testing.T) {
	keys := jwk.NewSet()
	a := &authn{keys: keys, issuer: "https://issuer.test/realms/idre", workerToken: "the-worker-secret"}

	var gotPrincipal principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrincipal = r.Context().Value(ctxPrincipal{}).(principal)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/tx/cases/x/escalate", nil)
	req.Header.Set("Authorization", "Bearer the-worker-secret")
	rec := httptest.NewRecorder()
	a.middleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("worker token must authenticate, got status %d body %s", rec.Code, rec.Body.String())
	}
	if !hasRole(gotPrincipal, serviceRole) {
		t.Fatalf("worker token must grant %s, got roles %v", serviceRole, gotPrincipal.Roles)
	}
}

func TestWrongTokenFallsThroughToJWTAndFails(t *testing.T) {
	keys := jwk.NewSet()
	a := &authn{keys: keys, issuer: "https://issuer.test/realms/idre", workerToken: "the-worker-secret"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/tx/cases", nil)
	req.Header.Set("Authorization", "Bearer not-the-worker-secret-and-not-a-jwt")
	rec := httptest.NewRecorder()
	a.middleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a non-matching, non-JWT token must 401, got %d", rec.Code)
	}
}

// Reproduces the finding from scoping FL AHCA as a second program: every
// program-layer endpoint (checkEligibility, setDualStatus, draftCorrespondence,
// qaDecision, issueInvoice, settleInvoice, createIntake, advanceIntake,
// recordOptOut, createShareLink, escalationTrigger) had zero role check at
// all -- reachable by any authenticated member of the tenant, any role.
// These table-driven cases pin the exact gate each handler now enforces.
func TestProgramLayerRoleFloors(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		gate  []string
		want  bool
	}{
		// setDualStatus / draftCorrespondence / createShareLink / importClaims /
		// submitDeliverable: broad case-staff gate.
		{"case staff: CASE_MANAGER allowed", []string{"CASE_MANAGER"}, []string{"CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, true},
		{"case staff: PM allowed", []string{"PM"}, []string{"CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, true},
		{"case staff: PARTY rejected", []string{"PARTY"}, []string{"CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, false},
		// checkEligibility: CASE_MANAGER/ATTORNEY/admins only -- not PM/CODER.
		{"eligibility: ATTORNEY allowed", []string{"ATTORNEY"}, []string{"CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, true},
		{"eligibility: PM rejected", []string{"PM"}, []string{"CASE_MANAGER", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, false},
		// settleInvoice: PM/FINANCE only -- not CASE_MANAGER (separation of duties
		// between the reviewer who drafts the case and whoever moves money).
		{"settle: FINANCE allowed", []string{"FINANCE"}, []string{"PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, true},
		{"settle: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, false},
		// recordOptOut: ATTORNEY only, per the source docs ("Attorney decides
		// if the plan may opt out") -- not even CASE_MANAGER.
		{"opt-out: ATTORNEY allowed", []string{"ATTORNEY"}, []string{"ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, true},
		{"opt-out: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, false},
	}
	for _, c := range cases {
		got := hasAnyRole(principal{Roles: c.roles}, c.gate...)
		if got != c.want {
			t.Errorf("%s: roles=%v gate=%v: got allowed=%v, want %v", c.name, c.roles, c.gate, got, c.want)
		}
	}
}

// qaDecision's QA-role gate is read straight from program_rules config (e.g.
// "ATTORNEY" on FL AHCA's dismissal template) rather than hardcoded per
// program -- this pins that a CODER token cannot approve an ATTORNEY-gated
// draft, and that ATTORNEY/FEDERAL_ADMIN/PLATFORM_ADMIN can.
func TestQaDecisionRoleMatchesConfiguredQARole(t *testing.T) {
	qaRole := "ATTORNEY"
	cases := []struct {
		roles []string
		want  bool
	}{
		{[]string{"CODER"}, false},
		{[]string{"PM"}, false},
		{[]string{"ATTORNEY"}, true},
		{[]string{"FEDERAL_ADMIN"}, true},
		{[]string{"PLATFORM_ADMIN"}, true},
	}
	for _, c := range cases {
		got := hasAnyRole(principal{Roles: c.roles}, qaRole, "FEDERAL_ADMIN", "PLATFORM_ADMIN")
		if got != c.want {
			t.Errorf("roles=%v against qa_role=%s: got allowed=%v, want %v", c.roles, qaRole, got, c.want)
		}
	}
}

// A broader sweep (every registered route checked against its handler body
// for a role gate) turned up a second round of findings beyond the FL AHCA
// scoping pass above: two endpoints whose OWN comments documented a
// worker-token-only intent that nothing enforced (checkResult, ledgerBalances
// -- either let any tenant member fabricate check-clearing data or read every
// party's ledger balance), plus a dozen case-mutation/CRM/graph-admin
// endpoints reachable by any authenticated tenant member regardless of role.
// These pin the gate each one got.
func TestSecondRoleAuditFloors(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		gate  []string
		want  bool
	}{
		// checkResult: worker-token only, not even admins -- this is pipeline-
		// reported OCR extraction data, never a human-supplied value.
		{"checkResult: serviceRole allowed", []string{serviceRole}, []string{serviceRole}, true},
		{"checkResult: PLATFORM_ADMIN rejected", []string{"PLATFORM_ADMIN"}, []string{serviceRole}, false},
		{"checkResult: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{serviceRole}, false},
		// ledgerBalances: reconciliation job + the humans who'd act on a balance.
		{"ledger: FINANCE allowed", []string{"FINANCE"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"ledger: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		// initiateCase: who may open a new federal NSA case record. PARTY is
		// allowed -- the portal's "New dispute" nav + form is self-service
		// federal NSA filing, not staff-only (confirmed against the UI's own
		// nav gate: has("PARTY", "CASE_MANAGER")).
		{"initiate: ARBITRATOR allowed", []string{"ARBITRATOR"}, []string{"PARTY", "CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"initiate: PARTY allowed", []string{"PARTY"}, []string{"PARTY", "CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"initiate: FINANCE rejected", []string{"FINANCE"}, []string{"PARTY", "CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		// assignCase / bulkCases: reassignment is a management decision, not
		// routine casework -- narrower than the broad case-staff gate.
		{"assign: PM allowed", []string{"PM"}, []string{"CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"assign: ATTORNEY rejected", []string{"ATTORNEY"}, []string{"CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		{"bulk: CASE_MANAGER allowed", []string{"CASE_MANAGER"}, []string{"CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, true},
		{"bulk: CODER rejected", []string{"CODER"}, []string{"CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, false},
		// grabNext: whoever actually works a queued case, not PM/FINANCE oversight.
		{"grabNext: ARBITRATOR allowed", []string{"ARBITRATOR"}, []string{"CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, true},
		{"grabNext: FINANCE rejected", []string{"FINANCE"}, []string{"CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, false},
		// uploadCheck / createCheckout: staff handling a payment instrument.
		{"uploadCheck: FINANCE allowed", []string{"FINANCE"}, []string{"CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"uploadCheck: NURSE_PHYSICIAN rejected", []string{"NURSE_PHYSICIAN"}, []string{"CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		// clearCheck: the ONLY path that turns a check image into settled
		// money -- same floor as settleInvoice, CASE_MANAGER excluded.
		{"clearCheck: FINANCE allowed", []string{"FINANCE"}, []string{"PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"clearCheck: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		// caseStaffRoles: generateLetter/requestLetterGen -- the portal only
		// ever shows these buttons to CASE_MANAGER/FEDERAL_ADMIN, so this
		// narrower set (not crmStaffRoles) is a safe superset of the UI.
		{"caseStaff: CODER allowed", []string{"CODER"}, caseStaffRoles, true},
		{"caseStaff: NURSE_PHYSICIAN allowed", []string{"NURSE_PHYSICIAN"}, caseStaffRoles, true},
		{"caseStaff: PARTY rejected", []string{"PARTY"}, caseStaffRoles, false},
		{"caseStaff: FINANCE rejected", []string{"FINANCE"}, caseStaffRoles, false},
		// crmStaffRoles: relateCases/createAccount/createContact/createTask/
		// completeTask/convertLead/addNote -- the portal's CRM nav and the
		// case-detail GNN "confirm link" button render with NO role
		// restriction at all, so ARBITRATOR/FINANCE (real, active roles)
		// must still reach these, unlike the narrower caseStaffRoles.
		{"crmStaff: ARBITRATOR allowed", []string{"ARBITRATOR"}, crmStaffRoles, true},
		{"crmStaff: FINANCE allowed", []string{"FINANCE"}, crmStaffRoles, true},
		{"crmStaff: CODER allowed", []string{"CODER"}, crmStaffRoles, true},
		{"crmStaff: PARTY rejected", []string{"PARTY"}, crmStaffRoles, false},
		// checkItem: caseStaffRoles plus ARBITRATOR specifically (the
		// checklist UI has always shown its check button to ARBITRATOR,
		// which caseStaffRoles alone does not cover).
		{"checkItem: ARBITRATOR allowed", []string{"ARBITRATOR"}, append(append([]string{}, caseStaffRoles...), "ARBITRATOR"), true},
		{"checkItem: PARTY rejected", []string{"PARTY"}, append(append([]string{}, caseStaffRoles...), "ARBITRATOR"), false},
		// graphAdminRoles: full resync/lakehouse export/retrain are expensive,
		// uncapped backend jobs -- admin-only, unlike graphAsk/graphFeedback
		// which stay open to any case staff.
		{"graphAdmin: PLATFORM_ADMIN allowed", []string{"PLATFORM_ADMIN"}, graphAdminRoles, true},
		{"graphAdmin: PM rejected", []string{"PM"}, graphAdminRoles, false},
		{"graphAdmin: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, graphAdminRoles, false},
	}
	for _, c := range cases {
		got := hasAnyRole(principal{Roles: c.roles}, c.gate...)
		if got != c.want {
			t.Errorf("%s: roles=%v gate=%v: got allowed=%v, want %v", c.name, c.roles, c.gate, got, c.want)
		}
	}
}

// signalCase forwards whatever signal name the caller sends straight to the
// Temporal workflow, with no differentiation -- any authenticated tenant
// member could send SELECTION_FINALIZED or PAYMENT_RECORDED even though the
// portal only ever shows those buttons to CASE_MANAGER and FINANCE
// respectively. Pins the per-signal floor now enforced.
func TestSignalCaseRoleFloors(t *testing.T) {
	cases := []struct {
		name   string
		signal string
		roles  []string
		want   bool
	}{
		{"RESPONSE_FILED: PARTY allowed", "RESPONSE_FILED", []string{"PARTY"}, true},
		{"RESPONSE_FILED: FINANCE rejected", "RESPONSE_FILED", []string{"FINANCE"}, false},
		{"SELECTION_FINALIZED: CASE_MANAGER allowed", "SELECTION_FINALIZED", []string{"CASE_MANAGER"}, true},
		{"SELECTION_FINALIZED: PARTY rejected", "SELECTION_FINALIZED", []string{"PARTY"}, false},
		{"DETERMINATION_ISSUED: ARBITRATOR allowed", "DETERMINATION_ISSUED", []string{"ARBITRATOR"}, true},
		{"DETERMINATION_ISSUED: CASE_MANAGER rejected", "DETERMINATION_ISSUED", []string{"CASE_MANAGER"}, false},
		{"PAYMENT_RECORDED: FINANCE allowed", "PAYMENT_RECORDED", []string{"FINANCE"}, true},
		{"PAYMENT_RECORDED: ARBITRATOR rejected", "PAYMENT_RECORDED", []string{"ARBITRATOR"}, false},
		{"PLAN_OPT_OUT: ATTORNEY allowed", "PLAN_OPT_OUT", []string{"ATTORNEY"}, true},
		{"PLAN_OPT_OUT: CASE_MANAGER rejected", "PLAN_OPT_OUT", []string{"CASE_MANAGER"}, false},
		{"CODING_REVIEW_COMPLETE: CODER allowed", "CODING_REVIEW_COMPLETE", []string{"CODER"}, true},
		{"CODING_REVIEW_COMPLETE: NURSE_PHYSICIAN rejected", "CODING_REVIEW_COMPLETE", []string{"NURSE_PHYSICIAN"}, false},
		{"CLINICAL_REVIEW_COMPLETE: NURSE_PHYSICIAN allowed", "CLINICAL_REVIEW_COMPLETE", []string{"NURSE_PHYSICIAN"}, true},
		{"ATTORNEY_REVIEW_COMPLETE: ATTORNEY allowed", "ATTORNEY_REVIEW_COMPLETE", []string{"ATTORNEY"}, true},
		{"ATTORNEY_REVIEW_COMPLETE: CODER rejected", "ATTORNEY_REVIEW_COMPLETE", []string{"CODER"}, false},
		// unmapped signal (e.g. PACKET_COMPLETE) falls back to caseStaffRoles.
		{"unmapped signal: CASE_MANAGER allowed", "PACKET_COMPLETE", []string{"CASE_MANAGER"}, true},
		{"unmapped signal: PARTY rejected", "PACKET_COMPLETE", []string{"PARTY"}, false},
		// admins always allowed regardless of signal.
		{"any signal: PLATFORM_ADMIN allowed", "SELECTION_FINALIZED", []string{"PLATFORM_ADMIN"}, true},
	}
	for _, c := range cases {
		gate := signalRoleFloors[c.signal]
		if gate == nil {
			gate = caseStaffRoles
		}
		got := hasAnyRole(principal{Roles: c.roles}, append(append([]string{}, gate...), serviceRole)...)
		if got != c.want {
			t.Errorf("%s: signal=%s roles=%v: got allowed=%v, want %v", c.name, c.signal, c.roles, got, c.want)
		}
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if isUniqueViolation(nil) {
		t.Fatal("nil error is not a unique violation")
	}
	if isUniqueViolation(&pgconn.PgError{Code: "23503"}) { // foreign_key_violation
		t.Fatal("a different pg error code must not be treated as a unique violation")
	}
	if !isUniqueViolation(&pgconn.PgError{Code: "23505"}) { // unique_violation
		t.Fatal("pg error code 23505 must be recognized as a unique violation")
	}
}
