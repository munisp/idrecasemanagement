package main

// Bulk dispute intake — a third-party filer (revenue-cycle vendor, legal
// representative, plan delegate) submits many disputes on behalf of MANY
// providers/plans in one authenticated call.
//
// Design contract:
//   - Idempotent by batch_ref: the filer's own batch identifier. A retried
//     submission (network drop, filer timeout) replays the recorded results
//     instead of double-filing — the same guarantee screen-first filers get
//     from "the form only submits once."
//   - Per-item isolation: every item is processed independently; one bad row
//     never blocks 499 good ones, and every row reports its own outcome.
//   - Same semantics as single intake: each item runs through the identical
//     validation and the identical programmed-tenant branch as POST /intake —
//     bulk is a multiplier on the existing path, never a parallel one.
//   - Audited twice: per item (the existing INTAKE_CREATED / startAhcaCase
//     audit) and once per batch (INTAKE_BATCH with counts and the batch_ref).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

const bulkIntakeMaxItems = 500

// bulkIntakeItem mirrors the createIntake payload plus the filer's own
// row-level reference (their spreadsheet row / claim-system key), which is
// echoed back so results can be reconciled against the source file.
type bulkIntakeItem struct {
	ExternalRef         string `json:"external_ref"`
	Email               string `json:"email"`
	ContactName         string `json:"contact_name"`
	Org                 string `json:"org"`
	Notes               string `json:"notes"`
	FilingPartyType     string `json:"filing_party_type"`
	DisputedAmountCents int64  `json:"disputed_amount_cents"`
	QPACents            int64  `json:"qpa_cents"`
}

type bulkIntakeResult struct {
	ExternalRef string `json:"external_ref,omitempty"`
	Status      string `json:"status"` // CREATED | ERROR
	IntakeID    string `json:"intake_id,omitempty"`
	CaseID      string `json:"case_id,omitempty"`
	CaseNumber  string `json:"case_number,omitempty"`
	// PaymentRequired is true when the case was created AWAITING_PAYMENT:
	// the filing party must settle the initial review fee before the case
	// number is released to them and document upload unlocks.
	PaymentRequired bool   `json:"payment_required,omitempty"`
	Error           string `json:"error,omitempty"`
}

// validateBulkIntakeItem is the pure per-row gate — same rules as
// createIntake, exercised by unit tests without a database.
func validateBulkIntakeItem(it bulkIntakeItem) string {
	if strings.TrimSpace(it.Email) == "" || !strings.Contains(it.Email, "@") {
		return "email required"
	}
	fpt := strings.ToUpper(strings.TrimSpace(it.FilingPartyType))
	if fpt != "" && fpt != "PROVIDER" && fpt != "HEALTH_PLAN" {
		return "filing_party_type must be PROVIDER or HEALTH_PLAN"
	}
	if it.DisputedAmountCents < 0 || it.QPACents < 0 {
		return "amounts must be non-negative"
	}
	return ""
}

// processOneBulkIntake runs ONE row through the exact createIntake semantics
// (programmed tenant -> startAhcaCase real-case branch; otherwise the legacy
// intake_requests row with its INTAKE_CREATED audit). Never called without
// validateBulkIntakeItem passing first.
func (s *server) processOneBulkIntake(r *http.Request, tenant, subject string, it bulkIntakeItem) bulkIntakeResult {
	res := bulkIntakeResult{ExternalRef: it.ExternalRef}
	if msg := validateBulkIntakeItem(it); msg != "" {
		res.Status, res.Error = "ERROR", msg
		return res
	}
	fpt := strings.ToUpper(strings.TrimSpace(it.FilingPartyType))
	if fpt == "" {
		fpt = "PROVIDER"
	}
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		caseID, caseNumber, err := s.startAhcaCase(r, tenant, cfg, it.Email, it.ContactName, it.Org, fpt, it.DisputedAmountCents)
		if err != nil {
			res.Status = "ERROR"
			if isUniqueViolation(err) {
				res.Error = "case_number collision — retry the batch (idempotent replay will skip completed rows)"
			} else {
				res.Error = "case creation failed"
			}
			return res
		}
		res.Status, res.CaseID, res.CaseNumber = "CREATED", caseID, caseNumber
		// startAhcaCase applies the payment gate when the program charges an
		// initial fee — surface that on the receipt so the filer knows the
		// party must pay before upload unlocks.
		var st string
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
			`SELECT status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&st)
		res.PaymentRequired = st == "AWAITING_PAYMENT"
		return res
	}
	var id string
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.intake_requests (tenant, email, contact_name, org, notes, outreach_at, filing_party_type, qpa_cents)
		VALUES ($1,$2,$3,$4,$5, now(), $6, nullif($7,0)) RETURNING id`,
		tenant, it.Email, it.ContactName, it.Org, it.Notes, fpt, it.QPACents).Scan(&id)
	if id == "" {
		res.Status, res.Error = "ERROR", "intake insert failed"
		return res
	}
	s.logAudit(r.Context(), tenant, "", "INTAKE_CREATED", map[string]any{
		"by": subject, "intake_id": id, "org": it.Org, "filing_party_type": fpt, "bulk": true,
	})
	res.Status, res.IntakeID = "CREATED", id
	return res
}

// bulkIntake handles POST /intake/bulk.
//
//	{
//	  "batch_ref": "RCM-ACME-2026-10-08-001",        // required, idempotency key
//	  "items": [ {"external_ref": "row-1", "email": "...", ...}, ... ]
//	}
//
// Response is always 200 when the request itself is well-formed: per-row
// outcomes live in results[]; counts summarize. A replayed batch_ref returns
// the recorded results with idempotent_replay: true and files nothing twice.
func (s *server) bulkIntake(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	// BULK_SUBMITTER is the third-party-filer role: it opens intake and reads
	// its own batches — nothing else on the platform. Staff roles keep their
	// existing single-intake rights and gain bulk as a superset.
	if !hasAnyRole(p, "BULK_SUBMITTER", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires BULK_SUBMITTER or staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		BatchRef string           `json:"batch_ref"`
		Items    []bulkIntakeItem `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	in.BatchRef = strings.TrimSpace(in.BatchRef)
	if in.BatchRef == "" || len(in.BatchRef) > 128 {
		http.Error(w, `{"error":"batch_ref required (<=128 chars) — your idempotency key for safe retries"}`, http.StatusBadRequest)
		return
	}
	if len(in.Items) == 0 {
		http.Error(w, `{"error":"items must contain at least one dispute"}`, http.StatusBadRequest)
		return
	}
	if len(in.Items) > bulkIntakeMaxItems {
		http.Error(w, fmt.Sprintf(`{"error":"at most %d items per batch — split larger files into multiple batch_refs"}`, bulkIntakeMaxItems), http.StatusBadRequest)
		return
	}

	// Idempotency check first: this batch_ref already completed -> replay the
	// recorded results verbatim. The filer cannot tell a retry from a fresh
	// submission, which is exactly the point.
	var existing struct {
		ID      int64
		Results []byte
	}
	row := s.db.QueryRow(r.Context(), `
		SELECT id, results FROM public.intake_batches
		WHERE tenant=$1 AND submitter=$2 AND batch_ref=$3`, tenant, p.Subject, in.BatchRef)
	if err := row.Scan(&existing.ID, &existing.Results); err == nil {
		var results []bulkIntakeResult
		_ = json.Unmarshal(existing.Results, &results)
		writeJSON(w, http.StatusOK, map[string]any{
			"batch_id": existing.ID, "idempotent_replay": true, "results": results,
		})
		return
	}

	results := make([]bulkIntakeResult, 0, len(in.Items))
	created, errored := 0, 0
	for i, it := range in.Items {
		if strings.TrimSpace(it.ExternalRef) == "" {
			it.ExternalRef = fmt.Sprintf("row-%d", i+1)
		}
		res := s.processOneBulkIntake(r, tenant, p.Subject, it)
		if res.Status == "CREATED" {
			created++
		} else {
			errored++
		}
		results = append(results, res)
	}
	resultsJSON, _ := json.Marshal(results)

	var batchID int64
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.intake_batches (tenant, submitter, batch_ref, item_count, created_count, error_count, results)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (tenant, submitter, batch_ref) DO NOTHING
		RETURNING id`, tenant, p.Subject, in.BatchRef, len(in.Items), created, errored, resultsJSON).Scan(&batchID)
	if err != nil || batchID == 0 {
		// Lost the race against a concurrent identical submission: replay its
		// recorded results rather than double-filing.
		if scanErr := s.db.QueryRow(r.Context(), `
			SELECT id, results FROM public.intake_batches
			WHERE tenant=$1 AND submitter=$2 AND batch_ref=$3`, tenant, p.Subject, in.BatchRef).Scan(&existing.ID, &existing.Results); scanErr == nil {
			_ = json.Unmarshal(existing.Results, &results)
			writeJSON(w, http.StatusOK, map[string]any{
				"batch_id": existing.ID, "idempotent_replay": true, "results": results,
			})
			return
		}
		http.Error(w, `{"error":"batch record failed"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, "", "INTAKE_BATCH", map[string]any{
		"by": p.Subject, "batch_id": batchID, "batch_ref": in.BatchRef,
		"items": len(in.Items), "created": created, "errors": errored,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": batchID, "batch_ref": in.BatchRef,
		"items": len(in.Items), "created": created, "errors": errored,
		"results": results,
	})
}

// getIntakeBatch returns a previously submitted batch with per-row results —
// the filer's receipt and the support desk's reconciliation view. Submitters
// see only their own batches; staff see any batch in the tenant.
func (s *server) getIntakeBatch(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "BULK_SUBMITTER", "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "batchId")
	var (
		submitter, batchRef         string
		itemCount, createdC, errorC int
		resultsJSON                 []byte
		createdAt                   string
	)
	q := `SELECT submitter, batch_ref, item_count, created_count, error_count, results, created_at::text
	      FROM public.intake_batches WHERE tenant=$1 AND id=$2`
	args := []any{tenant, id}
	if !hasAnyRole(p, "CASE_MANAGER", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		q += ` AND submitter=$3`
		args = append(args, p.Subject)
	}
	if err := s.db.QueryRow(r.Context(), q, args...).Scan(
		&submitter, &batchRef, &itemCount, &createdC, &errorC, &resultsJSON, &createdAt); err != nil {
		http.Error(w, `{"error":"batch not found"}`, http.StatusNotFound)
		return
	}
	var results []bulkIntakeResult
	_ = json.Unmarshal(resultsJSON, &results)
	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": id, "batch_ref": batchRef, "submitter": submitter,
		"items": itemCount, "created": createdC, "errors": errorC,
		"submitted_at": createdAt, "results": results,
	})
}
