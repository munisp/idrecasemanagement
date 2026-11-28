package main

// claim_eligibility.go — a partially accepted case: which of its claims are ineligible, and why.
//
// Each claim found ineligible is recorded ON THE CLAIM (public.case_claims): status INELIGIBLE, its own reason, who
// marked it and when (change log 2026-11-28, "where should we store the claims found ineligible?"). The Partial
// Acceptance letter (lettergen, office_letters.py) gathers them from there, grouped by reason.
//
// PUT /cases/{caseId}/claims/{claimNumber}/eligibility {"ineligible": true, "reason": "..."}: one claim, from the
//   case's Claims list. {"ineligible": false} puts it back as accepted.
// PUT /cases/{caseId}/claims/eligibility {"ineligible": ["C2", ...], "reason": "..."}: several at once with one
//   reason (the older form); claims not named are accepted again.
// At least one claim must stay accepted: if none is eligible, the dispute is ineligible (the eligibility review).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *server) claimEligibilityAllowed(w http.ResponseWriter, r *http.Request) (string, string, principal, bool) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: the Admin or the PM decides which claims are eligible"}`, http.StatusForbidden)
		return "", "", p, false
	}
	caseID := chi.URLParam(r, "caseId")
	if !s.caseVisible(r.Context(), p, tenant, caseID) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return "", "", p, false
	}
	return tenant, caseID, p, true
}

// claimCounts: claims on the case, and how many of them would be ineligible if `marked` were.
func (s *server) claimCounts(ctx context.Context, tenant, caseID string, marked []string) (total, found int) {
	_ = s.db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE claim_number = ANY($3)) FROM public.case_claims
		WHERE tenant=$1 AND case_id=$2`, tenant, caseID, marked).Scan(&total, &found)
	return
}

func (s *server) setClaimEligibility(w http.ResponseWriter, r *http.Request) {
	tenant, caseID, p, ok := s.claimEligibilityAllowed(w, r)
	if !ok {
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
	total, found := s.claimCounts(r.Context(), tenant, caseID, marked)
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
	reason := truncate(strings.TrimSpace(in.Reason), 2000)
	if len(marked) > 0 && reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "say why those claims are ineligible"})
		return
	}
	// Claims already ineligible keep their own reason unless this request gives one; the others are accepted again.
	if _, err := s.db.Exec(r.Context(), `UPDATE public.case_claims SET
			status = CASE WHEN claim_number = ANY($3) THEN 'INELIGIBLE' ELSE 'SUBMITTED' END,
			ineligible_reason = CASE WHEN claim_number = ANY($3) THEN $4 ELSE NULL END,
			ineligible_by = CASE WHEN claim_number = ANY($3) THEN $5 ELSE NULL END,
			ineligible_at = CASE WHEN claim_number = ANY($3) THEN now() ELSE NULL END
		WHERE tenant=$1 AND case_id=$2`, tenant, caseID, marked, reason, displayName(p)); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.setCaseDetail(r.Context(), tenant, caseID, "partial_ineligible_reason", reason)
	s.logActivity(r.Context(), tenant, caseID, "CLAIM_ELIGIBILITY",
		fmt.Sprintf("%d of %d claims marked ineligible by %s: %s", len(marked), total, displayName(p), strings.Join(marked, ", ")))
	s.logAudit(r.Context(), tenant, caseID, "CLAIM_ELIGIBILITY", map[string]any{"by": p.Subject, "ineligible": marked, "reason": reason})
	writeJSON(w, http.StatusOK, map[string]any{"ineligible": marked, "accepted": total - len(marked)})
}

// setOneClaimEligibility marks one claim ineligible (with its own reason) or accepted again.
func (s *server) setOneClaimEligibility(w http.ResponseWriter, r *http.Request) {
	tenant, caseID, p, ok := s.claimEligibilityAllowed(w, r)
	if !ok {
		return
	}
	claim := strings.TrimSpace(chi.URLParam(r, "claimNumber"))
	var in struct {
		Ineligible bool   `json:"ineligible"`
		Reason     string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || claim == "" {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	reason := truncate(strings.TrimSpace(in.Reason), 2000)
	if in.Ineligible && reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "say why this claim is ineligible — the letter gives the reason"})
		return
	}
	var total, already int
	var exists bool
	_ = s.db.QueryRow(r.Context(), `SELECT count(*), count(*) FILTER (WHERE status='INELIGIBLE' AND claim_number<>$3),
			bool_or(claim_number=$3) FROM public.case_claims WHERE tenant=$1 AND case_id=$2`,
		tenant, caseID, claim).Scan(&total, &already, &exists)
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "that claim number is not on this case"})
		return
	}
	if in.Ineligible && already+1 >= total {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one claim must stay accepted — if none is eligible, the dispute is ineligible (use the eligibility review)"})
		return
	}
	var err error
	if in.Ineligible {
		_, err = s.db.Exec(r.Context(), `UPDATE public.case_claims SET status='INELIGIBLE', ineligible_reason=$4,
			ineligible_by=$5, ineligible_at=now() WHERE tenant=$1 AND case_id=$2 AND claim_number=$3`,
			tenant, caseID, claim, reason, displayName(p))
	} else {
		_, err = s.db.Exec(r.Context(), `UPDATE public.case_claims SET status='SUBMITTED', ineligible_reason=NULL,
			ineligible_by=NULL, ineligible_at=NULL WHERE tenant=$1 AND case_id=$2 AND claim_number=$3`, tenant, caseID, claim)
	}
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	what := "accepted again"
	if in.Ineligible {
		what = "marked ineligible: " + reason
	}
	s.logActivity(r.Context(), tenant, caseID, "CLAIM_ELIGIBILITY", fmt.Sprintf("Claim %s %s (by %s)", claim, what, displayName(p)))
	s.logAudit(r.Context(), tenant, caseID, "CLAIM_ELIGIBILITY", map[string]any{"by": p.Subject, "claim": claim, "ineligible": in.Ineligible, "reason": reason})
	writeJSON(w, http.StatusOK, map[string]any{"claim_number": claim, "ineligible": in.Ineligible, "reason": reason})
}
