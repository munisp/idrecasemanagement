// Author: Patrick Munis at newwave
// letters.go — request letter generation from DOCX templates (lettergen worker).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// requestLetterGen: POST /v1/tenants/{tenant}/cases/{caseId}/lettergen/{templateKey}
// Validates the template key against the program's letter_templates config and
// enqueues a letter.requested event through the transactional outbox — the
// lettergen worker renders DOCX→PDF, seals it, files it on the docket, and
// opens the QA review. 202 Accepted; the letter appears under Documents and
// the QA queue when ready.
func (s *server) requestLetterGen(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	key := chi.URLParam(r, "templateKey")
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, caseStaffRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}

	// Template must be configured for this program.
	prog := s.loadProgram(r, tenant)
	if prog == nil {
		http.Error(w, `{"error":"no program rules for tenant"}`, http.StatusBadRequest)
		return
	}
	var found bool
	var lts []map[string]any
	if raw, ok := progRaw(r, s, tenant, "letter_templates"); ok {
		_ = json.Unmarshal(raw, &lts)
		for _, lt := range lts {
			if lt["key"] == key {
				found = true
				break
			}
		}
	}
	if !found {
		http.Error(w, `{"error":"unknown letter template for this program"}`, http.StatusBadRequest)
		return
	}
	// Case must exist.
	var caseNumber string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&caseNumber); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}

	// A review invoice's letter needs that party's invoice: refuse here, so the person asking sees why, rather than
	// a "Generating" toast and a failure only the case's activity shows.
	if party := map[string]string{"invoice_health_plan": "HEALTH_PLAN", "invoice_provider": "PROVIDER"}[key]; party != "" {
		var billed bool
		_ = s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM public.invoices WHERE tenant=$1 AND case_id=$2 AND party=$3
			AND kind <> 'INITIAL_FEE' AND status <> 'VOID')`, tenant, caseID, party).Scan(&billed)
		if !billed {
			who := map[string]string{"HEALTH_PLAN": "health plan", "PROVIDER": "provider"}[party]
			writeJSON(w, http.StatusConflict, map[string]string{"error": "there is no review invoice to the " + who +
				" on this case — the PM issues it first (Billing on the case); if the other party was billed, generate theirs"})
			return
		}
	}
	// displayName(p): this flows straight into qa_reviews.drafted_by via lettergen-py, so a reviewer opening the
	// QA queue sees who requested the letter, not a raw Keycloak UUID.
	if err := s.enqueueLetter(r, tenant, caseID, caseNumber, key, displayName(p)); err != nil {
		http.Error(w, `{"error":"outbox"}`, http.StatusInternalServerError)
		return
	}
	// A determination letter request while IN_REVIEW advances the case once
	// the rendered document lands; tick what the platform can already prove.
	s.autoChecklist(r, tenant, caseID)
	s.maybeAdvanceStatus(r, tenant, caseID)
	s.logAudit(r.Context(), tenant, caseID, "LETTER_REQUESTED", map[string]any{
		"by": p.Subject, "template": key, "case_number": caseNumber,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "template": key})
}

// progRaw fetches one key from the program config jsonb.
func progRaw(r *http.Request, s *server, tenant, key string) (json.RawMessage, bool) {
	var raw json.RawMessage
	err := s.db.QueryRow(r.Context(),
		`SELECT config->$2 FROM public.program_rules WHERE tenant=$1`, tenant, key).Scan(&raw)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	return raw, true
}

// enqueueLetter asks lettergen for one letter on a case (a letter.requested event through the outbox) and notes it
// on the case's activity; by is who it is generated for.
func (s *server) enqueueLetter(r *http.Request, tenant, caseID, caseNumber, key, by string) error {
	payload, _ := json.Marshal(map[string]any{
		"type": "letter.requested", "tenant": tenant, "case_id": caseID,
		"template": key, "case_number": caseNumber,
		"requested_by": by, "at": time.Now().UTC(),
		"traceparent": traceparentOf(r.Context()),
	})
	if _, err := s.db.Exec(r.Context(), fmt.Sprintf(
		`INSERT INTO tenant_%s.outbox (topic, key, payload) VALUES ($1,$2,$3)`,
		sanitizeTenant(tenant)),
		fmt.Sprintf("%s.%s.letters", s.cfg.TopicPrefix, tenant), caseID, payload); err != nil {
		return err
	}
	s.logActivity(r.Context(), tenant, caseID, "LETTER_REQUESTED",
		fmt.Sprintf("Letter generation requested by %s — template %s", by, key))
	return nil
}
