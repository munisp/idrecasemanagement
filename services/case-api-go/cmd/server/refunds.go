// refunds.go — refunding a paid fee, once, for the right amount, and only calling it refunded when it is.
//
// A refund is a row in public.refunds (scripts/refunds.sql). Refunding an invoice:
//
//  1. claims it: PAID -> REFUND_PENDING in one UPDATE, so a second click is refused, never a second refund;
//  2. finds how it was paid (its PAID public.payments row):
//     - a card payment is refunded through Stripe for THIS invoice's amount (a group checkout's one payment covers
//     several invoices -- refunding the whole payment refunded all of them), with an idempotency key and the
//     case/invoice in the metadata; the invoice stays REFUND_PENDING until Stripe says the refund succeeded;
//     - a check, or a fee marked paid by hand, is refunded outside the platform: it is done at once, with the
//     check / ACH reference Finance gives;
//  3. completes it exactly once (completeRefund): the invoice REFUNDED, the payment REFUNDED, one REFUND_ISSUED
//     event, one ledger reversal on the payment's own ledger leg and escrow account, the case timeline, the audit.
//
// Stripe's answer arrives three ways -- the API response, the refund.* / charge.refunded webhooks, and the
// reconcile pass for refunds still pending -- and all of them go through completeRefund / failRefund, which act
// once. A refund Stripe fails puts the invoice back to PAID and tells Finance. A duplicate payment's automatic
// refund (fee_payments.go) is kind DUPLICATE: its events never touch the invoice the other payment pays.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var errRefundState = errors.New("invoice is not paid, or is already being refunded")

// refundSource is how an invoice was paid, as far as refunding it goes. Pure, so it is tested.
type refundSource struct {
	Method        string // stripe | check | offline
	PaymentID     string
	PaymentIntent string
	LedgerKey     string // the key the payment's ledger leg was posted under ("" = none was posted)
	LedgerParty   string // the escrow party that leg credited
}

// sourceFor works out the refund source from the invoice's PAID payment row (provider, session id, intent) and the
// invoice's party. A card payment's ledger leg is keyed by the intent (one invoice) or intent:invoice (a group
// checkout: session "<cs>#<invoice>") and credited the invoice party's escrow; a check's by "chk:<id>" with no party.
func sourceFor(found bool, provider, sessionID, paymentIntent, paymentID, invID, party string) refundSource {
	switch {
	case !found:
		return refundSource{Method: "offline"}
	case provider == "check":
		return refundSource{Method: "check", PaymentID: paymentID, LedgerKey: sessionID}
	case paymentIntent != "":
		key := paymentIntent
		if strings.Contains(sessionID, "#") {
			key = paymentIntent + ":" + invID
		}
		return refundSource{Method: "stripe", PaymentID: paymentID, PaymentIntent: paymentIntent, LedgerKey: key, LedgerParty: party}
	}
	return refundSource{Method: "offline", PaymentID: paymentID}
}

// retryLedgerKey: the ledger key of a fee's refund after `reversed` earlier refunds came back. Pure, so it is tested.
func retryLedgerKey(key string, reversed int) string {
	if key == "" || reversed == 0 {
		return key
	}
	return fmt.Sprintf("%s:refund-retry-%d", key, reversed)
}

// stripeRefundOutcome maps a Stripe refund status onto what happens to ours: "succeeded", "failed" (failed or
// canceled) or "" (still pending / requires_action). Pure, so it is tested.
func stripeRefundOutcome(status string) string {
	switch status {
	case "succeeded":
		return "succeeded"
	case "failed", "canceled":
		return "failed"
	}
	return ""
}

// startRefund refunds a paid invoice: (resulting invoice status, error). The error text is for the person.
// outside: Finance returned the money outside Stripe (a check or ACH), e.g. after the card refund failed and the
// card can't take another; it is recorded at once with its reference, and still reverses the payment's ledger leg.
func (s *server) startRefund(r *http.Request, tenant, invID, reference, actor string, outside bool) (string, error) {
	ctx := r.Context()
	var caseID, party string
	var amount int64
	if err := s.db.QueryRow(ctx, `
		UPDATE public.invoices SET status='REFUND_PENDING' WHERE tenant=$1 AND id=$2 AND status='PAID'
		RETURNING case_id, amount_cents, party`, tenant, invID).Scan(&caseID, &amount, &party); err != nil {
		return "", errRefundState
	}
	undo := func() {
		_, _ = s.db.Exec(ctx, `UPDATE public.invoices SET status='PAID' WHERE tenant=$1 AND id=$2 AND status='REFUND_PENDING'`, tenant, invID)
	}
	var provider, sessionID, paymentIntent, paymentID string
	found := s.db.QueryRow(ctx, `
		SELECT coalesce(provider,'stripe'), coalesce(session_id,''), coalesce(payment_intent,''), id::text FROM public.payments
		WHERE tenant=$1 AND invoice_id=$2 AND status='PAID' ORDER BY updated_at DESC LIMIT 1`,
		tenant, invID).Scan(&provider, &sessionID, &paymentIntent, &paymentID) == nil
	src := sourceFor(found, provider, sessionID, paymentIntent, paymentID, invID, party)
	if outside && src.Method == "stripe" {
		src.Method, src.PaymentIntent = "offline", ""
	}
	// A refund after an earlier one was reversed (it came back from the bank) needs its own ledger leg: the
	// first refund's leg, keyed by the payment, is already in the ledger and would be taken for this one.
	var reversed int
	_ = s.db.QueryRow(ctx, `SELECT count(*) FROM public.financial_events WHERE tenant=$1 AND invoice_id::text=$2 AND kind='REFUND_REVERSED'`,
		tenant, invID).Scan(&reversed)
	src.LedgerKey = retryLedgerKey(src.LedgerKey, reversed)
	if src.Method == "stripe" && s.cfg.StripeSecret == "" {
		undo()
		return "", fmt.Errorf("this fee was paid by card, and card payments are not configured on this server")
	}
	status := "PENDING"
	if src.Method != "stripe" {
		status = "SUCCEEDED" // the money is returned outside the platform; recording it is the refund
	}
	var refundID string
	if err := s.db.QueryRow(ctx, `
		INSERT INTO public.refunds (tenant, case_id, invoice_id, payment_id, kind, method, amount_cents, status,
		                            payment_intent, ledger_key, party, reference, requested_by)
		VALUES ($1,$2,$3,nullif($4,'')::uuid,'FEE',$5,$6,'PENDING',nullif($7,''),nullif($8,''),$9,nullif($10,''),$11)
		RETURNING id::text`,
		tenant, caseID, invID, src.PaymentID, src.Method, amount, src.PaymentIntent, src.LedgerKey, src.LedgerParty,
		strings.TrimSpace(reference), actor).Scan(&refundID); err != nil {
		undo()
		return "", errRefundState // the one-live-refund index: someone else is refunding it
	}
	if status == "SUCCEEDED" {
		s.completeRefund(ctx, tenant, refundID, actor)
		return "REFUNDED", nil
	}
	form := url.Values{}
	form.Set("payment_intent", src.PaymentIntent)
	form.Set("amount", strconv.FormatInt(amount, 10))
	form.Set("reason", "requested_by_customer")
	form.Set("metadata[refund_id]", refundID)
	form.Set("metadata[invoice_id]", invID)
	form.Set("metadata[case_id]", caseID)
	form.Set("metadata[tenant]", tenant)
	res, err := s.stripePostIdem("/v1/refunds", form, "idre-refund-"+refundID)
	if err != nil {
		s.failRefund(ctx, tenant, refundID, err.Error(), false)
		return "", fmt.Errorf("Stripe did not take the refund: %v", err)
	}
	sid, _ := res["id"].(string)
	st, _ := res["status"].(string)
	_, _ = s.db.Exec(ctx, `UPDATE public.refunds SET stripe_refund_id=$3, updated_at=now() WHERE tenant=$1 AND id=$2::uuid`, tenant, refundID, sid)
	s.logActivity(ctx, tenant, caseID, "REFUND_REQUESTED",
		fmt.Sprintf("Refund of %s requested from Stripe (%s) by %s — completes when Stripe confirms it", usd(amount), sid, actor))
	switch stripeRefundOutcome(st) {
	case "succeeded":
		s.completeRefund(ctx, tenant, refundID, actor)
		return "REFUNDED", nil
	case "failed":
		why, _ := res["failure_reason"].(string)
		s.failRefund(ctx, tenant, refundID, orDash(why), true)
		return "", fmt.Errorf("Stripe declined the refund (%s)", orDash(why))
	}
	return "REFUND_PENDING", nil
}

// completeRefund records a refund that has gone through. It acts once per refund, whoever calls it (the API
// response, a webhook, the reconcile pass): the PENDING -> SUCCEEDED claim decides.
func (s *server) completeRefund(ctx context.Context, tenant, refundID, actor string) bool {
	var caseID, invID, kind, method, ledgerKey, party, pi, ref string
	var paymentID *string
	var amount int64
	if err := s.db.QueryRow(ctx, `
		UPDATE public.refunds SET status='SUCCEEDED', updated_at=now() WHERE tenant=$1 AND id=$2::uuid AND status='PENDING'
		RETURNING case_id, invoice_id::text, kind, method, amount_cents, coalesce(ledger_key,''), coalesce(party,''),
		          coalesce(payment_intent,''), coalesce(reference, stripe_refund_id, ''), payment_id::text`,
		tenant, refundID).Scan(&caseID, &invID, &kind, &method, &amount, &ledgerKey, &party, &pi, &ref, &paymentID); err != nil {
		return false
	}
	if kind == "DUPLICATE" {
		return true // the duplicate payment's own row was settled by refundDuplicate; the invoice stays paid
	}
	_, _ = s.db.Exec(ctx, `UPDATE public.invoices SET status='REFUNDED' WHERE tenant=$1 AND id=$2::uuid AND status IN ('REFUND_PENDING','PAID')`, tenant, invID)
	if paymentID != nil {
		_, _ = s.db.Exec(ctx, `UPDATE public.payments SET status='REFUNDED', updated_at=now() WHERE tenant=$1 AND id=$2::uuid`, tenant, *paymentID)
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	s.finEvent(r, tenant, caseID, invID, "REFUND_ISSUED", "OUT", amount, party, ref, actor)
	if ledgerKey != "" {
		s.postPaymentLedger(tenant, caseID, ledgerKey, party, uint64(amount), true) // escrow -> clearing, once (deterministic id)
	}
	how := map[string]string{"stripe": "through Stripe — back on the payer's card in 5–10 days", "stripe-dashboard": "in the Stripe dashboard",
		"check": "outside the platform (paid by check)", "offline": "outside the platform"}[method]
	s.logActivity(ctx, tenant, caseID, "REFUND_ISSUED", fmt.Sprintf("Initial fee %s refunded %s%s", usd(amount), how, orDash(" — ref "+ref)))
	s.logAudit(ctx, tenant, caseID, "INVOICE_SETTLED", map[string]any{
		"by": actor, "invoice_id": invID, "action": "REFUND", "status": "REFUNDED", "amount_cents": amount,
		"refund_id": refundID, "method": method, "remittance_ref": ref, "payment_intent": pi,
	})
	s.maybeAdvanceStatus(r, tenant, caseID)
	s.autoChecklist(r, tenant, caseID)
	return true
}

// failRefund records a refund that did not go through: the invoice goes back to PAID (it is still paid), and
// Finance is told why so they can try again or refund another way. notifyFinance is false when the person who
// pressed Refund is shown the error right away.
func (s *server) failRefund(ctx context.Context, tenant, refundID, why string, notifyFinance bool) bool {
	var caseID, invID, kind string
	var paymentID *string
	var amount int64
	if err := s.db.QueryRow(ctx, `
		UPDATE public.refunds SET status='FAILED', failure=$3, updated_at=now() WHERE tenant=$1 AND id=$2::uuid AND status='PENDING'
		RETURNING case_id, invoice_id::text, kind, amount_cents, payment_id::text`, tenant, refundID, why).Scan(&caseID, &invID, &kind, &amount, &paymentID); err != nil {
		return false
	}
	if kind == "DUPLICATE" {
		if paymentID != nil {
			_, _ = s.db.Exec(ctx, `UPDATE public.payments SET status='REFUND_DUE', last_error=$3, updated_at=now() WHERE tenant=$1 AND id=$2::uuid`, tenant, *paymentID, why)
		}
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
		s.notify(r, tenant, "role:FINANCE", "PAYMENT_NEEDS_ATTENTION",
			fmt.Sprintf("A fee of %s was paid twice and the automatic refund failed (%s) — refund it by hand", usd(amount), why), "#/cases/"+caseID)
		return true
	}
	_, _ = s.db.Exec(ctx, `UPDATE public.invoices SET status='PAID' WHERE tenant=$1 AND id=$2::uuid AND status='REFUND_PENDING'`, tenant, invID)
	s.logActivity(ctx, tenant, caseID, "REFUND_FAILED", fmt.Sprintf("Refund of %s did not go through (%s) — the fee is still paid", usd(amount), why))
	if notifyFinance {
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
		s.notify(r, tenant, "role:FINANCE", "REFUND_FAILED",
			fmt.Sprintf("The refund of %s did not go through (%s). The fee is still paid — refund it again from Financials → Refunds due.", usd(amount), why), "#/finance")
	}
	return true
}

// reverseRefund undoes a refund Stripe reported as succeeded and failed later (the payer's bank sent it back: a
// closed card, say). The money is with us again, so everything completeRefund did is reversed by new entries --
// the history is kept: the invoice and payment are PAID again, a REFUND_REVERSED event offsets the REFUND_ISSUED
// one, and a ledger leg moves the amount clearing -> escrow again under its own key.
func (s *server) reverseRefund(ctx context.Context, tenant, refundID, why string) {
	var caseID, invID, kind, ledgerKey, party string
	var paymentID *string
	var amount int64
	if err := s.db.QueryRow(ctx, `
		UPDATE public.refunds SET status='FAILED', failure=$3, updated_at=now() WHERE tenant=$1 AND id=$2::uuid AND status='SUCCEEDED'
		RETURNING case_id, invoice_id::text, kind, amount_cents, coalesce(ledger_key,''), coalesce(party,''), payment_id::text`,
		tenant, refundID, why).Scan(&caseID, &invID, &kind, &amount, &ledgerKey, &party, &paymentID); err != nil {
		return
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	if kind == "DUPLICATE" {
		if paymentID != nil {
			_, _ = s.db.Exec(ctx, `UPDATE public.payments SET status='REFUND_DUE', last_error=$3, updated_at=now() WHERE tenant=$1 AND id=$2::uuid`, tenant, *paymentID, why)
		}
		s.notify(r, tenant, "role:FINANCE", "PAYMENT_NEEDS_ATTENTION",
			fmt.Sprintf("The refund of a fee paid twice (%s) came back from the payer's bank (%s) — refund it another way", usd(amount), why), "#/cases/"+caseID)
		return
	}
	_, _ = s.db.Exec(ctx, `UPDATE public.invoices SET status='PAID' WHERE tenant=$1 AND id=$2::uuid AND status='REFUNDED'`, tenant, invID)
	if paymentID != nil {
		_, _ = s.db.Exec(ctx, `UPDATE public.payments SET status='PAID', updated_at=now() WHERE tenant=$1 AND id=$2::uuid AND status='REFUNDED'`, tenant, *paymentID)
	}
	s.finEvent(r, tenant, caseID, invID, "REFUND_REVERSED", "IN", amount, party, refundID, "stripe")
	if ledgerKey != "" {
		s.postPaymentLedger(tenant, caseID, ledgerKey+":refund-failed:"+refundID, party, uint64(amount), false)
	}
	s.logActivity(ctx, tenant, caseID, "REFUND_FAILED",
		fmt.Sprintf("The refund of %s came back from the payer's bank (%s) — the fee is paid again", usd(amount), why))
	s.logAudit(ctx, tenant, caseID, "REFUND_REVERSED", map[string]any{"by": "stripe", "invoice_id": invID, "refund_id": refundID,
		"amount_cents": amount, "reason": why})
	s.notify(r, tenant, "role:FINANCE", "REFUND_FAILED",
		fmt.Sprintf("A refund of %s came back from the payer's bank (%s). The fee is paid again — refund it another way from Financials → Refunds due.", usd(amount), why), "#/finance")
}

// stripeRefundEvent handles refund.created / refund.updated / refund.failed: Stripe's word on one refund.
func (s *server) stripeRefundEvent(ctx context.Context, obj json.RawMessage) {
	var rf struct {
		ID            string            `json:"id"`
		Status        string            `json:"status"`
		FailureReason string            `json:"failure_reason"`
		Metadata      map[string]string `json:"metadata"`
	}
	if json.Unmarshal(obj, &rf) != nil || rf.ID == "" {
		return
	}
	var tenant, refundID string
	if s.db.QueryRow(ctx, `SELECT tenant, id::text FROM public.refunds WHERE stripe_refund_id=$1
		UNION ALL SELECT tenant, id::text FROM public.refunds WHERE id::text=$2 AND stripe_refund_id IS NULL LIMIT 1`,
		rf.ID, rf.Metadata["refund_id"]).Scan(&tenant, &refundID) != nil {
		return // not one of ours: a refund made in the dashboard is handled from charge.refunded
	}
	_, _ = s.db.Exec(ctx, `UPDATE public.refunds SET stripe_refund_id=$3 WHERE tenant=$1 AND id=$2::uuid AND stripe_refund_id IS NULL`, tenant, refundID, rf.ID)
	switch stripeRefundOutcome(rf.Status) {
	case "succeeded":
		s.completeRefund(ctx, tenant, refundID, "stripe")
	case "failed":
		if !s.failRefund(ctx, tenant, refundID, orDash(rf.FailureReason), true) {
			s.reverseRefund(ctx, tenant, refundID, orDash(rf.FailureReason)) // it had succeeded, then failed
		}
	}
}

// chargeRefundedDecision: what a charge.refunded event means when no refund of ours explains it -- someone refunded
// in the Stripe dashboard. Only a full refund of a payment that pays one invoice is recorded as that invoice's
// refund; a partial one, or one of a group payment, is left for Finance (it is not ours to guess which fee it was).
// Pure, so it is tested.
func chargeRefundedDecision(amountRefunded, paymentAmount int64, groupPayment bool) string {
	switch {
	case groupPayment:
		return "ask"
	case amountRefunded >= paymentAmount && paymentAmount > 0:
		return "record"
	}
	return "ask"
}

// chargeRefundedEvent handles charge.refunded: Stripe's summary of a payment's refunds. Our own refunds (fee and
// duplicate) are completed by what they are; anything else is a refund made in the dashboard.
func (s *server) chargeRefundedEvent(ctx context.Context, obj json.RawMessage) {
	var ch struct {
		PaymentIntent  string `json:"payment_intent"`
		AmountRefunded int64  `json:"amount_refunded"`
		Refunds        struct {
			Data []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"data"`
		} `json:"refunds"`
	}
	if json.Unmarshal(obj, &ch) != nil || ch.PaymentIntent == "" {
		return
	}
	// Our refunds on this payment that Stripe lists as done, or pending ones when the list is not included.
	rows, err := s.db.Query(ctx, `SELECT tenant, id::text, coalesce(stripe_refund_id,'') FROM public.refunds
		WHERE payment_intent=$1 AND status='PENDING'`, ch.PaymentIntent)
	if err == nil {
		type mine struct{ tenant, id, sid string }
		var list []mine
		for rows.Next() {
			var m mine
			if rows.Scan(&m.tenant, &m.id, &m.sid) == nil {
				list = append(list, m)
			}
		}
		rows.Close()
		done := map[string]bool{}
		for _, x := range ch.Refunds.Data {
			if x.Status == "succeeded" {
				done[x.ID] = true
			}
		}
		for _, m := range list {
			if len(ch.Refunds.Data) == 0 || done[m.sid] {
				s.completeRefund(ctx, m.tenant, m.id, "stripe")
			}
		}
	}
	// Is all of what Stripe refunded explained by refunds we know of (fee or duplicate, pending or done)?
	var known int64
	_ = s.db.QueryRow(ctx, `SELECT coalesce(sum(amount_cents),0) FROM public.refunds WHERE payment_intent=$1 AND status IN ('PENDING','SUCCEEDED')`,
		ch.PaymentIntent).Scan(&known)
	if ch.AmountRefunded <= known {
		return
	}
	// A refund made in the dashboard. The payment it belongs to: the one still PAID with this intent.
	var tenant, caseID, invID, paymentID, sessionID, party string
	var payAmount int64
	if s.db.QueryRow(ctx, `
		SELECT p.tenant, p.case_id, p.invoice_id::text, p.id::text, coalesce(p.session_id,''), p.amount_cents, coalesce(i.party,'')
		FROM public.payments p JOIN public.invoices i ON i.tenant=p.tenant AND i.id=p.invoice_id
		WHERE p.payment_intent=$1 AND p.status='PAID' ORDER BY p.updated_at DESC LIMIT 1`, ch.PaymentIntent).
		Scan(&tenant, &caseID, &invID, &paymentID, &sessionID, &payAmount, &party) != nil {
		return
	}
	var groupRows int
	_ = s.db.QueryRow(ctx, `SELECT count(*) FROM public.payments WHERE payment_intent=$1`, ch.PaymentIntent).Scan(&groupRows)
	group := strings.Contains(sessionID, "#") || groupRows > 1
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	switch chargeRefundedDecision(ch.AmountRefunded-known, payAmount, group) {
	case "record":
		src := sourceFor(true, "stripe", sessionID, ch.PaymentIntent, paymentID, invID, party)
		var refundID string
		if s.db.QueryRow(ctx, `
			UPDATE public.invoices SET status='REFUND_PENDING' WHERE tenant=$1 AND id=$2::uuid AND status='PAID' RETURNING id::text`,
			tenant, invID).Scan(&refundID) != nil {
			return
		}
		if err := s.db.QueryRow(ctx, `
			INSERT INTO public.refunds (tenant, case_id, invoice_id, payment_id, kind, method, amount_cents, status, payment_intent,
			                            ledger_key, party, requested_by)
			VALUES ($1,$2,$3::uuid,$4::uuid,'FEE','stripe-dashboard',$5,'PENDING',$6,$7,$8,'stripe-dashboard') RETURNING id::text`,
			tenant, caseID, invID, paymentID, payAmount, ch.PaymentIntent, src.LedgerKey, party).Scan(&refundID); err != nil {
			_, _ = s.db.Exec(ctx, `UPDATE public.invoices SET status='PAID' WHERE tenant=$1 AND id=$2::uuid AND status='REFUND_PENDING'`, tenant, invID)
			return
		}
		s.completeRefund(ctx, tenant, refundID, "stripe-dashboard")
	default:
		s.notify(r, tenant, "role:FINANCE", "PAYMENT_NEEDS_ATTENTION",
			fmt.Sprintf("Stripe refunded %s of payment %s outside the platform (partly, or a payment covering several fees) — record which fee it was", usd(ch.AmountRefunded-known), ch.PaymentIntent),
			"#/cases/"+caseID)
		s.logActivity(ctx, tenant, caseID, "REFUND_OUTSIDE",
			fmt.Sprintf("Stripe reports %s refunded on payment %s outside the platform — not applied to a fee automatically", usd(ch.AmountRefunded-known), ch.PaymentIntent))
	}
}

// reconcilePendingRefunds asks Stripe about card refunds still pending after a couple of minutes (a webhook that
// never came, or came first): the background tick calls it with the other reconcile passes.
func (s *server) reconcilePendingRefunds(ctx context.Context) {
	if s.cfg.StripeSecret == "" {
		return
	}
	rows, err := s.db.Query(ctx, `
		SELECT tenant, id::text, stripe_refund_id FROM public.refunds
		WHERE status='PENDING' AND method='stripe' AND stripe_refund_id IS NOT NULL AND updated_at < now() - interval '2 minutes'
		ORDER BY updated_at LIMIT 50`)
	if err != nil {
		return
	}
	type pend struct{ tenant, id, sid string }
	var list []pend
	for rows.Next() {
		var p pend
		if rows.Scan(&p.tenant, &p.id, &p.sid) == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	for _, p := range list {
		res, err := s.stripeGet("/v1/refunds/" + url.PathEscape(p.sid))
		if err != nil {
			slog.Warn("refund reconcile", "refund", p.sid, "err", err)
			continue
		}
		st, _ := res["status"].(string)
		switch stripeRefundOutcome(st) {
		case "succeeded":
			s.completeRefund(ctx, p.tenant, p.id, "stripe")
		case "failed":
			why, _ := res["failure_reason"].(string)
			s.failRefund(ctx, p.tenant, p.id, orDash(why), true)
		default:
			_, _ = s.db.Exec(ctx, `UPDATE public.refunds SET updated_at=now() WHERE tenant=$1 AND id=$2::uuid`, p.tenant, p.id)
		}
	}
}
