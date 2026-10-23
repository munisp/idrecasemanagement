package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Notes on eligibility reviews (scripts/eligibility_notes.sql). The Reviewer's working notes go in with the review
// itself; the PM and the Attorney (and the Reviewer again) can add notes to a review afterwards. Notes are
// append-only: the record keeps who wrote what, when, in which role.

const eligibilityNoteMax = 4000

// eligibilityNoteRoles may write notes on a review: the people who decide, second-review or confirm eligibility.
var eligibilityNoteRoles = []string{"CASE_MANAGER", "PM", "ATTORNEY", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

// noteRole is the role a note is shown under: the most specific eligibility role the writer holds.
func noteRole(p principal) string {
	for _, r := range []string{"ATTORNEY", "PM", "CASE_MANAGER", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"} {
		if hasAnyRole(p, r) {
			return r
		}
	}
	return ""
}

// cleanEligibilityNote trims a note and reports whether it is usable ("" = no note given).
func cleanEligibilityNote(body string) (string, bool) {
	b := strings.TrimSpace(body)
	return b, len([]rune(b)) <= eligibilityNoteMax
}

func (s *server) insertEligibilityNote(ctx context.Context, tenant, caseID, reviewID, body string, p principal) (string, error) {
	var id string
	err := s.db.QueryRow(ctx, `
		INSERT INTO public.eligibility_review_notes (tenant, case_id, review_id, body, author, author_role)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		tenant, caseID, reviewID, body, displayName(p), noteRole(p)).Scan(&id)
	if err == nil {
		s.logActivity(ctx, tenant, caseID, "ELIGIBILITY_NOTE", "Eligibility review note by "+displayName(p)+": "+body)
		s.logAudit(ctx, tenant, caseID, "ELIGIBILITY_NOTE_ADDED", map[string]any{
			"by": p.Subject, "review_id": reviewID, "note_id": id, "role": noteRole(p),
		})
	}
	return id, err
}

// addEligibilityNote: POST /cases/{caseId}/eligibility/{reviewId}/notes {"body": "..."}
func (s *server) addEligibilityNote(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, eligibilityNoteRoles...) {
		http.Error(w, `{"error":"forbidden: eligibility notes are written by the Reviewer, the PM or the Attorney"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID, reviewID := chi.URLParam(r, "caseId"), chi.URLParam(r, "reviewId")
	var in struct {
		Body string `json:"body"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if json.Unmarshal(raw, &in) != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	body, ok := cleanEligibilityNote(in.Body)
	if body == "" || !ok {
		http.Error(w, `{"error":"a note needs some text (at most 4000 characters)"}`, http.StatusBadRequest)
		return
	}
	// The review must be this case's, in this tenant: a review id from another case is a 404, not a cross-post.
	var found bool
	_ = s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM public.eligibility_reviews WHERE id::text=$1 AND tenant=$2 AND case_id=$3)`,
		reviewID, tenant, caseID).Scan(&found)
	if !found {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	id, err := s.insertEligibilityNote(r.Context(), tenant, caseID, reviewID, body, p)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "review_id": reviewID, "body": body,
		"author": displayName(p), "author_role": noteRole(p)})
}

// eligibilityNotesByReview returns each review's notes, oldest first, for the review-history table.
func (s *server) eligibilityNotesByReview(r *http.Request, tenant, caseID string) map[string][]map[string]any {
	rows, err := s.queryRows(r, `
		SELECT id, review_id::text AS review_id, body, author, author_role, created_at
		FROM public.eligibility_review_notes WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC`, tenant, caseID)
	out := map[string][]map[string]any{}
	if err != nil {
		return out // notes are additive: a missing table (database not migrated yet) must not break the history
	}
	for _, n := range rows {
		rid, _ := n["review_id"].(string)
		out[rid] = append(out[rid], n)
	}
	return out
}
