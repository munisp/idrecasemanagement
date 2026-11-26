// Author: Patrick Munis at newwave
package main

// Who is told what as a case moves (change log 2026-11-25 #14 and #24, "Notifications/Roles" and "Required Workflow
// Notifications"). The Admin (CASE_MANAGER) is the change log's Administrator. QA is its own role (QA); the program's
// templates still name the PM as their QA approver, so a QA approval step accepts QA or the PM, and "notify QA"
// reaches both (notifyQA) -- the PM keeps sight of everything, as the change log asks.
//
//   a. the Admin drafts the Acceptance / Plan Notification   -> QA (the template's QA role) + the PM, for sight
//   b. the Coder / Nurse finishes a review (the decision letter) -> the Attorney + the PM
//   c. the Attorney asks for revisions                           -> the Coder / Nurse;  approves -> QA (the PM)
//   d. QA sends a decision letter or the Final Order back        -> the Coder, the Nurse, the Attorney and the PM
//      QA passes it                                              -> the PM, the Coder, the Nurse and the Attorney
//      QA passes the Acceptance letter                           -> the PM (case acceptance)
//   e. the Final Order is released                               -> the case team and the PM
// plus: a case reaching coding review tells the Coder; a requested clinical review tells the Nurse / Physician;
// an eligibility problem (ineligible, AOR hold) tells the PM.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// decisionLetters: the documents the Attorney and QA review for the decision.
var decisionLetters = map[string]bool{
	"final_order_rationale": true, "final_order": true, "cover_letter_final_notice": true, "default_determination": true,
}

func (s *server) caseNumberOf(ctx context.Context, tenant, caseID string) string {
	var n string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT coalesce(case_number,'') FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&n)
	if n == "" {
		return caseID
	}
	return n
}

// Who hears a case's alerts (2026-11-25): the Tenant Admin, the Admin and the PM hear every one; the Coder, the
// Nurse Reviewer, the Attorney and QA hear a case's alerts only when they are on that case (case_helpers, set from
// the case's Assignment box). An alert for a specialist role nobody holds on the case reaches the three who hear
// everything, who can then assign someone -- except QA, which falls back to every QA until one is named (so a draft
// never waits unseen). Roles outside the case team (Finance) still hear by role.
var (
	alwaysAlertedRoles = []string{"TENANT_ADMIN", "CASE_MANAGER", "PM"}
	caseTeamRoles      = map[string]bool{"CODER": true, "NURSE_PHYSICIAN": true, "ATTORNEY": true, "QA": true, "ARBITRATOR": true}
	caseTeamRoleList   = []string{"CODER", "NURSE_PHYSICIAN", "ATTORNEY", "QA", "ARBITRATOR"}
)

// caseAlertTargets: the notification targets for an alert about caseID meant for roles. Pure apart from helpers
// (role -> the person on the case), so it is tested.
func caseAlertTargets(helpers map[string]string, roles ...string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, rl := range alwaysAlertedRoles {
		add("role:" + rl)
	}
	for _, role := range roles {
		if role == "DOCTOR" || role == "NURSE" {
			role = "NURSE_PHYSICIAN"
		}
		switch {
		case role == "" || contains(alwaysAlertedRoles, role):
		case caseTeamRoles[role]:
			if who := helpers[role]; who != "" {
				add(who)
			} else if role == "QA" {
				add("role:QA") // no QA named on the case yet: every QA hears it, so nothing waits unseen (2026-11-26)
			} // any other role with nobody named: the three above have it, and can name someone
		default:
			add("role:" + role)
		}
	}
	return out
}

// caseAlert sends one alert about a case to whoever should hear it (caseAlertTargets).
func (s *server) caseAlert(r *http.Request, tenant, caseID, typ, body, link string, roles ...string) {
	if link == "" {
		link = "#/cases/" + caseID
	}
	var helpers map[string]string
	if caseID != "" {
		helpers = s.caseHelpers(r.Context(), tenant, caseID)
	}
	for _, t := range caseAlertTargets(helpers, roles...) {
		s.notify(r, tenant, t, typ, body, link)
	}
}

// caseAlertAll: an alert for everyone working the case.
func (s *server) caseAlertAll(r *http.Request, tenant, caseID, typ, body, link string) {
	s.caseAlert(r, tenant, caseID, typ, body, link, caseTeamRoleList...)
}

// notifyRoles: an alert about the case for these roles (caseAlert).
func (s *server) notifyRoles(r *http.Request, tenant, caseID, typ, body string, roles ...string) {
	s.caseAlert(r, tenant, caseID, typ, body, "", roles...)
}

// notifyCaseTeam: everyone working the case.
func (s *server) notifyCaseTeam(r *http.Request, tenant, caseID, typ, body string) {
	s.caseAlertAll(r, tenant, caseID, typ, body, "")
}

// notifyOnSignal: the hand-offs a recorded step makes.
func (s *server) notifyOnSignal(r *http.Request, tenant, caseID, signal string, data map[string]any) {
	cn := s.caseNumberOf(r.Context(), tenant, caseID)
	switch signal {
	case "PLAN_RESPONSE_RECEIVED":
		s.notifyRoles(r, tenant, caseID, "REVIEW_ASSIGNED", fmt.Sprintf("Case %s is ready for coding review", cn), "CODER")
	case "CODING_REVIEW_COMPLETE":
		// Change log #14: the review done, its drafter writes the decision letter; the Attorney hears of the letter
		// itself once it is drafted (lettergen), not of the review.
		if v, _ := data["clinical_requested"].(bool); v {
			s.notifyRoles(r, tenant, caseID, "REVIEW_ASSIGNED", fmt.Sprintf("Case %s is assigned for clinical review", cn), "NURSE_PHYSICIAN")
		}
	case "CLINICAL_REVIEW_COMPLETE":
		if v, _ := data["medical_reviewer_required"].(bool); v { // change log #15: the external Medical Reviewer was involved
			mr, _ := data["medical_reviewer"].(string)
			s.notifyRoles(r, tenant, caseID, "MEDICAL_REVIEWER_INVOLVED",
				fmt.Sprintf("Case %s: the external Medical Reviewer (%s) reviewed medical necessity", cn, mr), "PM")
		}
	case "FINAL_ORDER_ISSUED":
		// The PM (with the Tenant Admin and the Admin) hears of the Final Order's completion; the specialists' work on
		// the case is done (change log #14).
		s.notifyRoles(r, tenant, caseID, "FINAL_ORDER_RELEASED", fmt.Sprintf("Case %s: AHCA's Final Order is recorded — the case moves to invoicing", cn))
		s.archiveCaseLater(r, tenant, caseID) // #24e.ii: archive the case documentation (case_archive.go)
	}
}

// notifyOnDraft: a letter drafted for QA (a). The QA role is told where the draft is made; the PM sees it too.
func (s *server) notifyOnDraft(r *http.Request, tenant, caseID, template, qaRole string) {
	if template != "acceptance" && template != "plan_notification" && template != "estimate_cost" {
		return
	}
	if qaRole == "PM" {
		return // the QA notice already reached the PM
	}
	s.notifyRoles(r, tenant, caseID, "LETTER_CREATED",
		fmt.Sprintf("Case %s: the %s was drafted and is with QA", s.caseNumberOf(r.Context(), tenant, caseID), templateWords(template)), "PM")
}

// notifyOnQA: a QA decision on a decision letter or the Final Order (d).
func (s *server) notifyOnQA(r *http.Request, tenant, caseID, artifact, decision, note string) {
	cn := s.caseNumberOf(r.Context(), tenant, caseID)
	if (artifact == "acceptance" || artifact == "acceptance_letter") && decision != "REJECT" {
		s.notifyRoles(r, tenant, caseID, "CASE_ACCEPTED", fmt.Sprintf("Case %s: the Acceptance letter passed QA — the case is accepted", cn), "PM")
		return
	}
	if !decisionLetters[artifact] {
		return
	}
	what := templateWords(artifact)
	// Change log #14, least privilege: QA's verdict goes to the document's owner -- whoever drafted it -- and the PM
	// (who hears of significant revisions and quality issues), not to every specialist on the case.
	owner := s.draftOwner(r, tenant, caseID, artifact)
	if decision == "REJECT" { // its drafter was told with the decision (DRAFT_REJECTED)
		s.notifyRoles(r, tenant, caseID, "REVISIONS_REQUESTED",
			fmt.Sprintf("Case %s: QA sent the %s back for revisions%s", cn, what, orDash(" — "+note)))
		return
	}
	if artifact == "final_order" { // "QA: notify PM when Final Orders are completed and ready for release"
		s.notifyRoles(r, tenant, caseID, "FINAL_ORDER_READY", fmt.Sprintf("Case %s: the Final Order package passed QA and is ready for release", cn))
		return
	}
	s.notifyRoles(r, tenant, caseID, "QA_PASSED", fmt.Sprintf("Case %s: the %s passed QA", cn, what))
	if owner != "" { // the Coder or Nurse Reviewer who drafted the decision letter hears its final review is complete
		s.notify(r, tenant, owner, "QA_PASSED", fmt.Sprintf("Case %s: your %s passed QA", cn, what), "#/cases/"+caseID)
	}
}

// draftOwner: who drafted the latest version of artifact on the case (requested_by_sub), "" when unknown.
func (s *server) draftOwner(r *http.Request, tenant, caseID, artifact string) string {
	var sub string
	_ = s.db.QueryRow(r.Context(), `SELECT coalesce(requested_by_sub,'') FROM public.qa_reviews
		WHERE tenant=$1 AND case_id::text=$2 AND artifact=$3 ORDER BY created_at DESC LIMIT 1`, tenant, caseID, artifact).Scan(&sub)
	return sub
}

// notifyEligibilityIssue: an ineligible dispute or an AOR hold goes to the PM (the change log's escalation).
func (s *server) notifyEligibilityIssue(r *http.Request, tenant, caseID, result, reason string) {
	if result != "INELIGIBLE" && result != "HOLD_AOR" {
		return
	}
	s.notifyRoles(r, tenant, caseID, "ELIGIBILITY_ISSUE",
		fmt.Sprintf("Case %s: eligibility review found %s%s", s.caseNumberOf(r.Context(), tenant, caseID),
			map[string]string{"INELIGIBLE": "the dispute ineligible", "HOLD_AOR": "an AOR problem (on hold for the Attorney)"}[result], orDash(" — "+reason)), "PM")
}

// templateWords: a template key as people say it.
func templateWords(key string) string {
	if w, ok := map[string]string{
		"acceptance": "Acceptance letter", "plan_notification": "Plan Notification", "estimate_cost": "Cost Estimate",
		"final_order_rationale": "decision letter", "final_order": "Final Order package", "cover_letter_final_notice": "cover letter",
		"default_determination": "Default Determination",
	}[key]; ok {
		return w
	}
	return key
}

// requestRevisions: POST /cases/{caseId}/revision-request {"to": "CODER"|"NURSE_PHYSICIAN"|"BOTH", "note": "..."} --
// the Attorney (or the PM as QA) sends the decision letter back to the Coder and/or the Nurse / Physician with what
// to change (change log 2026-11-25 #24c). Recorded on the case; the specialists are told.
func (s *server) requestRevisions(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "ATTORNEY", "QA", "FEDERAL_ADMIN", "PLATFORM_ADMIN") { // change log #14: the Attorney and QA, not the PM
		http.Error(w, `{"error":"forbidden: the Attorney or QA asks for revisions"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		To   string `json:"to"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	roles := map[string][]string{"CODER": {"CODER"}, "NURSE_PHYSICIAN": {"NURSE_PHYSICIAN"}, "BOTH": {"CODER", "NURSE_PHYSICIAN"}}[in.To]
	if roles == nil || in.Note == "" || len(in.Note) > 4000 {
		http.Error(w, `{"error":"say who revises it (the Coder, the Nurse / Physician or both) and what to change"}`, http.StatusBadRequest)
		return
	}
	cn := s.caseNumberOf(r.Context(), tenant, caseID)
	who := map[string]string{"CODER": "the Coder", "NURSE_PHYSICIAN": "the Nurse / Physician", "BOTH": "the Coder and the Nurse / Physician"}[in.To]
	s.notifyRoles(r, tenant, caseID, "REVISIONS_REQUESTED",
		fmt.Sprintf("Case %s: %s asks for revisions to the decision letter — %s", cn, displayName(p), in.Note), roles...)
	s.logActivity(r.Context(), tenant, caseID, "REVISIONS_REQUESTED", fmt.Sprintf("%s asked %s for revisions: %s", displayName(p), who, in.Note))
	s.logAudit(r.Context(), tenant, caseID, "REVISIONS_REQUESTED", map[string]any{"by": p.Subject, "to": in.To, "note": in.Note})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// qaAcceptRoles: who may decide a QA step the program assigns to role. The QA role and the PM stand in for each
// other (the templates name the PM; the change log's QA role does the review).
func qaAcceptRoles(role string) []string {
	// Change log #14 (least privilege): QA reviews the letters; the PM oversees them but no longer approves in QA's
	// place. Templates that name the PM as their reviewer are QA's to review.
	if role == "PM" || role == "QA" {
		return []string{"QA"}
	}
	if role == "" {
		return nil
	}
	return []string{role}
}

// notifyQA tells whoever decides a QA step on caseID: the case's QA (and the PM) when the step is the PM's or
// QA's, otherwise the role named -- through caseAlert, so the Tenant Admin and the Admin hear it too.
func (s *server) notifyQA(r *http.Request, tenant, caseID, role, typ, body, link string) {
	s.caseAlert(r, tenant, caseID, typ, body, link, qaAcceptRoles(role)...)
}
