// refunds_due.go — the refunds the case workflow has ordered and Finance has not made yet.
//
// When no complete filing packet arrives within 7 days of the Reviewer's outreach, the AHCA workflow flags a
// potential refund (REFUND_REVIEW_DUE) and waits. Only when a person chooses Close and refund (REFUND_APPROVED) does
// it close the case "Closed-Refunded", tell the filer and notify Finance (REFUND_DUE on the case timeline). The money only moves
// when Finance refunds the initial-fee invoice (settleInvoice REFUND -> refunds.go: a Stripe refund of the fee for a
// card payment, otherwise recorded as refunded outside the platform). Until now nothing listed which of those refunds were still outstanding -- Finance had only the
// notification. GET /reports/refunds-due is that list: Financials shows it, with a Refund button per row.
package main

import (
	"fmt"
	"net/http"
	"time"
)

// refundDue is one outstanding refund.
type refundDue struct {
	CaseID      string     `json:"case_id"`
	CaseNumber  string     `json:"case_number"`
	Filer       string     `json:"filer"`
	FilerEmail  string     `json:"filer_email"`
	InvoiceID   string     `json:"invoice_id"`
	InvoiceNo   string     `json:"invoice_no"`
	Party       string     `json:"party"`
	AmountCents int64      `json:"amount_cents"`
	PaidAt      *time.Time `json:"paid_at"`
	ByCard      bool       `json:"by_card"`    // a Stripe payment exists: Refund issues the Stripe refund
	PaidVia     string     `json:"paid_via"`   // "card" | "check / ACH / other"
	OrderedAt   *time.Time `json:"ordered_at"` // when the workflow ordered the refund (REFUND_DUE), if recorded
	DaysWaiting int        `json:"days_waiting"`
	State       string     `json:"state"`        // DUE | IN_PROGRESS (a card refund Stripe has not confirmed yet)
	LastFailure string     `json:"last_failure"` // why the last attempt failed, if one did
}

// daysSince counts whole days from t to now (0 when t is unknown or in the future). Pure, so it is tested.
func daysSince(t *time.Time, now time.Time) int {
	if t == nil || t.After(now) {
		return 0
	}
	return int(now.Sub(*t).Hours() / 24)
}

// refundsDueSQL lists the cases the workflow closed for a refund whose initial fee is still PAID, or being refunded.
const refundsDueSQL = `
	SELECT c.id::text, c.case_number,
	       coalesce(nullif(c.details->>'requester_org',''), c.details->>'initiating_party', ''),
	       coalesce(c.details->>'requester_email', ''),
	       i.id::text, i.invoice_no, i.party, i.amount_cents, i.paid_at::timestamptz,
	       EXISTS (SELECT 1 FROM public.payments p WHERE p.tenant=$1 AND p.invoice_id=i.id AND p.status='PAID'
	               AND p.provider <> 'check' AND coalesce(p.payment_intent,'') <> ''),
	       (SELECT max(a.created_at) FROM public.case_activities a
	         WHERE a.tenant=$1 AND a.case_id=c.id::text AND a.type='REFUND_DUE'),
	       i.status,
	       coalesce((SELECT rf.failure FROM public.refunds rf WHERE rf.tenant=$1 AND rf.invoice_id=i.id AND rf.kind='FEE'
	                  ORDER BY rf.created_at DESC LIMIT 1), '')
	  FROM tenant_%s.cases c
	  JOIN public.invoices i ON i.tenant=$1 AND i.case_id=c.id::text AND i.kind='INITIAL_FEE' AND i.status IN ('PAID','REFUND_PENDING')
	 WHERE c.internal_status = 'Closed-Refunded'
	 ORDER BY 11 ASC NULLS LAST, c.case_number
	 LIMIT 500`

func (s *server) refundsDue(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, financialReadRoles...) {
		http.Error(w, `{"error":"forbidden: requires a financial/management role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(refundsDueSQL, sanitizeTenant(tenant)), tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	now := time.Now()
	out := []refundDue{}
	var total int64
	for rows.Next() {
		var d refundDue
		if err := rows.Scan(&d.CaseID, &d.CaseNumber, &d.Filer, &d.FilerEmail, &d.InvoiceID, &d.InvoiceNo, &d.Party,
			&d.AmountCents, &d.PaidAt, &d.ByCard, &d.OrderedAt, &d.State, &d.LastFailure); err != nil {
			continue
		}
		d.PaidVia = map[bool]string{true: "card", false: "check / ACH / other"}[d.ByCard]
		d.State = map[string]string{"REFUND_PENDING": "IN_PROGRESS"}[d.State]
		if d.State == "" {
			d.State = "DUE"
		}
		since := d.OrderedAt
		if since == nil {
			since = d.PaidAt
		}
		d.DaysWaiting = daysSince(since, now)
		total += d.AmountCents
		out = append(out, d)
	}
	// Potential refunds: the workflow found no complete packet 7 days after outreach and asked a person to decide.
	// Nothing is closed or refunded until someone chooses Close and refund on the case.
	potential := []map[string]any{}
	if prow, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT c.id::text, c.case_number, coalesce(nullif(c.details->>'requester_org',''), c.details->>'initiating_party', ''),
		       coalesce(i.amount_cents, 0), max(a.created_at)
		  FROM tenant_%s.cases c
		  JOIN public.case_activities a ON a.tenant=$1 AND a.case_id=c.id::text AND a.type='REFUND_REVIEW_DUE'
		  LEFT JOIN public.invoices i ON i.tenant=$1 AND i.case_id=c.id::text AND i.kind='INITIAL_FEE' AND i.status='PAID'
		 WHERE c.status <> 'CLOSED' AND coalesce(c.internal_status,'') NOT LIKE 'Closed%%'
		   AND coalesce(c.program_dates->>'packet_complete_at','') = ''
		 GROUP BY 1,2,3,4 ORDER BY 5 LIMIT 500`, sanitizeTenant(tenant)), tenant); err == nil {
		for prow.Next() {
			var id, num, filer string
			var amt int64
			var at time.Time
			if prow.Scan(&id, &num, &filer, &amt, &at) == nil {
				potential = append(potential, map[string]any{"case_id": id, "case_number": num, "filer": filer, "amount_cents": amt,
					"flagged_at": at, "days_waiting": daysSince(&at, now)})
			}
		}
		prow.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"potential": potential, "can_approve": hasAnyRole(p, signalRoleFloors["REFUND_APPROVED"]...),
		"refunds": out, "count": len(out), "total_cents": total,
		// Only Finance and the admins can make a refund (settleInvoice); everyone else reading the list sees it.
		"can_refund": hasAnyRole(p, "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN"),
	})
}
