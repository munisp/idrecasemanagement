package main

// flreport.go — FL AHCA contract reports: the 16-column weekly (A–P, exact
// order) and the 33-field monthly, per the PLUM CRM field criteria.
//
// Field sourcing: FL workflow data lives where the workflow records it —
// core columns on tenant_fl.cases, workflow dates in program_dates (set via
// POST /cases/{id}/program-date) and free-form attributes in details.
// Reports NEVER invent a value: a date nobody recorded renders "N/A", an
// outcome not yet determined renders "TBD – case in process", exactly as the
// field criteria require. Internal-only fields (review notes, rationale,
// RFI details, payment contact) are never emitted.

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var flReportRoles = []string{"CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR"}

type flCaseRow struct {
	CaseNumber, Status, CaseID string
	OpenedAt                   time.Time
	DisputedCents              int64
	Details, Dates             map[string]any
	ClaimsSubmitted            int
	InitialFee                 *flFee
	Invoices                   []flInv
}

type flFee struct {
	WhoPaid, Org, Email string
	AmountCents         int64
	PaidAt              string
}

type flInv struct {
	Party, Kind, Status string
	AmountCents         int64
	PaidAt              string
}

func dget(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return s
			}
		}
	}
	return ""
}

func mmdd(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "N/A"
	}
	if len(raw) >= 10 {
		if t, err := time.Parse("2006-01-02", raw[:10]); err == nil {
			return t.Format("01/02/2006")
		}
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Format("01/02/2006")
	}
	// addDays returns already-formatted MM/DD/YYYY — pass through.
	if t, err := time.Parse("01/02/2006", raw); err == nil {
		return t.Format("01/02/2006")
	}
	return "N/A"
}

func addDays(raw string, days int) string {
	if len(raw) >= 10 {
		if t, err := time.Parse("2006-01-02", raw[:10]); err == nil {
			return t.AddDate(0, 0, days).Format("01/02/2006")
		}
	}
	return "N/A"
}

func money(c int64) string {
	if c == 0 {
		return "N/A"
	}
	return "$" + strconv.FormatFloat(float64(c)/100, 'f', 2, 64)
}

// flReportCases assembles every case with its details, dates, claim counts,
// initial fee payment, and invoices.
func (s *server) flReportCases(r *http.Request, tenant, from, to string) ([]flCaseRow, error) {
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT id, case_number, status, opened_at, coalesce(disputed_amount_cents,0),
		       coalesce(details,'{}'::jsonb), coalesce(program_dates,'{}'::jsonb)
		FROM tenant_%s.cases
		WHERE ($2='' OR opened_at >= $2::date) AND ($3='' OR opened_at < $3::date + 1)
		ORDER BY opened_at`, sanitizeTenant(tenant)), tenant, from, to)
	if err != nil {
		return nil, err
	}
	out := []flCaseRow{}
	for _, row := range rows {
		cr := flCaseRow{
			CaseID: fmt.Sprint(row["id"]), CaseNumber: fmt.Sprint(row["case_number"]),
			Status: fmt.Sprint(row["status"]), DisputedCents: toInt64(row["disputed_amount_cents"]),
		}
		if t, ok := row["opened_at"].(time.Time); ok {
			cr.OpenedAt = t
		}
		cr.Details = map[string]any{}
		if d, ok := row["details"].(map[string]any); ok {
			cr.Details = d
		}
		cr.Dates = map[string]any{}
		if d, ok := row["program_dates"].(map[string]any); ok {
			cr.Dates = d
		}
		// claims submitted: explicit detail wins, else claim-line count
		if n := dget(cr.Details, "claims_submitted", "number_of_claims_submitted"); n != "" {
			cr.ClaimsSubmitted, _ = strconv.Atoi(n)
		} else {
			_ = s.db.QueryRow(r.Context(),
				`SELECT count(*) FROM public.claims WHERE tenant=$1 AND case_id=$2`,
				tenant, cr.CaseID).Scan(&cr.ClaimsSubmitted)
		}
		// initial fee payment (earliest settled intake payment)
		var f flFee
		var amt *int64
		var paidAt, who, org, email *string
		if err := s.db.QueryRow(r.Context(), `
			SELECT amount_cents, to_char(coalesce(paid_at,updated_at)::date,'YYYY-MM-DD'),
			       coalesce(payer_name,''), coalesce(payer_org,''), coalesce(payer_email,'')
			FROM public.payments WHERE tenant=$1 AND case_id=$2 AND status='PAID'
			ORDER BY created_at LIMIT 1`, tenant, cr.CaseID).
			Scan(&amt, &paidAt, &who, &org, &email); err == nil && amt != nil {
			f.AmountCents = *amt
			if paidAt != nil {
				f.PaidAt = *paidAt
			}
			if who != nil {
				f.WhoPaid = *who
			}
			if org != nil {
				f.Org = *org
			}
			if email != nil {
				f.Email = *email
			}
			cr.InitialFee = &f
		}
		// invoices per party
		invRows, _ := s.queryRows(r, `
			SELECT party, kind, status, amount_cents, coalesce(to_char(paid_at,'YYYY-MM-DD'),'')
			FROM public.invoices WHERE tenant=$1 AND case_id=$2 AND kind<>'INITIAL_FEE'
			ORDER BY party`, tenant, cr.CaseID)
		for _, iv := range invRows {
			cr.Invoices = append(cr.Invoices, flInv{
				Party: fmt.Sprint(iv["party"]), Kind: fmt.Sprint(iv["kind"]),
				Status: fmt.Sprint(iv["status"]), AmountCents: toInt64(iv["amount_cents"]),
				PaidAt: fmt.Sprint(iv["paid_at"]),
			})
		}
		out = append(out, cr)
	}
	return out, nil
}

func (c *flCaseRow) received() string {
	if v := dget(c.Dates, "received_at"); v != "" {
		return v
	}
	return c.OpenedAt.Format("2006-01-02")
}
func (c *flCaseRow) agencyStatus() string {
	if v := dget(c.Details, "fl_ahca_status", "agency_status"); v != "" {
		return v
	}
	return c.Status
}
func (c *flCaseRow) outcome() string {
	if v := dget(c.Details, "case_outcome", "outcome"); v != "" {
		return v
	}
	return "TBD – case in process"
}

var flWeeklyHeaders = []string{
	"FL AHCA Status", "Case Number", "Date Received",
	"Date Acknowledgment Letter Sent to Provider",
	"Number of Claims Submitted with Initial Filing",
	"Date Provider's Response is Due", "Date Provider Responded",
	"Date Acknowledgment Letter Sent to Health Plan",
	"Date Health Plan's Response is Due", "Date Plan Responded",
	"Number of Claims/Cases Reviewed",
	"Date Recommendation is Due to the Agency",
	"Date the Recommendation is Sent to the Agency",
	"Case Outcome", "Out-of-Network Provider (OON)",
	"Comments/Notes (external for Florida)",
}

func (c *flCaseRow) weeklyRow() []string {
	ackProv := dget(c.Dates, "ack_provider_letter_at", "date_acknowledgment_provider")
	ackPlan := dget(c.Dates, "plan_notified_at", "date_acknowledgment_plan")
	// Provider response due: ack+15 ONLY when an RFI or estimate went with the
	// acceptance letter; otherwise equal to the acknowledgment date itself.
	provDue := ackProv
	if dget(c.Dates, "estimate_sent_at", "rfi_sent_at") != "" {
		provDue = addDays(ackProv, 15)
		if provDue == "N/A" {
			provDue = ""
		}
	}
	reviewed := dget(c.Details, "claims_reviewed", "number_of_claims_reviewed")
	if reviewed == "" {
		reviewed = strconv.Itoa(c.ClaimsSubmitted)
	}
	return []string{
		c.agencyStatus(), c.CaseNumber, mmdd(c.received()), mmdd(ackProv),
		strconv.Itoa(c.ClaimsSubmitted),
		mmdd(provDue), mmdd(dget(c.Dates, "provider_responded_at")),
		mmdd(ackPlan), mmdd(addDays(ackPlan, 15)), mmdd(dget(c.Dates, "plan_responded_at")),
		reviewed,
		mmdd(addDays(c.received(), 60)), mmdd(dget(c.Dates, "recommendation_sent_at")),
		c.outcome(),
		func() string { if v := dget(c.Details, "oon_provider", "out_of_network"); v != "" { return v }; return "No" }(),
		dget(c.Details, "external_comments", "comments_florida"),
	}
}

var flMonthlyHeaders = []string{
	// 8 shared with weekly
	"FL AHCA Status", "Case Number", "Date Received",
	"Date Acknowledgment Letter Sent to Provider", "Date Plan Responded",
	"Date the Recommendation is Sent to the Agency", "Case Outcome",
	"Comments/Notes (external for Florida)",
	// 25 monthly-only
	"Provider Organization", "Provider Contact", "Provider Street Address",
	"Provider City", "Provider State", "Provider Postal Code",
	"Health Plan", "Line of Business", "Disputed Issue", "Disputed Amount",
	"Initial Review Fee Who Paid", "Initial Review Fee Amount Paid",
	"Initial Review Fee Date of Payment", "Case Status",
	"Date Final Order Sent", "Final Amount Awarded", "Party Billed",
	"Withdrawal/Dismissed Reason", "Invoice Number",
	"Health Plan Review Cost", "Health Plan Paid", "Health Plan Date Paid",
	"Provider Review Cost", "Provider Paid", "Provider Date Paid",
}

func invCols(invs []flInv, party string) (cost, paid, paidAt string) {
	cost, paid, paidAt = "N/A", "N/A", "N/A"
	for _, iv := range invs {
		if !strings.EqualFold(iv.Party, party) {
			continue
		}
		cost = money(iv.AmountCents)
		switch iv.Status {
		case "PAID":
			paid, paidAt = "Yes", mmdd(iv.PaidAt)
		case "VOID":
			paid = "N/A"
		default:
			paid = "No"
		}
	}
	return
}

func (c *flCaseRow) monthlyRow() []string {
	ackProv := dget(c.Dates, "ack_provider_letter_at", "date_acknowledgment_provider")
	feeWho, feeAmt, feeDate := "N/A", "N/A", "N/A"
	if c.InitialFee != nil {
		feeWho = c.InitialFee.WhoPaid
		if feeWho == "" {
			feeWho = c.InitialFee.Org
		}
		if feeWho == "" {
			feeWho = "N/A"
		}
		feeAmt, feeDate = money(c.InitialFee.AmountCents), mmdd(c.InitialFee.PaidAt)
	}
	hpCost, hpPaid, hpDate := invCols(c.Invoices, "HEALTH_PLAN")
	prCost, prPaid, prDate := invCols(c.Invoices, "PROVIDER")
	na := func(v string) string {
		if v == "" {
			return "N/A"
		}
		return v
	}
	return []string{
		c.agencyStatus(), c.CaseNumber, mmdd(c.received()), mmdd(ackProv),
		mmdd(dget(c.Dates, "plan_responded_at")),
		mmdd(dget(c.Dates, "recommendation_sent_at")), c.outcome(),
		dget(c.Details, "external_comments", "comments_florida"),
		na(dget(c.Details, "provider_org", "provider_organization", "org")),
		na(dget(c.Details, "provider_contact", "contact_name")),
		na(dget(c.Details, "provider_street", "provider_address")),
		na(dget(c.Details, "provider_city")), na(dget(c.Details, "provider_state")),
		na(dget(c.Details, "provider_postal", "provider_zip")),
		na(dget(c.Details, "health_plan", "plan_name")),
		na(dget(c.Details, "line_of_business")),
		na(dget(c.Details, "disputed_issue")),
		money(c.DisputedCents),
		feeWho, feeAmt, feeDate,
		c.Status,
		mmdd(dget(c.Dates, "final_order_sent_at")),
		func() string {
			if v := dget(c.Details, "final_amount_awarded_cents"); v != "" {
				n, _ := strconv.ParseInt(v, 10, 64)
				return money(n)
			}
			return "N/A"
		}(),
		na(dget(c.Details, "party_billed")),
		na(dget(c.Details, "withdrawal_reason", "dismissed_reason")),
		c.CaseNumber, // invoice number ALWAYS equals case number
		hpCost, hpPaid, hpDate, prCost, prPaid, prDate,
	}
}

func (s *server) flReportCSV(w http.ResponseWriter, r *http.Request, headers []string, name string, rowFn func(*flCaseRow) []string) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, flReportRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if to == "" {
		to = time.Now().Format("2006-01-02")
	}
	cases, err := s.flReportCases(r, tenant, from, to)
	if err != nil {
		http.Error(w, `{"error":"query"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s-%s.csv"`, name, tenant, to))
	cw := csv.NewWriter(w)
	_ = cw.Write(headers)
	for i := range cases {
		_ = cw.Write(rowFn(&cases[i]))
	}
	cw.Flush()
}

// GET /reports/fl/weekly — contract task 2.4.2 (due Mondays, previous week).
func (s *server) flWeeklyReport(w http.ResponseWriter, r *http.Request) {
	s.flReportCSV(w, r, flWeeklyHeaders, "fl-weekly-report", func(c *flCaseRow) []string { return c.weeklyRow() })
}

// GET /reports/fl/monthly — contract task 2.4.3 (due the 10th, previous month).
func (s *server) flMonthlyReport(w http.ResponseWriter, r *http.Request) {
	s.flReportCSV(w, r, flMonthlyHeaders, "fl-monthly-report", func(c *flCaseRow) []string { return c.monthlyRow() })
}
