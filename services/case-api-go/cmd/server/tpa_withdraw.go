// Author: Patrick Munis at newwave
package main

// A TPA asks to withdraw one of its disputes, at any point before the case closes (2026-11-26). It is recorded as
// the Admin's "Withdrawal requested" step records it (the same signal an emailed request sends); the office then
// verifies it and confirms the withdrawal -- a case is never closed on the filer's word alone. The Tenant Admin,
// the Admin and the PM are told.
//
//	POST /tpa/disputes/{caseId}/withdraw {"reason": "..."}

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var withdrawClosedStatuses = map[string]bool{"CLOSED": true, "WITHDRAWN": true, "DISMISSED": true, "INELIGIBLE": true}

func (s *server) tpaWithdrawRequest(w http.ResponseWriter, r *http.Request) {
	tenant, caseID, org, p, ok := s.tpaOwnDispute(w, r)
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in)
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || len(in.Reason) > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "say why the dispute is being withdrawn (up to 2,000 characters)"})
		return
	}
	var status, caseNumber string
	var already bool
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT status, coalesce(case_number,''),
		EXISTS(SELECT 1 FROM public.case_activities a WHERE a.tenant=$2 AND a.case_id::text=c.id::text AND a.type='WITHDRAWAL_REQUESTED_BY_TPA')
		FROM tenant_%s.cases c WHERE c.id::text=$1`, sanitizeTenant(tenant)), caseID, tenant).Scan(&status, &caseNumber, &already); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if withdrawClosedStatuses[strings.ToUpper(status)] {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this dispute is already closed"})
		return
	}
	if already {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a withdrawal was already requested — the office will confirm it with you"})
		return
	}
	wfID := fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), caseNumber)
	if _, err := s.signalCaseWorkflow(r.Context(), tenant, caseID, wfID, "PROVIDER_WITHDRAW",
		map[string]any{"by": "provider", "source": "tpa", "tpa": org.Name, "contact": displayName(p), "note": in.Reason}); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the request could not be recorded just now — try again in a minute"})
		return
	}
	s.setProgramDateOn(r.Context(), tenant, caseID, "withdrawal_requested_at", time.Now().UTC().Format("2006-01-02"))
	s.setCaseDetail(r.Context(), tenant, caseID, "withdrawal_requested_by", "provider")
	msg := fmt.Sprintf("%s (%s) asked to withdraw the dispute: %s", org.Name, displayName(p), in.Reason)
	s.logActivity(r.Context(), tenant, caseID, "WITHDRAWAL_REQUESTED_BY_TPA", msg+" — the office verifies and confirms it")
	// The step the workflow reads (as the Admin's "Withdrawal requested" records it), so "Confirm the withdrawal" is next.
	s.logActivity(r.Context(), tenant, caseID, "PROVIDER_WITHDRAW", fmt.Sprintf(`Withdrawal requested by %s — {"by":"provider","source":"tpa"}`, org.Name))
	s.logAudit(r.Context(), tenant, caseID, "WITHDRAWAL_REQUESTED_BY_TPA", map[string]any{"by": p.Subject, "tpa_id": org.ID, "reason": in.Reason})
	s.caseAlert(r, tenant, caseID, "WITHDRAWAL_REQUESTED", fmt.Sprintf("%s asks to withdraw %s — verify it with them and confirm the withdrawal", org.Name, caseNumber), "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "next": "The office will verify the request with you and confirm the withdrawal."})
}
