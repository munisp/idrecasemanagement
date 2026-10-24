// unapplied.go — money received that pays no fee, and what Finance does with it.
//
// A check for more than its fee, or a payment for a fee already paid, is money in the bank that is not fee
// revenue: it is recorded as PAYMENT_UNAPPLIED (checks.go, mojaloop.go) and kept out of Collected. Finance resolves
// each one, all at once or in parts:
//
//   - refund: the money goes back to the payer by check or ACH (UNAPPLIED_RETURNED, with the reference; the
//     ledger moves that part back out of escrow);
//   - apply:  it pays another open fee, in full (UNAPPLIED_APPLIED, plus that fee's PAYMENT_PAID and everything a
//     fee being paid sets off -- the case opens, rules fire, the checklist follows).
//
// A resolution names the not-applied amount it resolves (ref "unapplied:<event id>"), so what is left of each
// one is its amount less its resolutions, and the Financials "Not applied" figure is the sum of what is left.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// unappliedItem is one amount received that pays no fee, with what is left of it.
type unappliedItem struct {
	ID          int64     `json:"id"`
	CaseID      string    `json:"case_id"`
	CaseNumber  string    `json:"case_number"`
	InvoiceID   string    `json:"invoice_id"`
	InvoiceNo   string    `json:"invoice_no"`
	Source      string    `json:"source"` // the check / transfer it came with
	AmountCents int64     `json:"amount_cents"`
	LeftCents   int64     `json:"left_cents"`
	At          time.Time `json:"at"`
}

// unappliedLeft is what is left of an amount after its resolutions. Pure, so it is tested.
func unappliedLeft(amount int64, resolved []int64) int64 {
	left := amount
	for _, r := range resolved {
		left -= r
	}
	if left < 0 {
		return 0
	}
	return left
}

const unappliedSQL = `
	SELECT e.id, e.case_id, coalesce(e.invoice_id::text,''), coalesce(i.invoice_no,''), coalesce(e.ref,''), e.amount_cents,
	       e.amount_cents - coalesce((SELECT sum(x.amount_cents) FROM public.financial_events x
	                                   WHERE x.tenant=e.tenant AND x.kind IN ('UNAPPLIED_RETURNED','UNAPPLIED_APPLIED')
	                                     AND x.ref = 'unapplied:' || e.id), 0),
	       e.created_at
	  FROM public.financial_events e
	  LEFT JOIN public.invoices i ON i.tenant=e.tenant AND i.id=e.invoice_id
	 WHERE e.tenant=$1 AND e.kind='PAYMENT_UNAPPLIED'`

// listUnapplied: GET /reports/unapplied — the amounts with something left to resolve, oldest first.
func (s *server) listUnapplied(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, financialReadRoles...) {
		http.Error(w, `{"error":"forbidden: requires a financial/management role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), unappliedSQL+` ORDER BY e.created_at DESC`, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []unappliedItem{}
	var total int64
	for rows.Next() {
		var it unappliedItem
		if rows.Scan(&it.ID, &it.CaseID, &it.InvoiceID, &it.InvoiceNo, &it.Source, &it.AmountCents, &it.LeftCents, &it.At) != nil || it.LeftCents <= 0 {
			continue
		}
		total += it.LeftCents
		out = append(out, it)
	}
	rows.Close()
	if len(out) > 0 {
		var ids []string
		for _, it := range out {
			ids = append(ids, it.CaseID)
		}
		nums := map[string]string{}
		if cr, err := s.db.Query(r.Context(), fmt.Sprintf(`SELECT id::text, case_number FROM tenant_%s.cases WHERE id::text = ANY($1)`, sanitizeTenant(tenant)), ids); err == nil {
			for cr.Next() {
				var id, n string
				if cr.Scan(&id, &n) == nil {
					nums[id] = n
				}
			}
			cr.Close()
		}
		for i := range out {
			out[i].CaseNumber = nums[out[i].CaseID]
		}
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total_cents": total,
		"can_resolve": hasAnyRole(p, "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN")})
}

// resolveUnapplied: POST /finance/unapplied/{id}/resolve {action: refund|apply, amount_cents?, reference?, invoice?}
// refund: amount_cents (default: all that is left) returned by check / ACH, reference required.
// apply:  invoice = an open fee's invoice number (or a case number with one open fee); pays it in full.
func (s *server) resolveUnapplied(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires FINANCE, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, `{"error":"bad id"}`, http.StatusBadRequest)
		return
	}
	var in struct {
		Action      string `json:"action"`
		AmountCents int64  `json:"amount_cents"`
		Reference   string `json:"reference"`
		Invoice     string `json:"invoice"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	// One resolution at a time per amount: the row lock holds others back until this one is written.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)
	var it unappliedItem
	if err := tx.QueryRow(ctx, `SELECT id, case_id, coalesce(invoice_id::text,''), coalesce(ref,''), amount_cents FROM public.financial_events
		WHERE tenant=$1 AND id=$2 AND kind='PAYMENT_UNAPPLIED' FOR UPDATE`, tenant, id).Scan(&it.ID, &it.CaseID, &it.InvoiceID, &it.Source, &it.AmountCents); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var resolved int64
	_ = tx.QueryRow(ctx, `SELECT coalesce(sum(amount_cents),0) FROM public.financial_events WHERE tenant=$1 AND kind IN ('UNAPPLIED_RETURNED','UNAPPLIED_APPLIED') AND ref=$2`,
		tenant, fmt.Sprintf("unapplied:%d", id)).Scan(&resolved)
	left := unappliedLeft(it.AmountCents, []int64{resolved})
	if left <= 0 {
		http.Error(w, `{"error":"nothing left to resolve on this amount"}`, http.StatusConflict)
		return
	}
	actor := displayName(p)
	ref := fmt.Sprintf("unapplied:%d", id)
	switch in.Action {
	case "refund":
		amt := in.AmountCents
		if amt == 0 {
			amt = left
		}
		if amt < 0 || amt > left {
			writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("refund at most %s — that is what is left", usd(left))})
			return
		}
		if strings.TrimSpace(in.Reference) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enter the check or ACH reference of the refund"})
			return
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.financial_events (tenant, case_id, invoice_id, kind, direction, amount_cents, party, ref, actor)
			VALUES ($1,$2,nullif($3,'')::uuid,'UNAPPLIED_RETURNED','OUT',$4,'',$5,$6)`, tenant, it.CaseID, it.InvoiceID, amt, ref, actor); err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		// The ledger: that part leaves escrow again, under its own key.
		s.postPaymentLedger(tenant, it.CaseID, fmt.Sprintf("unapplied-return:%d:%d", id, resolved), "", uint64(amt), true)
		s.logActivity(ctx, tenant, it.CaseID, "UNAPPLIED_REFUNDED",
			fmt.Sprintf("%s received but not applied (from %s) refunded to the payer by %s — ref %s", usd(amt), orDash(it.Source), actor, in.Reference))
		s.logAudit(ctx, tenant, it.CaseID, "UNAPPLIED_REFUNDED", map[string]any{"by": p.Subject, "unapplied_id": id, "amount_cents": amt, "reference": in.Reference})
		writeJSON(w, http.StatusOK, map[string]any{"status": "refunded", "amount_cents": amt, "left_cents": left - amt})
	case "apply":
		key := strings.TrimSpace(in.Invoice)
		if key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enter the invoice number (or the case number) of the fee to pay"})
			return
		}
		// The fee: by invoice number, or a case number that has exactly one open fee (never a guess between two).
		var invID, caseID, party string
		var feeAmt int64
		var matches int
		_ = tx.QueryRow(ctx, fmt.Sprintf(`
			SELECT count(*) FROM public.invoices i LEFT JOIN tenant_%s.cases c ON c.id::text = i.case_id
			WHERE i.tenant=$1 AND i.status='OPEN' AND (i.invoice_no=$2 OR c.case_number=$2)`, sanitizeTenant(tenant)), tenant, key).Scan(&matches)
		if matches == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no open fee found for %q", key)})
			return
		}
		if matches > 1 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("%q has %d open fees — enter the invoice number of the one to pay", key, matches)})
			return
		}
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
			SELECT i.id::text, i.case_id, coalesce(i.party,''), i.amount_cents FROM public.invoices i
			LEFT JOIN tenant_%s.cases c ON c.id::text = i.case_id
			WHERE i.tenant=$1 AND i.status='OPEN' AND (i.invoice_no=$2 OR c.case_number=$2)`, sanitizeTenant(tenant)), tenant, key).Scan(&invID, &caseID, &party, &feeAmt); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no open fee found for %q", key)})
			return
		}
		if feeAmt > left {
			writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("that fee is %s, more than the %s left — a fee is paid in full", usd(feeAmt), usd(left))})
			return
		}
		var claimed string
		if tx.QueryRow(ctx, `UPDATE public.invoices SET status='PAID', paid_at=now()::date, remittance_ref=$3 WHERE tenant=$1 AND id::text=$2 AND status='OPEN' RETURNING id::text`,
			tenant, invID, "applied from "+ref).Scan(&claimed) != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "that fee was paid a moment ago"})
			return
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.financial_events (tenant, case_id, invoice_id, kind, direction, amount_cents, party, ref, actor) VALUES
			($1,$2,nullif($3,'')::uuid,'UNAPPLIED_APPLIED','NONE',$4,'',$5,$6),
			($1,$7,$8::uuid,'PAYMENT_PAID','IN',$4,$9,$5,$6)`,
			tenant, it.CaseID, it.InvoiceID, feeAmt, ref, actor, caseID, invID, party); err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		s.logActivity(ctx, tenant, caseID, "INVOICE_PAID", fmt.Sprintf("Invoice %s marked PAID — %s applied from money received on another case (%s)", invID, usd(feeAmt), ref))
		s.logActivity(ctx, tenant, it.CaseID, "UNAPPLIED_APPLIED", fmt.Sprintf("%s of the money received but not applied paid a fee on another case (by %s)", usd(feeAmt), actor))
		s.logAudit(ctx, tenant, caseID, "INVOICE_SETTLED", map[string]any{"by": p.Subject, "invoice_id": invID, "action": "PAY", "status": "PAID",
			"amount_cents": feeAmt, "party": party, "remittance_ref": ref, "applied_from": id})
		// What a fee being paid sets off, as when Finance marks one paid.
		_ = s.expireOverlapping(r.Context(), tenant, []string{invID})
		s.maybeMarkDecidedInvoicePaid(r, tenant, caseID)
		s.activatePaidIntake(r, tenant, caseID)
		s.fireEventRules(r, tenant, "invoice.settled", map[string]any{"case_id": caseID, "invoice_id": invID, "amount_cents": feeAmt,
			"party": party, "method": "applied", "remittance_ref": ref, "tenant": tenant})
		s.maybeAdvanceStatus(r, tenant, caseID)
		s.autoChecklist(r, tenant, caseID)
		writeJSON(w, http.StatusOK, map[string]any{"status": "applied", "amount_cents": feeAmt, "invoice_id": invID, "case_id": caseID, "left_cents": left - feeAmt})
	default:
		http.Error(w, `{"error":"action must be refund or apply"}`, http.StatusBadRequest)
	}
}
