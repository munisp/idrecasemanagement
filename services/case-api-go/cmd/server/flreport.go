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

// The Reviewer is not here: the Reports page is the PM's, and this list had
// let a Reviewer pull every report by address while the page was hidden.
var flReportRoles = []string{"PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "STATE_AUDITOR"}

type flCaseRow struct {
	CaseNumber, Status, CaseID string
	// Internal / Agency: the program's two status fields (the report's
	// "Case Status" and "FL AHCA Status"). Status is the engine's own code
	// and is never reported.
	Internal, Agency string
	// Provider / plan as held on the linked accounts.
	ProviderOrg, ProviderContact, HealthPlan string
	ProviderAddr                             map[string]any
	ClaimsReviewed                           int
	OpenedAt                                 time.Time
	DisputedCents                            int64
	Details, Dates                           map[string]any
	ClaimsSubmitted                          int
	InitialFee                               *flFee
	Invoices                                 []flInv
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
	// Which cases a report covers: everything that was open during the
	// period -- cases still open, plus cases opened or closed within it. A
	// case that has not been paid for is not a case yet and is left out.
	//
	// Every computed column carries an alias: queryRows keys rows by column
	// name, and three unnamed coalesce(...) columns all landed on the one key
	// "coalesce", which is why details, dates and amounts used to come back
	// empty for every case.
	t := sanitizeTenant(tenant)
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT c.id::text AS id, c.case_number, c.status, c.opened_at,
		       coalesce(c.internal_status,'') AS internal_status, coalesce(c.agency_status,'') AS agency_status,
		       coalesce(c.disputed_amount_cents,0) AS disputed_amount_cents, coalesce(c.num_claims,0) AS num_claims,
		       coalesce(c.details,'{}'::jsonb) AS details, coalesce(c.program_dates,'{}'::jsonb) AS program_dates,
		       coalesce(pa.legal_name,'') AS provider_org, coalesce(pa.address,'{}'::jsonb) AS provider_address,
		       coalesce((SELECT name FROM public.contacts k WHERE k.account_id = pa.id ORDER BY k.created_at LIMIT 1),'') AS provider_contact,
		       coalesce(ha.legal_name,'') AS health_plan
		FROM tenant_%s.cases c
		LEFT JOIN public.accounts pa ON pa.id::text = c.provider_id AND pa.tenant = $3
		LEFT JOIN public.accounts ha ON ha.id::text = c.payer_id AND ha.tenant = $3
		WHERE c.status <> 'AWAITING_PAYMENT'
		  AND ( coalesce(c.agency_status,'') <> 'Closed'
		        OR (($1::text='' OR c.updated_at >= NULLIF($1::text,'')::date) AND ($2::text='' OR c.updated_at < NULLIF($2::text,'')::date + 1))
		        OR (($1::text='' OR c.opened_at  >= NULLIF($1::text,'')::date) AND ($2::text='' OR c.opened_at  < NULLIF($2::text,'')::date + 1)) )
		  AND ($2::text='' OR c.opened_at < NULLIF($2::text,'')::date + 1)
		ORDER BY c.opened_at`, t), from, to, tenant)
	if err != nil {
		return nil, err
	}
	asMap := func(v any) map[string]any {
		if m, ok := v.(map[string]any); ok {
			return m
		}
		return map[string]any{}
	}
	out := []flCaseRow{}
	for _, row := range rows {
		cr := flCaseRow{
			CaseID: fmt.Sprint(row["id"]), CaseNumber: fmt.Sprint(row["case_number"]),
			Status: fmt.Sprint(row["status"]), DisputedCents: toInt64(row["disputed_amount_cents"]),
			Internal: fmt.Sprint(row["internal_status"]), Agency: fmt.Sprint(row["agency_status"]),
			ProviderOrg: fmt.Sprint(row["provider_org"]), ProviderContact: fmt.Sprint(row["provider_contact"]),
			HealthPlan: fmt.Sprint(row["health_plan"]), ProviderAddr: asMap(row["provider_address"]),
			Details: asMap(row["details"]), Dates: asMap(row["program_dates"]),
		}
		if t, ok := row["opened_at"].(time.Time); ok {
			cr.OpenedAt = t
		}
		// claims submitted: what staff recorded wins, then the case's own
		// count, then the imported claim lines.
		// (claims_detected -- what the document reader found -- is NOT used
		// here: the report carries the filing packet's count as the Reviewer
		// recorded it. The detected count is offered to the Reviewer instead.)
		if n := dget(cr.Details, "claims_submitted", "number_of_claims_submitted"); n != "" {
			cr.ClaimsSubmitted, _ = strconv.Atoi(n)
		} else if n := int(toInt64(row["num_claims"])); n > 0 {
			cr.ClaimsSubmitted = n
		} else {
			_ = s.db.QueryRow(r.Context(),
				`SELECT count(*) FROM public.claims WHERE tenant=$1 AND case_id=$2`,
				tenant, cr.CaseID).Scan(&cr.ClaimsSubmitted)
		}
		if n := dget(cr.Details, "num_claims_reviewed", "claims_reviewed", "number_of_claims_reviewed"); n != "" {
			f, _ := strconv.ParseFloat(n, 64)
			cr.ClaimsReviewed = int(f)
		}
		// Initial fee: the paid initial-fee invoice is the record (it covers
		// card, check and ACH alike); the card payment row adds who paid.
		var f flFee
		var amt int64
		var paidAt string
		if err := s.db.QueryRow(r.Context(), `
			SELECT amount_cents, coalesce(to_char(paid_at,'YYYY-MM-DD'),'')
			FROM public.invoices WHERE tenant=$1 AND case_id=$2 AND kind='INITIAL_FEE' AND status='PAID'
			ORDER BY paid_at LIMIT 1`, tenant, cr.CaseID).Scan(&amt, &paidAt); err == nil {
			f.AmountCents, f.PaidAt = amt, paidAt
			_ = s.db.QueryRow(r.Context(), `
				SELECT coalesce(payer_name,''), coalesce(payer_org,''), coalesce(payer_email,'')
				FROM public.payments WHERE tenant=$1 AND case_id=$2 ORDER BY (status='PAID') DESC, created_at LIMIT 1`,
				tenant, cr.CaseID).Scan(&f.WhoPaid, &f.Org, &f.Email)
			if f.Org == "" {
				f.Org = dget(cr.Details, "requester_org")
			}
			cr.InitialFee = &f
		}
		// invoices per party
		invRows, _ := s.queryRows(r, `
			SELECT party, kind, status, amount_cents, coalesce(to_char(paid_at,'YYYY-MM-DD'),'') AS paid_at
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

// flReportedStatuses: the only FL AHCA Status values the reports may carry
// (field rules). The case itself can hold two more -- "Closed" and "Other" --
// and may hold none before its packet is confirmed; reportAgencyStatus folds
// those into the five. The stored status is never changed by reporting.
var flReportedStatuses = []string{"Pending Initial Review", "Awaiting Provider Response", "Awaiting Plan Response", "Under Review", "Decided"}

func reportAgencyStatus(agency string) string {
	switch agency {
	case "Closed":
		// Every closure follows a decision (a determination, or a finding
		// that ends the case); the Case Outcome column says which.
		return "Decided"
	case "Other", "":
		// "Other" is the AOR hold, which happens during the initial review;
		// a paid case with no status yet is waiting for that review too.
		return "Pending Initial Review"
	}
	if contains(flReportedStatuses, agency) {
		return agency
	}
	return "Pending Initial Review"
}

func (c *flCaseRow) agencyStatus() string {
	agency := c.Agency
	if agency == "" {
		agency = dget(c.Details, "fl_ahca_status", "agency_status")
	}
	return reportAgencyStatus(agency) // never the engine's own status code
}

// externalNotes: the "Comments/Notes (External for Florida)" notes field.
func (c *flCaseRow) externalNotes() string {
	if rn, ok := c.Details["review_notes"].(map[string]any); ok {
		if ext, ok := rn["external"].(map[string]any); ok {
			if s := strings.TrimSpace(fmt.Sprint(ext["text"])); s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return dget(c.Details, "external_comments", "comments_florida")
}

// The workflow dates, under the names the system records them (the older
// names are still read for cases that carry them).
func (c *flCaseRow) ackProvider() string {
	return dget(c.Dates, "date_acknowledgment_letter_sent_to_provider", "ack_provider_letter_at", "date_acknowledgment_provider")
}
func (c *flCaseRow) ackPlan() string {
	return dget(c.Dates, "date_acknowledgment_letter_sent_to_health_plan", "plan_notified_at", "date_acknowledgment_plan")
}
func (c *flCaseRow) planResponded() string {
	return dget(c.Dates, "date_plan_responded", "plan_responded_at")
}
func (c *flCaseRow) recommendationSent() string {
	return dget(c.Dates, "date_recommendation_sent_to_agency", "recommendation_sent_at")
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
	ackProv, ackPlan := c.ackProvider(), c.ackPlan()
	// Provider response due: the recorded date when there is one; otherwise
	// ack+15 ONLY when an RFI or estimate went with the acceptance letter,
	// else equal to the acknowledgment date itself.
	provDue := dget(c.Dates, "date_provider_response_due")
	if provDue == "" {
		provDue = ackProv
		if dget(c.Dates, "estimate_sent_at", "rfi_sent_at") != "" {
			provDue = addDays(ackProv, 15)
			if provDue == "N/A" {
				provDue = ""
			}
		}
	}
	planDue := dget(c.Dates, "date_health_plan_response_due")
	if planDue == "" {
		planDue = addDays(ackPlan, 15)
	}
	// Counts nobody recorded are "N/A", not a zero that reads as a fact.
	count := func(n int) string {
		if n <= 0 {
			return "N/A"
		}
		return strconv.Itoa(n)
	}
	reviewed := count(c.ClaimsReviewed)
	if reviewed == "N/A" {
		reviewed = dget(c.Details, "claims_reviewed", "number_of_claims_reviewed")
		if reviewed == "" {
			reviewed = "N/A"
		}
	}
	return []string{
		c.agencyStatus(), c.CaseNumber, mmdd(c.received()), mmdd(ackProv),
		count(c.ClaimsSubmitted),
		mmdd(provDue), mmdd(dget(c.Dates, "date_provider_responded", "provider_responded_at")),
		mmdd(ackPlan), mmdd(planDue), mmdd(c.planResponded()),
		reviewed,
		mmdd(addDays(c.received(), 60)), mmdd(c.recommendationSent()),
		c.outcome(),
		func() string {
			if v := dget(c.Details, "oon_provider", "out_of_network"); v != "" {
				return v
			}
			return "No"
		}(),
		c.externalNotes(),
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

// first returns the first non-empty string.
func first(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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
	ackProv := c.ackProvider()
	feeWho, feeAmt, feeDate := "N/A", "N/A", "N/A"
	if c.InitialFee != nil {
		feeWho = c.InitialFee.WhoPaid
		if feeWho == "" {
			feeWho = c.InitialFee.Org
		}
		if feeWho == "" {
			// The filing party pays the initial fee.
			switch dget(c.Details, "filing_party_type") {
			case "HEALTH_PLAN":
				feeWho = "Health Plan"
			case "PROVIDER":
				feeWho = "Provider"
			default:
				feeWho = "N/A"
			}
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
		mmdd(c.planResponded()),
		mmdd(c.recommendationSent()), c.outcome(),
		c.externalNotes(),
		// Provider and plan: what staff typed on the case, else the linked
		// account, else (provider only) who asked for the dispute.
		na(first(dget(c.Details, "provider_org", "provider_organization", "org"), c.ProviderOrg, dget(c.Details, "initiating_party"), dget(c.Details, "requester_org"))),
		na(first(dget(c.Details, "provider_contact", "contact_name"), c.ProviderContact, dget(c.Details, "initiating_party_contact"), dget(c.Details, "requester_contact_name"))),
		na(first(dget(c.Details, "provider_street", "provider_address"), dget(c.ProviderAddr, "street", "line1", "address"))),
		na(first(dget(c.Details, "provider_city"), dget(c.ProviderAddr, "city"))),
		na(first(dget(c.Details, "provider_state"), dget(c.ProviderAddr, "state"))),
		na(first(dget(c.Details, "provider_postal", "provider_zip"), dget(c.ProviderAddr, "postal", "zip", "postal_code"))),
		na(first(dget(c.Details, "health_plan", "plan_name"), c.HealthPlan)),
		na(dget(c.Details, "line_of_business")),
		na(dget(c.Details, "disputed_issue")),
		money(c.DisputedCents),
		feeWho, feeAmt, feeDate,
		na(c.Internal),
		mmdd(dget(c.Dates, "date_final_order_sent", "final_order_sent_at")),
		func() string {
			if v := dget(c.Details, "final_amount_awarded_cents"); v != "" {
				n, _ := strconv.ParseInt(v, 10, 64)
				return money(n)
			}
			return "N/A"
		}(),
		na(dget(c.Details, "party_billed")),
		na(dget(c.Details, "withdrawal_dismissed_reason", "withdrawal_reason", "dismissed_reason")),
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
	if from == "" && to == "" {
		from, to = flDefaultPeriod(name, time.Now().UTC())
	}
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

// flDefaultPeriod is the period a report covers when none is asked for: the
// weekly report covers the previous Monday-to-Sunday week, the monthly report
// the previous calendar month.
func flDefaultPeriod(name string, now time.Time) (from, to string) {
	day := now.Truncate(24 * time.Hour)
	if strings.Contains(name, "monthly") {
		firstOfThis := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
		return firstOfThis.AddDate(0, -1, 0).Format("2006-01-02"), firstOfThis.AddDate(0, 0, -1).Format("2006-01-02")
	}
	monday := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	return monday.AddDate(0, 0, -7).Format("2006-01-02"), monday.AddDate(0, 0, -1).Format("2006-01-02")
}
