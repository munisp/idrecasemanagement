package main

// claim_eligibility.go — a partially accepted case: which of its claims are ineligible, and why.
//
// PUT /cases/{caseId}/claims/eligibility {"ineligible": ["C2", ...], "reason": "..."}: the claims named are marked
// INELIGIBLE (the others SUBMITTED again) and the reason is kept on the case (details.partial_ineligible_reason).
// The Partial Acceptance letter (lettergen, office_letters.py) says which claims were accepted and which were not.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *server) setClaimEligibility(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: the Reviewer or the PM decides which claims are eligible"}`, http.StatusForbidden)
		return
	}
	caseID := chi.URLParam(r, "caseId")
	if !s.caseVisible(r.Context(), p, tenant, caseID) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var in struct {
		Ineligible []string `json:"ineligible"`
		Reason     string   `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	var marked []string
	for _, n := range in.Ineligible {
		if n = strings.TrimSpace(n); n != "" {
			marked = append(marked, n)
		}
	}
	var total, found int
	_ = s.db.QueryRow(r.Context(), `SELECT count(*), count(*) FILTER (WHERE claim_number = ANY($3)) FROM public.case_claims
		WHERE tenant=$1 AND case_id=$2`, tenant, caseID, marked).Scan(&total, &found)
	switch {
	case total == 0:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this case has no claims recorded yet"})
		return
	case found != len(marked):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "some of those claim numbers are not on this case"})
		return
	case len(marked) >= total:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one claim must stay accepted — if none is eligible, the case is ineligible"})
		return
	}
	reason := strings.TrimSpace(in.Reason)
	if len(marked) > 0 && reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "say why those claims are ineligible"})
		return
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE public.case_claims SET status = CASE WHEN claim_number = ANY($3) THEN 'INELIGIBLE'
		ELSE 'SUBMITTED' END WHERE tenant=$1 AND case_id=$2`, tenant, caseID, marked); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.setCaseDetail(r.Context(), tenant, caseID, "partial_ineligible_reason", truncate(reason, 2000))
	s.logActivity(r.Context(), tenant, caseID, "CLAIM_ELIGIBILITY",
		fmt.Sprintf("%d of %d claims marked ineligible by %s: %s", len(marked), total, displayName(p), strings.Join(marked, ", ")))
	s.logAudit(r.Context(), tenant, caseID, "CLAIM_ELIGIBILITY", map[string]any{"by": p.Subject, "ineligible": marked, "reason": reason})
	writeJSON(w, http.StatusOK, map[string]any{"ineligible": marked, "accepted": total - len(marked)})
}
