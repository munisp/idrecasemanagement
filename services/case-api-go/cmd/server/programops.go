package main

// Program operations: invoicing/receivables with dual parties (G8), bulk
// claims import for large-volume disputes (G10), pre-case intake requests
// with refund windows (G12), contract deliverables schedule (G7), and
// agency opt-out decisions (G14). All amounts in cents; invoice number =
// case number per FL AHCA program semantics, configurable per tenant.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---- Invoices / receivables (G8) --------------------------------------------

// issueInvoice creates a receivable against HEALTH_PLAN or PROVIDER. Invoice
// number equals the case number (program convention); both parties can carry
// independent receivables on one case (dual receivables).
func (s *server) issueInvoice(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Party       string `json:"party"` // HEALTH_PLAN|PROVIDER
		Kind        string `json:"kind"`  // INITIAL_FEE|FULL_REVIEW|DEFAULT
		AmountCents int64  `json:"amount_cents"`
		DueDays     int    `json:"due_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		(in.Party != "HEALTH_PLAN" && in.Party != "PROVIDER") || in.AmountCents <= 0 {
		http.Error(w, `{"error":"party (HEALTH_PLAN|PROVIDER) and positive amount_cents required"}`, http.StatusBadRequest)
		return
	}
	if in.Kind == "" {
		in.Kind = "INITIAL_FEE"
	}
	if in.DueDays <= 0 {
		in.DueDays = 30
		if cfg := s.loadProgram(r, tenant); cfg != nil && cfg.Fees.InvoiceDueDays != nil {
			in.DueDays = *cfg.Fees.InvoiceDueDays
		}
	}
	var caseNumber string
	if err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&caseNumber); err != nil {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	var invID string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.invoices (tenant, case_id, invoice_no, party, kind, amount_cents, due_date)
		VALUES ($1,$2,$3,$4,$5,$6, (now() + make_interval(days => $7))::date)
		ON CONFLICT (tenant, case_id, party, kind) DO UPDATE SET amount_cents=EXCLUDED.amount_cents, status='OPEN'
		RETURNING id`, tenant, caseID, caseNumber, in.Party, in.Kind, in.AmountCents, in.DueDays).Scan(&invID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	s.logActivity(r.Context(), tenant, caseID, "INVOICE_ISSUED",
		fmt.Sprintf("Invoice %s issued to %s for $%d.%02d (%s, %d-day terms) by %s",
			caseNumber, in.Party, in.AmountCents/100, in.AmountCents%100, in.Kind, in.DueDays, p.Subject))
	s.finEvent(r, tenant, caseID, invID, "INVOICE_ISSUED", "NONE", in.AmountCents, in.Party, caseNumber, p.Subject)
	writeJSON(w, http.StatusOK, map[string]any{"invoice_id": invID, "invoice_no": caseNumber})
}

func (s *server) listInvoices(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	where, args := `tenant=$1`, []any{tenant}
	if caseID != "" {
		where += ` AND case_id=$2`
		args = append(args, caseID)
	}
	rows, err := s.queryRows(r, `
		SELECT id, case_id, invoice_no, party, kind, amount_cents, status, due_date, paid_at, remittance_ref, created_at
		FROM public.invoices WHERE `+where+` ORDER BY created_at DESC`, args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoices": rows})
}

// settleInvoice records a payment, void, or refund (remittance reference kept
// for reconciliation with the payment provider).
func (s *server) settleInvoice(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	invID := chi.URLParam(r, "invId")
	var in struct {
		Action        string `json:"action"` // PAY|REFUND|VOID
		RemittanceRef string `json:"remittance_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	status := map[string]string{"PAY": "PAID", "REFUND": "REFUNDED", "VOID": "VOID"}[in.Action]
	if status == "" {
		http.Error(w, `{"error":"action must be PAY|REFUND|VOID"}`, http.StatusBadRequest)
		return
	}
	// Card refund first when a Stripe payment exists — never mark REFUNDED
	// without the money actually moving.
	if in.Action == "REFUND" {
		if err := s.refundCardPayment(r, tenant, invID); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"stripe refund failed: %s"}`, err.Error()), http.StatusBadGateway)
			return
		}
	}
	var caseID string
	var amount int64
	var party string
	err := s.db.QueryRow(r.Context(), `
		UPDATE public.invoices SET status=$3, remittance_ref=$4,
		       paid_at = CASE WHEN $3='PAID' THEN now()::date ELSE paid_at END
		WHERE tenant=$1 AND id=$2 AND status='OPEN' RETURNING case_id, amount_cents, party`,
		tenant, invID, status, in.RemittanceRef).Scan(&caseID, &amount, &party)
	if err != nil {
		http.Error(w, `{"error":"not open or not found"}`, http.StatusConflict)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "INVOICE_"+status,
		fmt.Sprintf("Invoice %s marked %s%s", invID, status, orDash(" — remittance "+in.RemittanceRef)))
	if status == "PAID" {
		s.maybeMarkDecidedInvoicePaid(r, tenant, caseID)
	}
	// unified financial stream (offline settlements: check/ACH/manual)
	kind, dir := "INVOICE_VOIDED", "NONE"
	if in.Action == "PAY" {
		kind, dir = "PAYMENT_PAID", "IN"
	} else if in.Action == "REFUND" {
		kind, dir = "REFUND_ISSUED", "OUT"
	}
	s.finEvent(r, tenant, caseID, invID, kind, dir, amount, party, in.RemittanceRef,
		r.Context().Value(ctxPrincipal{}).(principal).Subject)
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

// receivablesReport: aging view across the tenant (open/paid/refunded totals).
func (s *server) receivablesReport(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, `
		SELECT party, kind, status, count(*) AS n, sum(amount_cents) AS total_cents,
		       sum(amount_cents) FILTER (WHERE status='OPEN' AND due_date < now()::date) AS overdue_cents
		FROM public.invoices WHERE tenant=$1 GROUP BY party, kind, status ORDER BY party, kind, status`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"receivables": rows})
}

// ---- Bulk claims (G10: large-volume disputes, e.g. 21k claims/case) --------

// importClaims accepts a batch of claim lines for a case; idempotent on
// (case_id, claim_number). Batch endpoint keeps 21k-claim cases ingestible
// without per-row HTTP overhead.
func (s *server) importClaims(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Claims []struct {
			ClaimNumber string `json:"claim_number"`
			CPT         string `json:"cpt"`
			BilledCents int64  `json:"billed_cents"`
			PaidCents   int64  `json:"paid_cents"`
		} `json:"claims"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Claims) == 0 {
		http.Error(w, `{"error":"claims array required"}`, http.StatusBadRequest)
		return
	}
	if len(in.Claims) > 5000 {
		http.Error(w, `{"error":"max 5000 claims per batch"}`, http.StatusBadRequest)
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())
	var billed int64
	for _, c := range in.Claims {
		if c.ClaimNumber == "" {
			continue
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO public.case_claims (tenant, case_id, claim_number, cpt, billed_cents, paid_cents)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (tenant, case_id, claim_number) DO NOTHING`,
			tenant, caseID, c.ClaimNumber, c.CPT, c.BilledCents, c.PaidCents); err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		billed += c.BilledCents
	}
	if _, err := tx.Exec(r.Context(),
		fmt.Sprintf(`UPDATE tenant_%s.cases SET num_claims = coalesce(num_claims,0)+$2, disputed_amount_cents = coalesce(disputed_amount_cents,0)+$3, updated_at=now() WHERE id=$1`,
			sanitizeTenant(tenant)), caseID, len(in.Claims), billed); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	// large-volume disputes can trip the escalation trigger too
	var disputed int64
	_ = s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT coalesce(disputed_amount_cents,0) FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&disputed)
	s.escalationTrigger(r, tenant, caseID, disputed)
	s.logActivity(r.Context(), tenant, caseID, "CLAIMS_IMPORT",
		fmt.Sprintf("%d claim lines imported ($%d.%02d billed; running total %d claims)",
			len(in.Claims), billed/100, billed%100, len(in.Claims)))
	resp := map[string]any{"imported": len(in.Claims)}
	// Adopted Capitol Bridge large-volume policy (v01.01.2026): evaluated on
	// every import when the tenant has it enabled; violations land on the
	// case record + activity stream.
	if vc := s.checkVolumeRules(r, tenant, caseID); vc != nil {
		resp["volume_check"] = vc
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) listClaims(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	rows, err := s.queryRows(r, `
		SELECT claim_number, cpt, billed_cents, paid_cents, created_at
		FROM public.case_claims WHERE tenant=$1 AND case_id=$2 ORDER BY claim_number LIMIT 500`,
		tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"claims": rows, "note": "first 500 lines; export via reports for full sets"})
}

// ---- Pre-case intake (G12) ---------------------------------------------------

// createIntake opens a pre-case intake request (INSTRUCTURED = packet
// requested, awaiting docs/fee). outreach_at anchors the refund window AND
// the day-13 completeness gate. filing_party_type records who files —
// AHCA 2026: almost always PROVIDER, but a health plan CAN file; the value
// mirrors the notification flow (the NON-filing party receives the plan
// notification packet).
func (s *server) createIntake(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Email           string `json:"email"`
		ContactName     string `json:"contact_name"`
		Org             string `json:"org"`
		Notes           string `json:"notes"`
		FilingPartyType string `json:"filing_party_type"` // PROVIDER|HEALTH_PLAN (default PROVIDER)
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Email == "" {
		http.Error(w, `{"error":"email required"}`, http.StatusBadRequest)
		return
	}
	fpt := strings.ToUpper(strings.TrimSpace(in.FilingPartyType))
	if fpt == "" {
		fpt = "PROVIDER"
	}
	if fpt != "PROVIDER" && fpt != "HEALTH_PLAN" {
		http.Error(w, `{"error":"filing_party_type must be PROVIDER or HEALTH_PLAN"}`, http.StatusBadRequest)
		return
	}
	var id string
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.intake_requests (tenant, email, contact_name, org, notes, outreach_at, filing_party_type)
		VALUES ($1,$2,$3,$4,$5, now(), $6) RETURNING id`,
		tenant, in.Email, in.ContactName, in.Org, in.Notes, fpt).Scan(&id)
	writeJSON(w, http.StatusOK, map[string]any{"intake_id": id, "status": "INSTRUCTED", "filing_party_type": fpt})
}

// advanceIntake moves an intake through DOCS_RECEIVED → PACKET_COMPLETE →
// PAID → CONVERTED, or closes it CLOSED_REFUNDED (enforcing the program
// refund window when set) / INELIGIBLE.
//
// PACKET_COMPLETE is the statutory anchor (AHCA 2026): the 10-day initial
// review runs from COMPLETE-packet receipt, so the timestamp is written
// here and copied onto the case at CONVERTED as details.packet_complete_at —
// the basis the program's INITIAL_REVIEW clock config already references.
// The review clock never starts from payment or first submission.
func (s *server) advanceIntake(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	id := chi.URLParam(r, "intakeId")
	var in struct {
		Status string `json:"status"`
		CaseID string `json:"case_id"` // required on CONVERTED
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Status == "" {
		http.Error(w, `{"error":"status required"}`, http.StatusBadRequest)
		return
	}
	if in.Status == "CONVERTED" && in.CaseID == "" {
		http.Error(w, `{"error":"case_id required to convert"}`, http.StatusBadRequest)
		return
	}
	if in.Status == "PACKET_COMPLETE" {
		// Idempotent anchor: first completeness event wins; re-marks don't
		// restart the clock.
		if _, err := s.db.Exec(r.Context(), `
			UPDATE public.intake_requests SET status=$3, packet_complete_at=coalesce(packet_complete_at, now())
			WHERE tenant=$1 AND id=$2`, tenant, id, in.Status); err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": in.Status})
		return
	}
	if in.Status == "CONVERTED" {
		// Carry the filing party type and the packet-complete anchor onto the
		// case record so the review clock and notification routing follow the
		// case, not the intake row.
		var fpt string
		var packetAt *time.Time
		_ = s.db.QueryRow(r.Context(),
			`SELECT filing_party_type, packet_complete_at FROM public.intake_requests WHERE tenant=$1 AND id=$2`,
			tenant, id).Scan(&fpt, &packetAt)
		var packetISO string
		if packetAt != nil {
			packetISO = packetAt.UTC().Format(time.RFC3339)
		}
		if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
			UPDATE tenant_%s.cases SET details = details || jsonb_build_object(
			  'filing_party_type', $2::text, 'packet_complete_at', $3::text), updated_at=now()
			WHERE id=$1`, sanitizeTenant(tenant)), in.CaseID, fpt, packetISO); err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
	}
	if in.Status == "CLOSED_REFUNDED" {
		// refund window enforcement (G12 + fees.refund_window_days)
		if cfg := s.loadProgram(r, tenant); cfg != nil && cfg.Fees.RefundWindowDays > 0 {
			var outreach time.Time
			if err := s.db.QueryRow(r.Context(),
				`SELECT outreach_at FROM public.intake_requests WHERE tenant=$1 AND id=$2`, tenant, id).
				Scan(&outreach); err == nil &&
				time.Now().After(outreach.AddDate(0, 0, cfg.Fees.RefundWindowDays)) {
				http.Error(w, fmt.Sprintf(`{"error":"refund window (%d days) has closed"}`, cfg.Fees.RefundWindowDays), http.StatusConflict)
				return
			}
		}
	}
	_, err := s.db.Exec(r.Context(), `
		UPDATE public.intake_requests SET status=$3, case_id=$4
		WHERE tenant=$1 AND id=$2`, tenant, id, in.Status, in.CaseID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": in.Status})
}

func (s *server) listIntake(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, `
		SELECT id, email, contact_name, org, status, outreach_at, case_id, created_at,
		       filing_party_type, packet_complete_at
		FROM public.intake_requests WHERE tenant=$1 ORDER BY created_at DESC LIMIT 200`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"intake": rows})
}

// ---- Deliverables schedule (G7) ----------------------------------------------

// deliverables returns the program's contract deliverables with computed next
// due dates (weekly:MONDAY, monthly:10, case:EVENT).
func (s *server) listDeliverables(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var raw []byte
	if err := s.db.QueryRow(r.Context(),
		`SELECT config->'deliverables' FROM public.program_rules WHERE tenant=$1`, tenant).
		Scan(&raw); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"deliverables": []any{}})
		return
	}
	var rules []struct {
		Name    string `json:"name"`
		DueRule string `json:"due_rule"`
		Format  string `json:"format"`
	}
	_ = json.Unmarshal(raw, &rules)
	type dv struct {
		Name    string `json:"name"`
		DueRule string `json:"due_rule"`
		Format  string `json:"format"`
		NextDue string `json:"next_due"`
	}
	out := []dv{}
	now := time.Now()
	for _, d := range rules {
		next := ""
		switch {
		case len(d.DueRule) > 7 && d.DueRule[:7] == "weekly:":
			want := map[string]time.Weekday{"MONDAY": time.Monday, "TUESDAY": time.Tuesday, "WEDNESDAY": time.Wednesday, "THURSDAY": time.Thursday, "FRIDAY": time.Friday}[d.DueRule[7:]]
			delta := (int(want) - int(now.Weekday()) + 7) % 7
			if delta == 0 {
				delta = 7
			}
			next = now.AddDate(0, 0, delta).Format("2006-01-02")
		case len(d.DueRule) > 8 && d.DueRule[:8] == "monthly:":
			var day int
			fmt.Sscanf(d.DueRule[8:], "%d", &day)
			cand := time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, time.UTC)
			if cand.Before(now) {
				cand = cand.AddDate(0, 1, 0)
			}
			next = cand.Format("2006-01-02")
		default:
			next = "event-driven"
		}
		out = append(out, dv{d.Name, d.DueRule, d.Format, next})
	}
	// persisted deliverable instances (status + delivery timestamps)
	rows, _ := s.queryRows(r, `
		SELECT name, case_id, due_date, status, delivered_at FROM public.deliverables
		WHERE tenant=$1 ORDER BY due_date DESC NULLS LAST LIMIT 50`, tenant)
	writeJSON(w, http.StatusOK, map[string]any{"deliverables": out, "history": rows})
}

func (s *server) submitDeliverable(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Name     string `json:"name"`
		CaseID   string `json:"case_id"`
		DueRule  string `json:"due_rule"`
		DueDate  string `json:"due_date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
		return
	}
	if in.DueRule == "" {
		in.DueRule = "adhoc"
	}
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.deliverables (tenant, name, due_rule, case_id, due_date, status, delivered_at)
		VALUES ($1,$2,$3,nullif($4,''),nullif($5,'')::date,'DELIVERED',now())
		ON CONFLICT (tenant, name, case_id) DO UPDATE SET status='DELIVERED', delivered_at=now()`,
		tenant, in.Name, in.DueRule, in.CaseID, in.DueDate)
	writeJSON(w, http.StatusOK, map[string]string{"status": "DELIVERED"})
}

// ---- Agency opt-out (G14) -----------------------------------------------------

func (s *server) recordOptOut(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Eligible  bool   `json:"eligible"`  // plan eligible to opt out of the state process
		Rationale string `json:"rationale"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.opt_out_decisions (tenant, case_id, eligible, rationale, decided_by)
		VALUES ($1,$2,$3,$4,$5)`, tenant, caseID, in.Eligible, in.Rationale, p.Subject)
	if in.Eligible {
		_, _ = s.db.Exec(r.Context(),
			fmt.Sprintf(`UPDATE tenant_%s.cases SET internal_status='Plan Opt-Out', updated_at=now() WHERE id=$1`, sanitizeTenant(tenant)), caseID)
	}
	s.logActivity(r.Context(), tenant, caseID, "OPT_OUT_DECISION",
		fmt.Sprintf("Opt-out eligibility: %v%s (by %s)", in.Eligible, orDash(" — "+in.Rationale), p.Subject))
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}


// ---- FL AHCA lifecycle semantics (AHCA answers, 2026) -------------------------

// terminalInternalStatuses: the ONLY ways a case is closed per AHCA — Plan
// Opt-Out, Ineligible, Dismissed, Withdrawn. A completed case is NOT closed;
// it rests at decidedInvoicePaidStatus.
var terminalInternalStatuses = map[string]bool{
	"Plan Opt-Out": true, "Ineligible": true, "Dismissed": true, "Withdrawn": true,
}

const decidedInvoicePaidStatus = "Decided - Invoice Paid"

// maybeMarkDecidedInvoicePaid sets the completed-case resting status when the
// last open receivable on a decided case is paid. AHCA: closure is reserved
// for opt-out/ineligible/dismissed/withdrawn — payment completion is its own
// terminal-but-not-closed state.
func (s *server) maybeMarkDecidedInvoicePaid(r *http.Request, tenant, caseID string) {
	var open int
	var internal string
	if err := s.db.QueryRow(r.Context(), `
		SELECT count(*) FILTER (WHERE status='OPEN') FROM public.invoices
		WHERE tenant=$1 AND case_id=$2`, tenant, caseID).Scan(&open); err != nil || open > 0 {
		return
	}
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(internal_status,'') FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&internal); err != nil {
		return
	}
	if terminalInternalStatuses[internal] || internal == decidedInvoicePaidStatus || internal == "" {
		return // already terminal, already resting, or not yet decided
	}
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(
		`UPDATE tenant_%s.cases SET internal_status=$2, updated_at=now() WHERE id=$1`,
		sanitizeTenant(tenant)), caseID, decidedInvoicePaidStatus)
	s.logActivity(r.Context(), tenant, caseID, "CASE_COMPLETED",
		"All invoices paid — case rests at 'Decided - Invoice Paid' (completed, not closed)")
}

// sweepIntakeDay13: AHCA completeness gate — an RFI may ride with the
// acceptance letter, but if the documentation hasn't arrived by day 13 after
// outreach, the case is found incomplete and an ineligibility letter issues.
// Runs daily (registered in main.go); idempotent by status transition.
func (s *server) sweepIntakeDay13() {
	rows, err := s.db.Query(context.Background(), `
		UPDATE public.intake_requests
		SET status='INELIGIBLE'
		WHERE status IN ('INSTRUCTED','DOCS_RECEIVED')
		  AND packet_complete_at IS NULL
		  AND outreach_at < now() - interval '13 days'
		RETURNING tenant, id, email`)
	if err != nil {
		return
	}
	defer rows.Close()
	type hit struct{ tenant, id, email string }
	var hits []hit
	for rows.Next() {
		var h hit
		if rows.Scan(&h.tenant, &h.id, &h.email) == nil {
			hits = append(hits, h)
		}
	}
	for _, h := range hits {
		// Staff-facing notification so the ineligibility letter is drafted and
		// sent (letter content itself requires the program's DOCX template).
		_, _ = s.db.Exec(context.Background(), `
			INSERT INTO public.notifications (tenant, user_sub, type, body)
			VALUES ($1,'*','SLA_BREACH',$2)`,
			h.tenant, fmt.Sprintf("Intake %s (%s) found incomplete at day 13 — issue ineligibility letter", h.id, h.email))
	}
}

// ---- Large-volume claim dispute rules (Capitol Bridge policy v01.01.2026) ----

type volumeRules struct {
	Enabled   bool `json:"enabled"`
	Threshold int  `json:"large_volume_threshold_claims"`
	SingleCPT bool `json:"single_cpt_per_dispute"`
	Caps      map[string]struct {
		PerDispute   int `json:"max_claims_per_dispute"`
		Rolling14Day int `json:"max_claims_per_rolling_14_days"`
	} `json:"caps"`
}

func (s *server) loadVolumeRules(r *http.Request, tenant string) *volumeRules {
	var raw []byte
	if err := s.db.QueryRow(r.Context(),
		`SELECT config->'volume_rules' FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw); err != nil {
		return nil
	}
	var vr volumeRules
	if json.Unmarshal(raw, &vr) != nil || !vr.Enabled || vr.Threshold <= 0 {
		return nil
	}
	return &vr
}

// checkVolumeRules evaluates the adopted Capitol Bridge large-volume policy
// against a case after a claims import. Violations are recorded on the case
// (details.volume_violations) and the activity stream; disposition per policy
// is INELIGIBLE with resubmission permitted, decided by staff at review.
func (s *server) checkVolumeRules(r *http.Request, tenant, caseID string) map[string]any {
	vr := s.loadVolumeRules(r, tenant)
	if vr == nil {
		return nil
	}
	var numClaims int
	var providerID, medReview string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(num_claims,0), coalesce(provider_id,''),
		        coalesce(details->>'requires_medical_review','')
		 FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).
		Scan(&numClaims, &providerID, &medReview); err != nil {
		return nil
	}
	if numClaims < vr.Threshold {
		return map[string]any{"large_volume": false, "claims": numClaims}
	}

	capKey := "no_medical_review"
	if medReview == "true" || medReview == "yes" {
		capKey = "medical_review"
	}
	caps := vr.Caps[capKey]
	var violations []string

	if caps.PerDispute > 0 && numClaims > caps.PerDispute {
		violations = append(violations, fmt.Sprintf(
			"%d claims exceeds the %d-claim per-dispute cap (%s)", numClaims, caps.PerDispute, capKey))
	}
	// One CPT code per dispute (claims without a CPT don't count against).
	if vr.SingleCPT {
		var distinct int
		_ = s.db.QueryRow(r.Context(), `
			SELECT count(DISTINCT nullif(cpt,'')) FROM public.case_claims
			WHERE tenant=$1 AND case_id=$2`, tenant, caseID).Scan(&distinct)
		if distinct > 1 {
			violations = append(violations, fmt.Sprintf(
				"%d distinct CPT codes — large-volume disputes are limited to one CPT code per dispute", distinct))
		}
	}
	// Rolling 14-day submission cap across the filing party's disputes.
	if caps.Rolling14Day > 0 && providerID != "" {
		var rolling int
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`
			SELECT coalesce(sum(num_claims),0) FROM tenant_%s.cases
			WHERE provider_id=$1 AND opened_at > now() - interval '14 days'`,
			sanitizeTenant(tenant)), providerID).Scan(&rolling)
		if rolling > caps.Rolling14Day {
			violations = append(violations, fmt.Sprintf(
				"%d claims submitted by this filing party in a 14-day window exceeds the %d-claim cap (%s)",
				rolling, caps.Rolling14Day, capKey))
		}
	}

	result := map[string]any{
		"large_volume": true, "claims": numClaims, "review_class": capKey,
		"violations": violations, "policy": "Capitol Bridge v01.01.2026",
		"disposition": "INELIGIBLE (resubmission permitted once cured)",
	}
	vj, _ := json.Marshal(map[string]any{
		"volume_checked_at": time.Now().UTC().Format(time.RFC3339),
		"volume_violations": violations,
		"volume_large":      true,
	})
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(
		`UPDATE tenant_%s.cases SET details = details || $2::jsonb, updated_at=now() WHERE id=$1`,
		sanitizeTenant(tenant)), caseID, string(vj))
	if len(violations) > 0 {
		s.logActivity(r.Context(), tenant, caseID, "VOLUME_RULE_VIOLATION",
			fmt.Sprintf("Large-volume policy: %s — per policy the dispute is ineligible; resubmission permitted once cured",
				strings.Join(violations, "; ")))
	}
	return result
}
