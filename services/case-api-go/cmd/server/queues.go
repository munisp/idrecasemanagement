package main

// queues.go — specialty work queues for CODER / NURSE_PHYSICIAN / ATTORNEY / PM.
//
// Review completion previously existed only as Temporal signals, so there was
// nothing to SQL-query for a worklist. public.specialty_reviews is now the
// system of record: staff request a review, the specialist claims and
// completes it, and completion forwards the matching workflow signal —
// the queue table and the workflow can never diverge.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// disciplineMeta maps a queue discipline to its reviewer roles and the
// workflow signal fired on completion.
var disciplineMeta = map[string]struct {
	roles  []string
	signal string
}{
	"CODING":   {[]string{"CODER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, "CODING_REVIEW_COMPLETE"},
	"CLINICAL": {[]string{"NURSE_PHYSICIAN", "DOCTOR", "NURSE", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, "CLINICAL_REVIEW_COMPLETE"},
	"LEGAL":    {[]string{"ATTORNEY", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}, "ATTORNEY_REVIEW_COMPLETE"},
}

// requestReview: POST /cases/{caseId}/reviews — case staff request a
// specialty review on a case (e.g. bundling dispute -> CODING).
func (s *server) requestReview(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires case staff"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Discipline string `json:"discipline"`
		Note       string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	meta, ok := disciplineMeta[in.Discipline]
	if !ok {
		http.Error(w, `{"error":"discipline must be CODING, CLINICAL, or LEGAL"}`, http.StatusBadRequest)
		return
	}
	if len(in.Note) > 4000 {
		http.Error(w, `{"error":"note too long (4000 chars)"}`, http.StatusBadRequest)
		return
	}
	var caseNumber string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var id string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.specialty_reviews (tenant, case_id, discipline, note, requested_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		tenant, caseID, in.Discipline, in.Note, p.Subject).Scan(&id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"an active %s review already exists for this case"}`, in.Discipline), http.StatusConflict)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, in.Discipline+"_REVIEW_REQUESTED",
		fmt.Sprintf("%s review requested by %s — %s", in.Discipline, p.Subject, truncate(in.Note, 200)))
	s.notify(r, tenant, "*", "REVIEW_REQUESTED",
		fmt.Sprintf("Case %s needs a %s review.", caseNumber, in.Discipline), "#/queues/"+in.Discipline)
	_ = meta
	writeJSON(w, http.StatusCreated, map[string]any{"review_id": id, "discipline": in.Discipline, "status": "REQUESTED"})
}

// reviewQueue: GET /queues/{discipline} — the specialist's worklist. PMs see
// every discipline; specialists see their own. Joins the tenant case for the
// context a reviewer needs (number, service line, disputed amount, status).
func (s *server) reviewQueue(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	discipline := chi.URLParam(r, "discipline")
	meta, ok := disciplineMeta[discipline]
	if !ok {
		http.Error(w, `{"error":"unknown queue"}`, http.StatusNotFound)
		return
	}
	if !hasAnyRole(p, append(append([]string{}, meta.roles...), serviceRole)...) {
		http.Error(w, fmt.Sprintf(`{"error":"forbidden: %s queue requires %s"}`, discipline, meta.roles[0]), http.StatusForbidden)
		return
	}
	showAll := r.URL.Query().Get("all") == "1"
	q := fmt.Sprintf(`
		SELECT v.id, v.case_id, c.case_number, c.status, c.service_line,
		       c.disputed_amount_cents, v.discipline, v.status AS review_status,
		       v.note, v.requested_by, v.requested_at, v.reviewer, v.claimed_at, v.completed_at
		FROM public.specialty_reviews v
		JOIN tenant_%s.cases c ON c.id::text = v.case_id
		WHERE v.tenant=$1 AND v.discipline=$2`, sanitizeTenant(tenant))
	args := []any{tenant, discipline}
	if !showAll {
		q += ` AND v.status <> 'COMPLETE'`
	}
	q += ` ORDER BY v.requested_at LIMIT 500`
	rows, err := s.queryRows(r, q, args...)
	if err != nil {
		slog.Error("query failed", "handler", "queues.go", "err", err)
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": rows, "discipline": discipline})
}

// claimReview: POST /reviews/{reviewId}/claim — specialist takes ownership.
func (s *server) claimReview(w http.ResponseWriter, r *http.Request) {
	s.transitionReview(w, r, "IN_REVIEW")
}

// completeReview: POST /reviews/{reviewId}/complete — closes the review and
// forwards the discipline's workflow signal (single writer, no divergence).
func (s *server) completeReview(w http.ResponseWriter, r *http.Request) {
	s.transitionReview(w, r, "COMPLETE")
}

func (s *server) transitionReview(w http.ResponseWriter, r *http.Request, target string) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	reviewID := chi.URLParam(r, "reviewId")
	var in struct {
		Note string `json:"note"`
	}
	// Body optional for claim.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
	}
	var discipline, caseID, curStatus string
	if err := s.db.QueryRow(r.Context(), `
		SELECT discipline, case_id, status FROM public.specialty_reviews
		WHERE tenant=$1 AND id=$2`, tenant, reviewID).Scan(&discipline, &caseID, &curStatus); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	meta := disciplineMeta[discipline]
	if !hasAnyRole(p, append(append([]string{}, meta.roles...), serviceRole)...) {
		http.Error(w, `{"error":"forbidden: not your discipline"}`, http.StatusForbidden)
		return
	}
	if target == "COMPLETE" && curStatus == "COMPLETE" {
		http.Error(w, `{"error":"review already complete"}`, http.StatusConflict)
		return
	}
	res, err := s.db.Exec(r.Context(), `
		UPDATE public.specialty_reviews
		SET status=$3, reviewer=$4, note = CASE WHEN $5<>'' THEN $5 ELSE note END,
		    claimed_at = CASE WHEN $3='IN_REVIEW' THEN now() ELSE claimed_at END,
		    completed_at = CASE WHEN $3='COMPLETE' THEN now() ELSE NULL END
		WHERE tenant=$1 AND id=$2 AND status <> 'COMPLETE'`,
		tenant, reviewID, target, displayName(p), in.Note)
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"transition not allowed"}`, http.StatusConflict)
		return
	}
	if target == "COMPLETE" {
		var caseNumber string
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
			`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber)
		s.signalWorkflow(r, tenant, caseID, caseNumber, meta.signal, map[string]any{
			"review_id": reviewID, "reviewer": p.Subject})
		s.logActivity(r.Context(), tenant, caseID, meta.signal,
			fmt.Sprintf("%s review completed by %s", discipline, p.Subject))
		s.logAudit(r.Context(), tenant, caseID, meta.signal, map[string]any{
			"by": p.Subject, "review_id": reviewID, "discipline": discipline})
	}
	writeJSON(w, http.StatusOK, map[string]any{"review_id": reviewID, "status": target})
}

// pmQueue: GET /queues/pm — escalations needing program-manager attention.
// Backed by the existing escalations table (auto-triggered above the program
// amount threshold, plus manual escalations).
func (s *server) pmQueue(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires PM"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT e.id, e.case_id, c.case_number, c.status, c.service_line,
		       c.disputed_amount_cents, e.clock, e.level, e.escalated_to, e.detail, e.created_at
		FROM public.escalations e
		JOIN tenant_%s.cases c ON c.id::text = e.case_id
		WHERE e.tenant=$1 AND c.status NOT LIKE 'CLOSED%%'
		ORDER BY e.created_at DESC LIMIT 200`, sanitizeTenant(tenant)), tenant)
	if err != nil {
		slog.Error("query failed", "handler", "queues.go", "err", err)
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// The board's second table: the program's open cases, newest first.
	open, err := s.queryRows(r, fmt.Sprintf(`
		SELECT id, case_number, status, opened_at
		FROM tenant_%s.cases
		WHERE status NOT LIKE 'CLOSED%%'
		ORDER BY opened_at DESC LIMIT 100`, sanitizeTenant(tenant)))
	if err != nil {
		slog.Error("query failed", "handler", "queues.go", "err", err)
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// "queue" is kept for existing callers; the board reads the named lists.
	writeJSON(w, http.StatusOK, map[string]any{"queue": rows, "escalations": rows, "open_cases": open, "discipline": "PM"})
}
