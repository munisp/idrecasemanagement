// Author: Patrick Munis at newwave
package main

// SOP step 10 "Clinical/Coding review completed" with more than one reviewer (2026-11-26). Besides the case's Coder
// and Nurse Reviewer (case_helpers), the PM, the Admin or the Tenant Admin can name further reviewers; each records
// their own review (findings and recommended outcome) as a step of their own. The main Coding / Clinical review --
// the signal that moves the case on -- waits until every named reviewer of its kind has recorded theirs.
//
//	GET    /cases/{caseId}/review-team            the further reviewers, with whether each has reviewed
//	POST   /cases/{caseId}/review-team            {"user_id": "...", "role": "CODER"|"NURSE_PHYSICIAN"}
//	DELETE /cases/{caseId}/review-team/{userId}
//	POST   /cases/{caseId}/review-records         a named reviewer's own review {"recommended_outcome", "findings"}

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

type wfReviewer struct {
	ID, Name, Role string
	Done           bool
}

var reviewTeamEditRoles = []string{"PM", "CASE_MANAGER", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

// caseReviewTeam: the further reviewers of a case, each with whether they have recorded a review since being named.
func (s *server) caseReviewTeam(ctx context.Context, tenant, caseID string) []wfReviewer {
	rows, err := s.db.Query(ctx, `
		SELECT t.user_id, t.role, EXISTS(SELECT 1 FROM public.case_review_records r WHERE r.tenant=t.tenant AND r.case_id=t.case_id
		       AND r.user_id=t.user_id AND r.created_at >= t.added_at)
		FROM public.case_review_team t WHERE t.tenant=$1 AND t.case_id=$2 ORDER BY t.added_at`, tenant, caseID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []wfReviewer
	var ids []string
	for rows.Next() {
		var x wfReviewer
		if rows.Scan(&x.ID, &x.Role, &x.Done) == nil {
			out = append(out, x)
			ids = append(ids, x.ID)
		}
	}
	names := s.resolveSubjectNames(ctx, ids)
	for i := range out {
		out[i].Name = orWord(names[out[i].ID], "a reviewer")
	}
	return out
}

// pendingReviewers: the named reviewers of role who have not recorded their review yet, by name.
func pendingReviewers(team []wfReviewer, role string) []string {
	var out []string
	for _, x := range team {
		if x.Role == role && !x.Done {
			out = append(out, x.Name)
		}
	}
	return out
}

func (s *server) getReviewTeam(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	team := s.caseReviewTeam(r.Context(), tenant, chi.URLParam(r, "caseId"))
	out := []map[string]any{}
	for _, x := range team {
		out = append(out, map[string]any{"id": x.ID, "name": x.Name, "role": x.Role, "role_label": assignableRoles[x.Role], "reviewed": x.Done})
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	writeJSON(w, http.StatusOK, map[string]any{"reviewers": out, "can_edit": hasAnyRole(p, reviewTeamEditRoles...)})
}

func (s *server) addReviewer(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, reviewTeamEditRoles...) {
		http.Error(w, `{"error":"forbidden: reviewers are named by the PM or the Admin"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in) != nil || (in.Role != "CODER" && in.Role != "NURSE_PHYSICIAN") || in.UserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose a Coder or a Nurse Reviewer to add"})
		return
	}
	name, err := s.validAssignee(r.Context(), tenant, in.Role, in.UserID)
	if err != nil || name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that person is not a " + assignableRoles[in.Role] + " of this program"})
		return
	}
	if s.caseHelpers(r.Context(), tenant, caseID)[in.Role] == in.UserID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": name + " is already the case's " + assignableRoles[in.Role]})
		return
	}
	if _, err := s.db.Exec(r.Context(), `INSERT INTO public.case_review_team (tenant, case_id, user_id, role, added_by) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant, case_id, user_id) DO UPDATE SET role=EXCLUDED.role, added_by=EXCLUDED.added_by, added_at=now()`,
		tenant, caseID, in.UserID, in.Role, displayName(p)); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	cn := s.caseNumberOf(r.Context(), tenant, caseID)
	s.logActivity(r.Context(), tenant, caseID, "REVIEWER_ADDED", fmt.Sprintf("%s added %s as a further %s", displayName(p), name, assignableRoles[in.Role]))
	s.notify(r, tenant, in.UserID, "REVIEW_ASSIGNED", fmt.Sprintf("Case %s: you are asked to review it too (%s)", cn, assignableRoles[in.Role]), "#/cases/"+caseID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func (s *server) removeReviewer(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, reviewTeamEditRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID, uid := chi.URLParam(r, "caseId"), chi.URLParam(r, "userId")
	tag, _ := s.db.Exec(r.Context(), `DELETE FROM public.case_review_team WHERE tenant=$1 AND case_id=$2 AND user_id=$3`, tenant, caseID, uid)
	if tag.RowsAffected() > 0 {
		s.logActivity(r.Context(), tenant, caseID, "REVIEWER_REMOVED", fmt.Sprintf("%s took a further reviewer off the case", displayName(p)))
	}
	w.WriteHeader(http.StatusNoContent)
}

// recordReview: a named further reviewer records their own review.
func (s *server) recordReview(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var role string
	if s.db.QueryRow(r.Context(), `SELECT role FROM public.case_review_team WHERE tenant=$1 AND case_id=$2 AND user_id=$3`,
		tenant, caseID, p.Subject).Scan(&role) != nil {
		http.Error(w, `{"error":"forbidden: only a reviewer named on this case records a review here"}`, http.StatusForbidden)
		return
	}
	var in struct {
		Outcome  string `json:"recommended_outcome"`
		Findings string `json:"findings"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
	in.Findings = strings.TrimSpace(in.Findings)
	if !recommendableOutcomes[in.Outcome] || in.Findings == "" || len(in.Findings) > 8000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose the recommended outcome and write your findings"})
		return
	}
	if _, err := s.db.Exec(r.Context(), `INSERT INTO public.case_review_records (tenant, case_id, user_id, user_name, role, recommended_outcome, findings)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, tenant, caseID, p.Subject, displayName(p), role, in.Outcome, in.Findings); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	cn := s.caseNumberOf(r.Context(), tenant, caseID)
	words := map[string]string{"OVERTURNED": "overturned", "PARTIALLY_OVERTURNED": "partially overturned", "UPHELD": "upheld"}[in.Outcome]
	s.logActivity(r.Context(), tenant, caseID, "REVIEW_RECORDED", fmt.Sprintf("%s (%s) recorded their review — recommends %s: %s",
		displayName(p), assignableRoles[role], words, truncate(in.Findings, 300)))
	s.caseAlert(r, tenant, caseID, "REVIEW_RECORDED", fmt.Sprintf("Case %s: %s recorded their %s review (%s)", cn, displayName(p),
		strings.ToLower(assignableRoles[role]), words), "", role, "ATTORNEY")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// caseReviewRecords: every recorded further review, newest first (for the decision report).
func (s *server) caseReviewRecords(r *http.Request, tenant, caseID string) []map[string]any {
	rows, _ := s.queryRows(r, `SELECT user_name, role, recommended_outcome, findings, created_at FROM public.case_review_records
		WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC LIMIT 50`, tenant, caseID)
	return rows
}
