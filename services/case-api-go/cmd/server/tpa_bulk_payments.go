package main

// tpa_bulk_payments.go — one payment for a bulk batch.
//
// A single filing gets its own fee checkout and payment email. A bulk-filed
// dispute does not (startAhcaCaseFull with batchPay): its INITIAL_FEE invoice
// is raised and the case waits in AWAITING_PAYMENT. The TPA then pays for
// every filed, unpaid dispute in the batch with ONE Stripe checkout ("pay as
// they file": disputes filed later in the batch are paid with a later
// checkout). A new checkout for the batch first expires the batch's previous
// unpaid one, so no invoice can be paid twice. When Stripe reports the
// checkout paid, every invoice in it is settled, posted to the ledger and its
// case opened -- the same steps a single payment takes.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

type unpaidFee struct {
	invoiceID, caseID, caseNumber string
	amount                        int64
}

// batchUnpaidFees: the filed disputes of a batch whose initial fee is still open.
func (s *server) batchUnpaidFees(ctx context.Context, tenant, batchID string) ([]unpaidFee, error) {
	rows, err := s.db.Query(ctx, `
		SELECT f.id::text, i.case_id, coalesce(i.case_number,''), f.amount_cents
		FROM public.tpa_dispute_batch_items i
		JOIN public.invoices f ON f.tenant=i.tenant AND f.case_id=i.case_id AND f.kind='INITIAL_FEE' AND f.status='OPEN'
		WHERE i.tenant=$1 AND i.batch_id::text=$2 AND i.status='FILED'
		ORDER BY i.row_no`, tenant, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []unpaidFee
	for rows.Next() {
		var f unpaidFee
		if rows.Scan(&f.invoiceID, &f.caseID, &f.caseNumber, &f.amount) == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

// tpaBulkCheckout: POST /tpa/disputes/bulk/{batchId}/checkout -- one Stripe
// checkout for the batch's filed, unpaid disputes.
func (s *server) tpaBulkCheckout(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	batchID := chi.URLParam(r, "batchId")
	var batchRef string
	if err := s.db.QueryRow(r.Context(), `SELECT batch_ref FROM public.tpa_dispute_batches WHERE tenant=$1 AND tpa_id=$2 AND id::text=$3`,
		tenant, org.ID, batchID).Scan(&batchRef); err != nil {
		http.Error(w, `{"error":"batch not found"}`, http.StatusNotFound)
		return
	}
	// Retire the batch's previous unpaid checkout first. If Stripe will not
	// expire it (already being paid), stop: its invoices may settle any moment.
	open, _ := s.queryRows(r, `SELECT id::text AS id, session_id FROM public.payment_groups
		WHERE tenant=$1 AND batch_id::text=$2 AND status='OPEN'`, tenant, batchID)
	for _, g := range open {
		sid := fmt.Sprint(g["session_id"])
		if _, err := s.stripePost("/v1/checkout/sessions/"+url.PathEscape(sid)+"/expire", url.Values{}); err != nil {
			http.Error(w, `{"error":"an earlier payment for this batch is being completed — check again in a minute"}`, http.StatusConflict)
			return
		}
		_, _ = s.db.Exec(r.Context(), `UPDATE public.payment_groups SET status='EXPIRED' WHERE id::text=$1 AND status='OPEN'`, g["id"])
	}
	fees, err := s.batchUnpaidFees(r.Context(), tenant, batchID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if len(fees) == 0 {
		http.Error(w, `{"error":"nothing to pay: every filed dispute in this batch is paid"}`, http.StatusConflict)
		return
	}
	var total int64
	byAmount := map[int64]int{}
	ids := make([]string, 0, len(fees))
	for _, f := range fees {
		total += f.amount
		byAmount[f.amount]++
		ids = append(ids, f.invoiceID)
	}
	var groupID string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.payment_groups (tenant, batch_id, invoice_ids, amount_cents, created_by)
		VALUES ($1,$2::uuid,$3::uuid[],$4,$5) RETURNING id::text`,
		tenant, batchID, ids, total, p.Subject).Scan(&groupID); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	back := strings.TrimRight(s.cfg.PortalBaseURL, "/") + "/app.html#/tpa"
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("success_url", back+"?paid=batch")
	form.Set("cancel_url", back)
	form.Set("customer_email", org.ContactEmail)
	n := 0
	for amount, qty := range byAmount {
		form.Set(fmt.Sprintf("line_items[%d][quantity]", n), strconv.Itoa(qty))
		form.Set(fmt.Sprintf("line_items[%d][price_data][currency]", n), "usd")
		form.Set(fmt.Sprintf("line_items[%d][price_data][unit_amount]", n), strconv.FormatInt(amount, 10))
		form.Set(fmt.Sprintf("line_items[%d][price_data][product_data][name]", n),
			fmt.Sprintf("IDRE initial review fee — batch %s", batchRef))
		n++
	}
	form.Set("metadata[tenant]", tenant)
	form.Set("metadata[payment_group_id]", groupID)
	form.Set("metadata[payer_org]", org.Name)
	form.Set("payment_intent_data[metadata][payment_group_id]", groupID)
	sess, err := s.stripePost("/v1/checkout/sessions", form)
	if err != nil {
		_, _ = s.db.Exec(r.Context(), `DELETE FROM public.payment_groups WHERE id::text=$1`, groupID)
		http.Error(w, `{"error":"the payment page could not be opened: `+jsonSafe(err.Error())+`"}`, http.StatusBadGateway)
		return
	}
	sessID, _ := sess["id"].(string)
	checkoutURL, _ := sess["url"].(string)
	_, _ = s.db.Exec(r.Context(), `UPDATE public.payment_groups SET session_id=$2, checkout_url=$3 WHERE id::text=$1`, groupID, sessID, checkoutURL)
	for _, f := range fees {
		// One payments row per invoice, keyed by session#invoice (session_id is unique).
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.payments (tenant, case_id, invoice_id, session_id, amount_cents, raw)
			VALUES ($1,$2,$3,$4,$5,'{}'::jsonb) ON CONFLICT (session_id) DO NOTHING`,
			tenant, f.caseID, f.invoiceID, sessID+"#"+f.invoiceID, f.amount)
		s.finEvent(r, tenant, f.caseID, f.invoiceID, "PAYMENT_INITIATED", "NONE", f.amount, "PROVIDER", sessID, "tpa-batch-checkout")
	}
	s.logAudit(r.Context(), tenant, "", "TPA_BATCH_CHECKOUT", map[string]any{
		"tpa_id": org.ID, "batch_id": batchID, "payment_group_id": groupID, "disputes": len(fees), "amount_cents": total})
	writeJSON(w, http.StatusCreated, map[string]any{
		"checkout_url": checkoutURL, "disputes": len(fees), "amount_cents": total, "payment_group_id": groupID})
}

// jsonSafe keeps an upstream error message from breaking a hand-built JSON body.
func jsonSafe(s string) string {
	return strings.NewReplacer(`"`, `'`, `\`, `/`, "\n", " ").Replace(s)
}

// settlePaymentGroup is the checkout.session.completed branch for a batch
// checkout: every invoice in it is settled, posted, and its case opened.
// Idempotent: only an OPEN group is settled, once.
func (s *server) settlePaymentGroup(r *http.Request, tenant, groupID, sessionID, paymentIntent, payerEmail string) {
	var invoiceIDs []string
	if err := s.db.QueryRow(r.Context(), `
		UPDATE public.payment_groups SET status='PAID', paid_at=now(), payment_intent=$3
		WHERE tenant=$1 AND id::text=$2 AND status IN ('OPEN','EXPIRED')
		RETURNING invoice_ids::text[]`, tenant, groupID, paymentIntent).Scan(&invoiceIDs); err != nil {
		return // unknown or already settled
	}
	for _, invID := range invoiceIDs {
		var caseID, party string
		var amount int64
		if err := s.db.QueryRow(r.Context(), `
			UPDATE public.invoices SET status='PAID', remittance_ref=$3, paid_at=now()::date
			WHERE tenant=$1 AND id::text=$2 AND status='OPEN' RETURNING case_id, amount_cents, party`,
			tenant, invID, paymentIntent).Scan(&caseID, &amount, &party); err != nil {
			continue // settled another way meanwhile
		}
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.payments SET status='PAID', payment_intent=$3, payer_email=$4, updated_at=now()
			WHERE tenant=$1 AND session_id=$2`, tenant, sessionID+"#"+invID, paymentIntent, payerEmail)
		s.finEvent(r, tenant, caseID, invID, "PAYMENT_PAID", "IN", amount, party, paymentIntent, "stripe-webhook")
		s.logAudit(r.Context(), tenant, caseID, "INVOICE_SETTLED", map[string]any{
			"by": "stripe-webhook", "invoice_id": invID, "action": "PAY", "status": "PAID", "amount_cents": amount,
			"party": party, "remittance_ref": paymentIntent, "payment_group_id": groupID})
		// One payment intent covers many invoices: each gets its own ledger leg.
		s.postPaymentLedger(tenant, caseID, paymentIntent+":"+invID, party, uint64(amount), false)
		s.activatePaidIntake(r, tenant, caseID)
		s.logActivity(r.Context(), tenant, caseID, "PAYMENT_RECEIVED",
			fmt.Sprintf("Initial fee $%d.%02d paid with the bulk batch payment (%s)", amount/100, amount%100, paymentIntent))
	}
	slog.Info("bulk batch payment settled", "group", groupID, "invoices", len(invoiceIDs))
}

// emailBatchFiled tells the TPA, once per batch per filing round, which of
// its disputes were filed and where to pay for them -- instead of one
// payment email per dispute.
func (s *server) emailBatchFiled(ctx context.Context, tenant, batchID string, caseNumbers []string) {
	var ref, email, name string
	if err := s.db.QueryRow(ctx, `
		SELECT b.batch_ref, coalesce(t.contact_email,''), t.name FROM public.tpa_dispute_batches b
		JOIN public.tpas t ON t.id=b.tpa_id WHERE b.tenant=$1 AND b.id::text=$2`, tenant, batchID).Scan(&ref, &email, &name); err != nil || email == "" {
		return
	}
	fees, _ := s.batchUnpaidFees(ctx, tenant, batchID)
	var total int64
	for _, f := range fees {
		total += f.amount
	}
	link := strings.TrimRight(s.cfg.PortalBaseURL, "/") + "/app.html#/tpa"
	body := fmt.Sprintf("%d dispute(s) from your bulk filing %s have been filed: %s.\n\n", len(caseNumbers), ref, strings.Join(caseNumbers, ", "))
	if len(fees) > 0 {
		body += fmt.Sprintf("Their initial review fees are due: %d dispute(s), $%d.%02d in total. Pay for all of them at once in the portal — TPA → Bulk filing → %s → Pay:\n%s\n\n"+
			"A dispute opens, with its case number and document link, once its fee is paid.", len(fees), total/100, total%100, ref, link)
	} else {
		body += "Their fees are invoiced under your organization's invoicing arrangement.\n" + link
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	subject := fmt.Sprintf("Bulk filing %s — %d dispute(s) filed", ref, len(caseNumbers))
	if err := s.sendMail([]string{email}, nil, subject, body); err != nil {
		s.logCorrespondence(r, tenant, "", "OUT", "bulk_filed", subject, body, []string{email}, nil, "system:tpa-bulk", err.Error())
		return
	}
	s.logCorrespondence(r, tenant, "", "OUT", "bulk_filed", subject, body, []string{email}, nil, "system:tpa-bulk")
}
