// Author: Patrick Munis at newwave
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
	"strings"

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
	if !s.featureGate(w, r, r.Context().Value(ctxTenant{}).(string), "review_queues") {
		return
	}
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
	if !s.featureGate(w, r, r.Context().Value(ctxTenant{}).(string), "review_queues") {
		return
	}
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
		       v.note, v.requested_by, v.requested_at, v.reviewer, v.claimed_at, v.completed_at,
		       coalesce(c.details->>'diagnosis_codes','[]') AS diagnosis_codes,
		       coalesce(c.details->>'bundling_suspected','') AS bundling_suspected,
		       h.user_id AS assigned_to, (h.user_id = $3) AS mine
		FROM public.specialty_reviews v
		JOIN tenant_%s.cases c ON c.id::text = v.case_id
		LEFT JOIN public.case_helpers h ON h.tenant = v.tenant AND h.case_id = v.case_id AND h.role = $4
		WHERE v.tenant=$1 AND v.discipline=$2`, sanitizeTenant(tenant))
	args := []any{tenant, discipline, p.Subject, disciplineRole[discipline]} // the specialist named on each case (helpers.go)
	if !showAll {
		q += ` AND v.status <> 'COMPLETE'`
	}
	// ?issue=bundling narrows to bundling/unbundling disputes: cases an
	// extracted document flagged (details.bundling_suspected, set by
	// doc-intel from NCCI/modifier-59/bundling vocabulary) or whose recorded
	// disputed issue mentions bundling.
	if r.URL.Query().Get("issue") == "bundling" && s.featureEnabled(r, tenant, "bundling_filter") {
		q += ` AND (coalesce(c.details->>'bundling_suspected','') = 'true'
		            OR c.details->>'disputed_issue' ILIKE '%bundl%'
		            OR c.details->>'issue' ILIKE '%bundl%')`
	}
	q += ` ORDER BY (h.user_id = $3) IS TRUE DESC, v.requested_at DESC LIMIT 500` // your own cases first, newest first
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
	if !s.featureGate(w, r, r.Context().Value(ctxTenant{}).(string), "review_queues") {
		return
	}
	s.transitionReview(w, r, "IN_REVIEW")
}

// completeReview: POST /reviews/{reviewId}/complete — closes the review and
// forwards the discipline's workflow signal (single writer, no divergence).
func (s *server) completeReview(w http.ResponseWriter, r *http.Request) {
	if !s.featureGate(w, r, r.Context().Value(ctxTenant{}).(string), "review_queues") {
		return
	}
	s.transitionReview(w, r, "COMPLETE")
}

func (s *server) transitionReview(w http.ResponseWriter, r *http.Request, target string) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	reviewID := chi.URLParam(r, "reviewId")
	var in struct {
		Note string `json:"note"`
		// Coding only, and required there: the same question the case's
		// Steps panel asks -- does the case need clinical review?
		ClinicalRequested *bool `json:"clinical_requested"`
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
	if target == "COMPLETE" {
		if curStatus == "COMPLETE" {
			http.Error(w, `{"error":"review already complete"}`, http.StatusConflict)
			return
		}
		data, msg := reviewCompletionData(discipline, reviewID, displayName(p), in.ClinicalRequested)
		if msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		// The same path the case's Steps panel takes: role floor, step order,
		// workflow signal, the activity record Steps reads, audit -- and it
		// closes this item with the reviewer's note.
		if !s.deliverCaseSignal(w, r, tenant, caseID, meta.signal, data, in.Note) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"review_id": reviewID, "status": target})
		return
	}
	res, err := s.db.Exec(r.Context(), `
		UPDATE public.specialty_reviews
		SET status=$3, reviewer=$4, note = CASE WHEN $5<>'' THEN $5 ELSE note END, claimed_at = now()
		WHERE tenant=$1 AND id=$2 AND status <> 'COMPLETE'`,
		tenant, reviewID, target, displayName(p), in.Note)
	if err != nil || res.RowsAffected() == 0 {
		http.Error(w, `{"error":"transition not allowed"}`, http.StatusConflict)
		return
	}
	// Claiming a review is when it starts: the review's working target counts from here.
	if k := reviewStartedKey[discipline]; k != "" && target == "IN_REVIEW" {
		s.markProgramDate(r.Context(), tenant, caseID, k)
	}
	writeJSON(w, http.StatusOK, map[string]any{"review_id": reviewID, "status": target})
}

// pmQueue: GET /queues/pm — escalations needing program-manager attention.
// Backed by the existing escalations table (auto-triggered above the program
// amount threshold, plus manual escalations).
func (s *server) pmQueue(w http.ResponseWriter, r *http.Request) {
	if !s.featureGate(w, r, r.Context().Value(ctxTenant{}).(string), "review_queues") {
		return
	}
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

// reviewDisciplineOf is the queue a review-complete workflow signal belongs to.
func reviewDisciplineOf(signal string) string {
	for d, m := range disciplineMeta {
		if m.signal == signal {
			return d
		}
	}
	return ""
}

// reviewCompletionData is what a queue completion sends the workflow: what
// the case's Steps panel would send for the same step. Coding must say
// whether clinical review is needed. Returns an error message when not.
func reviewCompletionData(discipline, reviewID, reviewer string, clinical *bool) (map[string]any, string) {
	data := map[string]any{"review_id": reviewID, "reviewer": reviewer}
	if discipline == "CODING" {
		if clinical == nil {
			return nil, `{"error":"say whether the case needs clinical review (clinical_requested)"}`
		}
		data["clinical_requested"] = *clinical
	}
	return data, ""
}

// closeOpenReview closes a case's open review in one discipline, whichever
// way the step was done: from the queue (with the reviewer's note) or from
// the case's Steps panel (noted as such).
func (s *server) closeOpenReview(r *http.Request, tenant, caseID, discipline, by, note string) {
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.specialty_reviews
		SET status='COMPLETE', completed_at=now(),
		    reviewer = CASE WHEN $5<>'' OR reviewer='' THEN $4 ELSE reviewer END,
		    note = CASE WHEN $5<>'' THEN $5 ELSE note || ' (completed from the case''s Steps)' END
		WHERE tenant=$1 AND case_id=$2 AND discipline=$3 AND status <> 'COMPLETE'`,
		tenant, caseID, discipline, by, note)
}

// openReview puts a case in a discipline's queue unless it is already there,
// and tells that discipline.
func (s *server) openReview(r *http.Request, tenant, caseID, caseNumber, discipline, note, by string) {
	tag, err := s.db.Exec(r.Context(), `
		INSERT INTO public.specialty_reviews (tenant, case_id, discipline, note, requested_by)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, tenant, caseID, discipline, truncate(note, 4000), by)
	if err != nil || tag.RowsAffected() == 0 {
		return
	}
	s.logActivity(r.Context(), tenant, caseID, discipline+"_REVIEW_REQUESTED", note)
	for _, role := range disciplineMeta[discipline].roles {
		if role == "FEDERAL_ADMIN" || role == "PLATFORM_ADMIN" {
			continue
		}
		s.notify(r, tenant, "role:"+role, "REVIEW_REQUESTED",
			fmt.Sprintf("Case %s needs a %s review.", caseNumber, strings.ToLower(discipline)), "#/queues/"+discipline)
	}
}

// reviewStartedKey / reviewDoneKey: the program dates a specialty review's start and end are recorded under.
var reviewStartedKey = map[string]string{"CODING": "coding_review_started_at", "CLINICAL": "clinical_review_started_at", "LEGAL": "attorney_review_started_at"}
var reviewDoneKey = map[string]string{"CODING": "coding_review_completed_at", "CLINICAL": "clinical_review_completed_at", "LEGAL": "attorney_review_completed_at"}
