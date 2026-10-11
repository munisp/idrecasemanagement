package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lestrrat-go/jwx/v2/jwk"
)

// "Reviewer only reviews" follow-up: CASE_MANAGER keeps every DECIDE/
// ADVANCE/UPDATE action on an existing record (advanceIntake, qaDecide,
// convertLead, completeTask, addNote, submitDeliverable, assign, escalate,
// decide) but loses the ability to CREATE a new top-level record. PM picks
// up the create-side duties that moved off CASE_MANAGER. Pins the exact
// gate each affected handler now enforces.
func TestReviewerOnlyReviewsRoleFloors(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		gate  []string
		want  bool
	}{
		// createIntake: the SOP has the Reviewer send the submission
		// instructions, which is what opening an intake does.
		{"createIntake: PM allowed", []string{"PM"}, []string{"CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"createIntake: CASE_MANAGER allowed", []string{"CASE_MANAGER"}, []string{"CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"createIntake: CODER rejected", []string{"CODER"}, []string{"CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		// bulkIntake: same policy, in lockstep with createIntake.
		{"bulkIntake: PM allowed", []string{"PM"}, []string{"BULK_SUBMITTER", "TPA", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"bulkIntake: CASE_MANAGER allowed", []string{"CASE_MANAGER"}, []string{"BULK_SUBMITTER", "TPA", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		// issueInvoice / assignCase: the PM's, per the SOP roles table.
		{"issueInvoice: PM allowed", []string{"PM"}, []string{"PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"issueInvoice: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		{"assignCase: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		// requestAdhocDeliverable: requesting a NEW ad hoc deliverable --
		// submitDeliverable (fulfilling an existing one) is untouched.
		{"requestAdhocDeliverable: PM allowed", []string{"PM"}, []string{"PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"requestAdhocDeliverable: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
	}
	for _, c := range cases {
		got := hasAnyRole(principal{Roles: c.roles}, c.gate...)
		if got != c.want {
			t.Errorf("%s: roles=%v gate=%v: got allowed=%v, want %v", c.name, c.roles, c.gate, got, c.want)
		}
	}

	// createAccount/createContact/createTask: CASE_MANAGER is excluded by an
	// inline check layered ON TOP of crmStaffRoles (not removed from the
	// shared var itself, since reviewer keeps every other crmStaffRoles
	// action -- convertLead/completeTask/addNote/listNotes). Mirrors each
	// handler's own two-step gate exactly.
	crmCreateAllowed := func(roles []string) bool {
		p := principal{Roles: roles}
		return hasAnyRole(p, crmStaffRoles...) && !hasRole(p, "CASE_MANAGER")
	}
	crmCreateCases := []struct {
		name  string
		roles []string
		want  bool
	}{
		{"crmCreate: PM allowed", []string{"PM"}, true},
		{"crmCreate: ARBITRATOR allowed", []string{"ARBITRATOR"}, true},
		{"crmCreate: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, false},
		{"crmCreate: PARTY rejected (not even in crmStaffRoles)", []string{"PARTY"}, false},
	}
	for _, c := range crmCreateCases {
		if got := crmCreateAllowed(c.roles); got != c.want {
			t.Errorf("%s: roles=%v: got allowed=%v, want %v", c.name, c.roles, got, c.want)
		}
	}

	// submitApplication/claimTPA: open to any authenticated tenant member
	// EXCEPT CASE_MANAGER (an inline exclusion, not a positive role list) --
	// mirror that shape exactly rather than a gate array.
	notCaseManager := func(roles []string) bool { return !hasRole(principal{Roles: roles}, "CASE_MANAGER") }
	openExceptReviewerCases := []struct {
		name  string
		roles []string
		want  bool
	}{
		{"submitApplication/claimTPA: PM allowed", []string{"PM"}, true},
		{"submitApplication/claimTPA: PARTY allowed", []string{"PARTY"}, true},
		{"submitApplication/claimTPA: TPA allowed", []string{"TPA"}, true},
		{"submitApplication/claimTPA: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, false},
	}
	for _, c := range openExceptReviewerCases {
		if got := notCaseManager(c.roles); got != c.want {
			t.Errorf("%s: roles=%v: got allowed=%v, want %v", c.name, c.roles, got, c.want)
		}
	}

	// opsRoles: reviewer loses the Ops dashboard entirely (tenant-wide
	// operational/financial KPIs, not dispute-review content).
	opsCases := []struct {
		name  string
		roles []string
		want  bool
	}{
		{"opsRoles: PM allowed", []string{"PM"}, true},
		{"opsRoles: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, false},
	}
	for _, c := range opsCases {
		if got := hasAnyRole(principal{Roles: c.roles}, opsRoles...); got != c.want {
			t.Errorf("%s: roles=%v: got allowed=%v, want %v", c.name, c.roles, got, c.want)
		}
	}

	// adminTPAInviteCode/adminSetTPAStatus: the TPA oversight list's action
	// buttons (mint code / act as / suspend) -- reviewer keeps the
	// read-only oversight table (adminListTPAs, untouched) but not these.
	tpaAdminActionCases := []struct {
		name  string
		roles []string
		want  bool
	}{
		{"tpaAdminAction: FEDERAL_ADMIN allowed", []string{"FEDERAL_ADMIN"}, true},
		{"tpaAdminAction: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, false},
		{"tpaAdminAction: PM rejected (never had this)", []string{"PM"}, false},
	}
	for _, c := range tpaAdminActionCases {
		if got := hasAnyRole(principal{Roles: c.roles}, "FEDERAL_ADMIN", "PLATFORM_ADMIN"); got != c.want {
			t.Errorf("%s: roles=%v: got allowed=%v, want %v", c.name, c.roles, got, c.want)
		}
	}
}

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
//
// CASE_MANAGER was removed from this gate in the access-control audit
// (AC-5 separation of duties): the role that decides a dispute's
// eligibility/outcome shouldn't also post the ledger transfer that moves
// its money -- same floor settleInvoice/clearCheck already used.
func TestFeeTransferRoleFloorRejectsWrongRole(t *testing.T) {
	cases := []struct {
		roles []string
		want  bool
	}{
		{[]string{"ARBITRATOR"}, false},
		{[]string{"PARTY"}, false},
		{[]string{"CASE_MANAGER"}, false},
		{[]string{"FINANCE"}, true},
		{[]string{"FEDERAL_ADMIN"}, true},
		{[]string{"PLATFORM_ADMIN"}, true},
	}
	for _, c := range cases {
		got := hasAnyRole(principal{Roles: c.roles}, "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")
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
		// settleInvoice: permanent policy is FINANCE/admins only -- not
		// CASE_MANAGER (separation of duties between the reviewer who drafts
		// the case and whoever moves money), and PM was removed in the
		// access-control audit for the same reason (PM holds case-adjacent
		// QA/review powers, so shouldn't also settle the resulting money).
		{"settle: FINANCE allowed", []string{"FINANCE"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, true},
		{"settle: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, false},
		{"settle: PM rejected", []string{"PM"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, false},
		// recordOptOut: permanent policy is ATTORNEY only, per the source
		// docs ("Attorney decides if the plan may opt out") -- not even
		// CASE_MANAGER.
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
		{[]string{"CASE_MANAGER"}, false},
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
		// ledgerBalances: permanent policy is reconciliation job + the humans
		// who'd act on a balance -- not CASE_MANAGER.
		{"ledger: FINANCE allowed", []string{"FINANCE"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"ledger: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		// initiateCase: who may open a new federal NSA case record. PARTY is
		// allowed -- the portal's "New dispute" nav + form is self-service
		// federal NSA filing, not staff-only (confirmed against the UI's own
		// nav gate: has("PARTY", "PM")). ARBITRATOR was removed in the
		// access-control audit (AC-5): the role that decides a dispute
		// shouldn't also be able to file one -- undermines impartiality
		// regardless of whether it's ever misused. CASE_MANAGER removed (PM
		// added) per the "reviewer only reviews" follow-up: filing is a
		// creation action, not a review one.
		{"initiate: ARBITRATOR rejected", []string{"ARBITRATOR"}, []string{"PARTY", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		{"initiate: PARTY allowed", []string{"PARTY"}, []string{"PARTY", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"initiate: PM allowed", []string{"PM"}, []string{"PARTY", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"initiate: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"PARTY", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		{"initiate: FINANCE rejected", []string{"FINANCE"}, []string{"PARTY", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		// assignCase / bulkCases: reassignment is a management decision, not
		// routine casework -- narrower than the broad case-staff gate.
		{"assign: PM allowed", []string{"PM"}, []string{"PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"assign: ATTORNEY rejected", []string{"ATTORNEY"}, []string{"PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		{"bulk: CASE_MANAGER allowed", []string{"CASE_MANAGER"}, []string{"CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, true},
		{"bulk: CODER rejected", []string{"CODER"}, []string{"CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, false},
		// grabNext: whoever actually works a queued case, not PM/FINANCE oversight.
		{"grabNext: ARBITRATOR allowed", []string{"ARBITRATOR"}, []string{"CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, true},
		{"grabNext: FINANCE rejected", []string{"FINANCE"}, []string{"CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, false},
		// uploadCheck / createCheckout: staff handling a payment instrument.
		{"uploadCheck: FINANCE allowed", []string{"FINANCE"}, []string{"CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"uploadCheck: NURSE_PHYSICIAN rejected", []string{"NURSE_PHYSICIAN"}, []string{"CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		// clearCheck: the ONLY path that turns a check image into settled
		// money -- permanent policy is the same floor as settleInvoice,
		// CASE_MANAGER and (per the access-control audit) PM both excluded.
		{"clearCheck: FINANCE allowed", []string{"FINANCE"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"clearCheck: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		{"clearCheck: PM rejected", []string{"PM"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		// matchCheck: same floor as clearCheck (access-control audit) -- it
		// determines which case/invoice real money attaches to.
		{"matchCheck: FINANCE allowed", []string{"FINANCE"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, true},
		{"matchCheck: PM rejected", []string{"PM"}, []string{"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}, false},
		// transitionInvoice "approve": converts a draft service invoice into
		// a payable obligation. CASE_MANAGER removed in the access-control
		// audit; PM still allowed (program-management oversight, one step
		// short of actually settling money).
		{"invoice approve: PM allowed", []string{"PM"}, []string{"PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, true},
		{"invoice approve: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, []string{"PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, false},
		// caseStaffRoles: generateLetter/requestLetterGen -- the portal only
		// ever shows these buttons to CASE_MANAGER/FEDERAL_ADMIN, so this
		// narrower set (not crmStaffRoles) is a safe superset of the UI.
		{"caseStaff: CODER allowed", []string{"CODER"}, caseStaffRoles, true},
		{"caseStaff: NURSE_PHYSICIAN allowed", []string{"NURSE_PHYSICIAN"}, caseStaffRoles, true},
		{"caseStaff: DOCTOR satisfies clinical grant", []string{"DOCTOR"}, caseStaffRoles, true},
		{"caseStaff: NURSE satisfies clinical grant", []string{"NURSE"}, caseStaffRoles, true},
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

// Live role testing (a human driving the real portal as each seeded user)
// found what the previous two audits missed: read-only endpoints serving
// the Financials, QA Gate, and Reports pages had zero backend gate at all,
// so a narrow specialist role with no business reason to see any of it --
// ARBITRATOR, CODER, NURSE_PHYSICIAN, ATTORNEY -- could read check images'
// real bank routing/account numbers, invoices, payments, and pending QA
// correspondence drafts just by typing the URL directly; only the nav hid
// the page from them, nothing on the server did. Pins the gate each got.
func TestThirdRoleAuditFloors_ReadEndpoints(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		gate  []string
		want  bool
	}{
		// financialReadRoles: listChecks/financialReport/receivablesReport/
		// listInvoices/listPayments. Matches the Financials nav gate exactly.
		{"financialRead: STATE_AUDITOR allowed (read-only oversight)", []string{"STATE_AUDITOR"}, financialReadRoles, true},
		{"financialRead: ARBITRATOR rejected", []string{"ARBITRATOR"}, financialReadRoles, false},
		{"financialRead: CODER rejected", []string{"CODER"}, financialReadRoles, false},
		{"financialRead: NURSE_PHYSICIAN rejected", []string{"NURSE_PHYSICIAN"}, financialReadRoles, false},
		{"financialRead: ATTORNEY rejected", []string{"ATTORNEY"}, financialReadRoles, false},
		// qaReadRoles: qaQueue/qaGet. Matches the QA Gate nav gate exactly.
		{"qaRead: ARBITRATOR allowed", []string{"ARBITRATOR"}, qaReadRoles, true},
		{"qaRead: ATTORNEY allowed", []string{"ATTORNEY"}, qaReadRoles, true},
		{"qaRead: CODER rejected", []string{"CODER"}, qaReadRoles, false},
		{"qaRead: NURSE_PHYSICIAN rejected", []string{"NURSE_PHYSICIAN"}, qaReadRoles, false},
		{"qaRead: FINANCE rejected", []string{"FINANCE"}, qaReadRoles, false},
		// reportsReadRoles: summaryReport/slaReport. Matches the Reports nav
		// gate exactly -- notably FINANCE and ARBITRATOR are NOT here, unlike
		// financialReadRoles, because the Reports nav never admitted them.
		{"reportsRead: PM allowed", []string{"PM"}, reportsReadRoles, true},
		{"reportsRead: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, reportsReadRoles, false},
		{"reportsRead: FINANCE rejected", []string{"FINANCE"}, reportsReadRoles, false},
		{"reportsRead: ARBITRATOR rejected", []string{"ARBITRATOR"}, reportsReadRoles, false},
		// opsRoles (opsDashboard, also backs the Reports -> Trends tab): PM
		// added so Trends doesn't 403 for a role the Reports nav itself
		// grants access to (confirmed live: it did, and the frontend just
		// hung on "Loading..." forever instead of showing the error).
		{"opsRoles: PM allowed", []string{"PM"}, opsRoles, true},
		{"opsRoles: CODER rejected (no Reports nav access at all)", []string{"CODER"}, opsRoles, false},
	}
	for _, c := range cases {
		got := hasAnyRole(principal{Roles: c.roles}, c.gate...)
		if got != c.want {
			t.Errorf("%s: roles=%v: got allowed=%v, want %v", c.name, c.roles, got, c.want)
		}
	}
}

// Access-control follow-up: the original audit restricted CASE_MANAGER's
// WRITE access to money-moving actions but left its READ access to the
// standalone finance screens (Financials, Billing, AR/AP, Reconciliation,
// Checks) untouched -- live testing found a reviewer-role account could
// still see all of them, which was the actual original ask ("reviewer
// shouldn't see finance"), not just a write restriction. These are
// separate screens from a case's own embedded financial record
// (caseFinancials), which CASE_MANAGER still needs and still has.
func TestFourthRoleAuditFloors_FinanceVisibility(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		gate  []string
		want  bool
	}{
		{"financialRead: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, financialReadRoles, false},
		{"financialRead: PM still allowed", []string{"PM"}, financialReadRoles, true},
		{"arapRead: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, arapReadRoles, false},
		{"arapRead: PM still allowed", []string{"PM"}, arapReadRoles, true},
		{"reconRead: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, reconReadRoles, false},
		{"reconRead: PM still allowed", []string{"PM"}, reconReadRoles, true},
		{"billingView: CASE_MANAGER rejected", []string{"CASE_MANAGER"}, billingViewRoles, false},
		{"billingView: PM still allowed", []string{"PM"}, billingViewRoles, true},
	}
	for _, c := range cases {
		got := hasAnyRole(principal{Roles: c.roles}, c.gate...)
		if got != c.want {
			t.Errorf("%s: roles=%v: got allowed=%v, want %v", c.name, c.roles, got, c.want)
		}
	}
	// uploadCheck has its own inline gate rather than a shared role list.
	if hasAnyRole(principal{Roles: []string{"CASE_MANAGER"}}, "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		t.Error("uploadCheck gate: CASE_MANAGER should be rejected")
	}
	if !hasAnyRole(principal{Roles: []string{"PM"}}, "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		t.Error("uploadCheck gate: PM should still be allowed")
	}
}

// createFederalAdmin / createTenantStaff: there was previously no way to
// create a FEDERAL_ADMIN/PLATFORM_ADMIN account at all through the app
// (federal.admin itself was hand-seeded directly in the Keycloak realm),
// and no way to add a second staff member to an already-existing tenant
// without resubmitting a whole tenant-creation request. Scoped with the
// user: either federal tier can mint more of either federal role; a PM
// may add tenant staff only to their own tenant. CASE_MANAGER was removed
// from this (and Reports, Team time) per the same access-control
// follow-up as financialReadRoles -- reviewer loses tenant staff admin
// entirely, not just a subset.
func TestAdminCreationRoleFloors(t *testing.T) {
	t.Run("createFederalAdmin", func(t *testing.T) {
		cases := []struct {
			name  string
			roles []string
			want  bool
		}{
			{"PLATFORM_ADMIN allowed", []string{"PLATFORM_ADMIN"}, true},
			{"FEDERAL_ADMIN allowed", []string{"FEDERAL_ADMIN"}, true},
			{"CASE_MANAGER rejected", []string{"CASE_MANAGER"}, false},
			{"PM rejected", []string{"PM"}, false},
		}
		for _, c := range cases {
			got := hasAnyRole(principal{Roles: c.roles}, "FEDERAL_ADMIN", "PLATFORM_ADMIN")
			if got != c.want {
				t.Errorf("%s: roles=%v: got allowed=%v, want %v", c.name, c.roles, got, c.want)
			}
		}
	})

	t.Run("createTenantStaff tenant-scoping", func(t *testing.T) {
		// Mirrors createTenantStaff's own gate logic (admins bypass the
		// tenant check; PM must match the target tenant).
		allowed := func(roles []string, callerTenant, targetTenant string) bool {
			p := principal{Roles: roles}
			if hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
				return true
			}
			return hasRole(p, "PM") && targetTenant == callerTenant
		}
		cases := []struct {
			name         string
			roles        []string
			callerTenant string
			targetTenant string
			want         bool
		}{
			{"FEDERAL_ADMIN can target any tenant", []string{"FEDERAL_ADMIN"}, "tx", "fl", true},
			{"PM can target own tenant", []string{"PM"}, "fl", "fl", true},
			{"PM rejected on a different tenant", []string{"PM"}, "tx", "fl", false},
			{"CASE_MANAGER rejected even on own tenant", []string{"CASE_MANAGER"}, "fl", "fl", false},
			{"ARBITRATOR rejected even on own tenant", []string{"ARBITRATOR"}, "tx", "tx", false},
		}
		for _, c := range cases {
			got := allowed(c.roles, c.callerTenant, c.targetTenant)
			if got != c.want {
				t.Errorf("%s: roles=%v caller=%s target=%s: got allowed=%v, want %v",
					c.name, c.roles, c.callerTenant, c.targetTenant, got, c.want)
			}
		}
	})
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

// TENANT_ADMIN is a different role from reviewer (CASE_MANAGER): it reaches
// CASE_MANAGER-gated handlers through hasRole's quirk, so every gate that
// dropped or excluded CASE_MANAGER has to name or exempt TENANT_ADMIN itself.
func TestTenantAdminKeepsWhatReviewerLost(t *testing.T) {
	ta := principal{Roles: []string{"TENANT_ADMIN"}}
	rev := principal{Roles: []string{"CASE_MANAGER"}}
	lists := map[string][]string{
		"reportsReadRoles":        reportsReadRoles,
		"opsRoles":                opsRoles,
		"timeReport":              {"PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN", serviceRole},
		"tenantStaff":             {"PM", "TENANT_ADMIN"},
		"initiateCase":            {"PARTY", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN", serviceRole},
		"requestAdhocDeliverable": {"PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN", serviceRole},
		"tpaAdminAction":          {"FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN"},
	}
	for name, gate := range lists {
		if !hasAnyRole(ta, gate...) {
			t.Errorf("%s: TENANT_ADMIN must be allowed", name)
		}
		if hasAnyRole(rev, gate...) {
			t.Errorf("%s: CASE_MANAGER must stay rejected", name)
		}
	}
	// Inline exclusions (createAccount/Contact/Task, submitApplication, claimTPA).
	excluded := func(p principal) bool { return hasRole(p, "CASE_MANAGER") && !hasRole(p, "TENANT_ADMIN") }
	if excluded(ta) {
		t.Error("inline reviewer exclusion must not catch TENANT_ADMIN")
	}
	if !excluded(rev) {
		t.Error("inline reviewer exclusion must still catch CASE_MANAGER")
	}
}

// TENANT_ADMIN passes any gate that admits a tenant staff role, and nothing
// else: not platform-only gates, not identity checks on external roles.
func TestTenantAdminIsTenantSuperAdmin(t *testing.T) {
	ta := principal{Roles: []string{"TENANT_ADMIN"}}
	allowed := map[string][]string{
		"financialReadRoles": financialReadRoles,
		"billingViewRoles":   billingViewRoles,
		"arapReadRoles":      arapReadRoles,
		"reconReadRoles":     reconReadRoles,
		"settle (FINANCE)":   {"FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
		"opt-out (ATTORNEY)": {"ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN"},
		"crmStaffRoles":      crmStaffRoles,
	}
	for name, gate := range allowed {
		if !hasAnyRole(ta, gate...) {
			t.Errorf("%s: TENANT_ADMIN must be allowed", name)
		}
	}
	denied := map[string][]string{
		"createTenant / federal admins": {"FEDERAL_ADMIN", "PLATFORM_ADMIN"},
		"worker-token only":             {serviceRole},
		"is-a-TPA identity check":       {"TPA"},
		"external roles only":           {"PARTY", "BULK_SUBMITTER", "STATE_AUDITOR"},
	}
	for name, gate := range denied {
		if hasAnyRole(ta, gate...) {
			t.Errorf("%s: TENANT_ADMIN must NOT be allowed", name)
		}
	}
	// Other roles are unaffected by the rule.
	if hasAnyRole(principal{Roles: []string{"CASE_MANAGER"}}, financialReadRoles...) {
		t.Error("CASE_MANAGER must stay out of financialReadRoles")
	}
}
