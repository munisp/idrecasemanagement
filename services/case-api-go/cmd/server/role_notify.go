// Author: Patrick Munis at newwave
package main

// Who is told what as a case moves (change log 2026-11-25 #14 and #24, "Notifications/Roles" and "Required Workflow
// Notifications"). In this program the Reviewer (CASE_MANAGER) is the change log's Administrator, and the PM keeps
// the QA gate, so "notify QA" and "notify the PM" both reach role:PM -- one notice, not two.
//
//   a. the Reviewer drafts the Acceptance / Plan Notification   -> QA (the template's QA role) + the PM, for sight
//   b. the Coder / Nurse finishes a review (the decision letter) -> the Attorney + the PM
//   c. the Attorney asks for revisions                           -> the Coder / Nurse;  approves -> QA (the PM)
//   d. QA sends a decision letter or the Final Order back        -> the Coder, the Nurse and the Attorney
//      QA passes it                                              -> the PM
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

// notifyRoles sends one notice to each role (each once).
func (s *server) notifyRoles(r *http.Request, tenant, caseID, typ, body string, roles ...string) {
	seen := map[string]bool{}
	for _, role := range roles {
		if role == "" || seen[role] {
			continue
		}
		seen[role] = true
		s.notify(r, tenant, "role:"+role, typ, body, "#/cases/"+caseID)
	}
}

// notifyCaseTeam: the case's Reviewer (its assignee), the specialists who worked it and the PM.
func (s *server) notifyCaseTeam(r *http.Request, tenant, caseID, typ, body string) {
	var who string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT coalesce(assigned_to,'') FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&who)
	if who != "" {
		s.notify(r, tenant, who, typ, body, "#/cases/"+caseID)
	}
	s.notifyRoles(r, tenant, caseID, typ, body, "CODER", "NURSE_PHYSICIAN", "ATTORNEY", "PM")
}

// notifyOnSignal: the hand-offs a recorded step makes.
func (s *server) notifyOnSignal(r *http.Request, tenant, caseID, signal string, data map[string]any) {
	cn := s.caseNumberOf(r.Context(), tenant, caseID)
	switch signal {
	case "PLAN_RESPONSE_RECEIVED":
		s.notifyRoles(r, tenant, caseID, "REVIEW_ASSIGNED", fmt.Sprintf("Case %s is ready for coding review", cn), "CODER")
	case "CODING_REVIEW_COMPLETE":
		s.notifyRoles(r, tenant, caseID, "DECISION_LETTER_DRAFTED",
			fmt.Sprintf("Case %s: the Coder finished the coding review — the decision letter is ready for the Attorney", cn), "ATTORNEY", "PM")
		if v, _ := data["clinical_requested"].(bool); v {
			s.notifyRoles(r, tenant, caseID, "REVIEW_ASSIGNED", fmt.Sprintf("Case %s is assigned for clinical review", cn), "NURSE_PHYSICIAN")
		}
	case "CLINICAL_REVIEW_COMPLETE":
		s.notifyRoles(r, tenant, caseID, "DECISION_LETTER_DRAFTED",
			fmt.Sprintf("Case %s: the Nurse / Physician finished the clinical review — the decision letter is ready for the Attorney", cn), "ATTORNEY", "PM")
		if v, _ := data["medical_reviewer_required"].(bool); v { // change log #15: the external Medical Reviewer was involved
			mr, _ := data["medical_reviewer"].(string)
			s.notifyRoles(r, tenant, caseID, "MEDICAL_REVIEWER_INVOLVED",
				fmt.Sprintf("Case %s: the external Medical Reviewer (%s) reviewed medical necessity", cn, mr), "PM")
		}
	case "ATTORNEY_REVIEW_COMPLETE":
		s.notifyRoles(r, tenant, caseID, "DECISION_LETTER_APPROVED",
			fmt.Sprintf("Case %s: the Attorney approved the decision letter — ready for final QA review", cn), "PM")
	case "FINAL_ORDER_ISSUED":
		s.notifyCaseTeam(r, tenant, caseID, "FINAL_ORDER_RELEASED", fmt.Sprintf("Case %s: AHCA's Final Order is recorded — the case moves to invoicing", cn))
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
	if !decisionLetters[artifact] {
		return
	}
	cn := s.caseNumberOf(r.Context(), tenant, caseID)
	what := templateWords(artifact)
	if decision == "REJECT" {
		s.notifyRoles(r, tenant, caseID, "REVISIONS_REQUESTED",
			fmt.Sprintf("Case %s: QA sent the %s back for revisions%s", cn, what, orDash(" — "+note)), "CODER", "NURSE_PHYSICIAN", "ATTORNEY")
		return
	}
	s.notifyRoles(r, tenant, caseID, "QA_PASSED", fmt.Sprintf("Case %s: the %s passed QA", cn, what), "PM")
	if artifact == "final_order" {
		s.notifyCaseTeam(r, tenant, caseID, "FINAL_ORDER_RELEASED", fmt.Sprintf("Case %s: the Final Order package went to AHCA", cn))
	}
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
	if !hasAnyRole(p, "ATTORNEY", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
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
