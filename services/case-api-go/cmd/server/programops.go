package main

// Program operations: invoicing/receivables with dual parties (G8), bulk
// claims import for large-volume disputes (G10), pre-case intake requests
// with refund windows (G12), contract deliverables schedule (G7), and
// agency opt-out decisions (G14). All amounts in cents; invoice number =
// case number per FL AHCA program semantics, configurable per tenant.

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	var caseID string
	err := s.db.QueryRow(r.Context(), `
		UPDATE public.invoices SET status=$3, remittance_ref=$4,
		       paid_at = CASE WHEN $3='PAID' THEN now()::date ELSE paid_at END
		WHERE tenant=$1 AND id=$2 AND status='OPEN' RETURNING case_id`,
		tenant, invID, status, in.RemittanceRef).Scan(&caseID)
	if err != nil {
		http.Error(w, `{"error":"not open or not found"}`, http.StatusConflict)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "INVOICE_"+status,
		fmt.Sprintf("Invoice %s marked %s%s", invID, status, orDash(" — remittance "+in.RemittanceRef)))
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
	writeJSON(w, http.StatusOK, map[string]any{"imported": len(in.Claims)})
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
// requested, awaiting docs/fee). outreach_at anchors the refund window.
func (s *server) createIntake(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Email       string `json:"email"`
		ContactName string `json:"contact_name"`
		Org         string `json:"org"`
		Notes       string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Email == "" {
		http.Error(w, `{"error":"email required"}`, http.StatusBadRequest)
		return
	}
	var id string
	_ = s.db.QueryRow(r.Context(), `
		INSERT INTO public.intake_requests (tenant, email, contact_name, org, notes, outreach_at)
		VALUES ($1,$2,$3,$4,$5, now()) RETURNING id`,
		tenant, in.Email, in.ContactName, in.Org, in.Notes).Scan(&id)
	writeJSON(w, http.StatusOK, map[string]any{"intake_id": id, "status": "INSTRUCTED"})
}

// advanceIntake moves an intake through DOCS_RECEIVED → PAID → CONVERTED, or
// closes it CLOSED_REFUNDED (enforcing the program refund window when set).
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
		SELECT id, email, contact_name, org, status, outreach_at, case_id, created_at
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
			fmt.Sprintf(`UPDATE tenant_%s.cases SET internal_status='Opted Out', updated_at=now() WHERE id=$1`, sanitizeTenant(tenant)), caseID)
	}
	s.logActivity(r.Context(), tenant, caseID, "OPT_OUT_DECISION",
		fmt.Sprintf("Opt-out eligibility: %v%s (by %s)", in.Eligible, orDash(" — "+in.Rationale), p.Subject))
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}
