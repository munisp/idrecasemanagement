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
	"encoding/json"
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

// tpaUnpaidFees: every open initial fee on a dispute this TPA filed (single
// and bulk), optionally only one batch's.
func (s *server) tpaUnpaidFees(ctx context.Context, tenant, tpaID, batchID string) ([]unpaidFee, error) {
	q := `
		SELECT f.id::text, f.case_id, coalesce(f.invoice_no,''), f.amount_cents
		FROM public.case_origin o
		JOIN public.invoices f ON f.tenant=o.tenant AND f.case_id=o.case_id AND f.kind='INITIAL_FEE' AND f.status='OPEN'
		WHERE o.tenant=$1 AND o.tpa_id::text=$2`
	args := []any{tenant, tpaID}
	if batchID != "" {
		q += ` AND o.case_id IN (SELECT case_id FROM public.tpa_dispute_batch_items WHERE batch_id::text=$3 AND status='FILED')`
		args = append(args, batchID)
	}
	rows, err := s.db.Query(ctx, q+` ORDER BY f.invoice_no`, args...)
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

// tpaFees: GET /tpa/fees -- what this TPA owes right now.
func (s *server) tpaFees(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	fees, err := s.tpaUnpaidFees(r.Context(), tenant, org.ID, "")
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	var total int64
	for _, f := range fees {
		total += f.amount
	}
	writeJSON(w, http.StatusOK, map[string]any{"unpaid": len(fees), "amount_cents": total})
}

// tpaBulkCheckout: POST /tpa/disputes/bulk/{batchId}/checkout -- one
// checkout for a batch's filed, unpaid disputes.
func (s *server) tpaBulkCheckout(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
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
	s.feeCheckout(w, r, tenant, org, batchID, "batch "+batchRef)
}

// tpaFeesCheckout: POST /tpa/fees/checkout -- one checkout for every unpaid
// fee of this TPA's disputes, however they were filed.
func (s *server) tpaFeesCheckout(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	s.feeCheckout(w, r, tenant, org, "", "unpaid disputes")
}

// feeCheckout opens one Stripe checkout for the unpaid fees in scope (a
// batch, or all of the TPA's). Every other open checkout that could take the
// same money is expired first -- earlier group checkouts and each fee's own
// single-dispute link -- so no fee can be paid twice.
func (s *server) feeCheckout(w http.ResponseWriter, r *http.Request, tenant string, org *tpaOrg, batchID, label string) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	fees, err := s.tpaUnpaidFees(r.Context(), tenant, org.ID, batchID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if len(fees) == 0 {
		http.Error(w, `{"error":"nothing to pay: every filed dispute is paid"}`, http.StatusConflict)
		return
	}
	ids := make([]string, 0, len(fees))
	for _, f := range fees {
		ids = append(ids, f.invoiceID)
	}
	open, _ := s.queryRows(r, `
		SELECT DISTINCT 'group' AS kind, g.id::text AS id, g.session_id FROM public.payment_groups g
		WHERE g.tenant=$1 AND g.status='OPEN' AND g.invoice_ids && $2::uuid[]
		UNION
		SELECT DISTINCT 'single', '', pm.session_id FROM public.payments pm
		WHERE pm.tenant=$1 AND pm.status='PENDING' AND pm.invoice_id = ANY($2::uuid[])
		  AND pm.session_id LIKE 'cs_%' AND pm.session_id NOT LIKE '%#%'`, tenant, ids)
	for _, o := range open {
		sid := fmt.Sprint(o["session_id"])
		if sid == "" || sid == "<nil>" {
			continue
		}
		if _, err := s.stripePost("/v1/checkout/sessions/"+url.PathEscape(sid)+"/expire", url.Values{}); err != nil &&
			!strings.Contains(err.Error(), "expired") {
			http.Error(w, `{"error":"an earlier payment for some of these disputes is being completed — check again in a minute"}`, http.StatusConflict)
			return
		}
		if o["kind"] == "group" {
			_, _ = s.db.Exec(r.Context(), `UPDATE public.payment_groups SET status='EXPIRED' WHERE id::text=$1 AND status='OPEN'`, o["id"])
		} else {
			_, _ = s.db.Exec(r.Context(), `UPDATE public.payments SET status='EXPIRED', updated_at=now() WHERE tenant=$1 AND session_id=$2 AND status='PENDING'`, tenant, sid)
		}
	}
	var total int64
	byAmount := map[int64]int{}
	for _, f := range fees {
		total += f.amount
		byAmount[f.amount]++
	}
	var groupID string
	var batchArg any
	if batchID != "" {
		batchArg = batchID
	}
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.payment_groups (tenant, batch_id, tpa_id, invoice_ids, amount_cents, created_by)
		VALUES ($1,$2::uuid,$3::uuid,$4::uuid[],$5,$6) RETURNING id::text`,
		tenant, batchArg, org.ID, ids, total, p.Subject).Scan(&groupID); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	back := strings.TrimRight(s.cfg.PortalBaseURL, "/") + "/app.html#/tpa"
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("success_url", back+"?paid=1")
	form.Set("cancel_url", back)
	if org.ContactEmail != "" {
		form.Set("customer_email", org.ContactEmail)
	}
	n := 0
	for amount, qty := range byAmount {
		form.Set(fmt.Sprintf("line_items[%d][quantity]", n), strconv.Itoa(qty))
		form.Set(fmt.Sprintf("line_items[%d][price_data][currency]", n), "usd")
		form.Set(fmt.Sprintf("line_items[%d][price_data][unit_amount]", n), strconv.FormatInt(amount, 10))
		form.Set(fmt.Sprintf("line_items[%d][price_data][product_data][name]", n), "IDRE initial review fee — "+label)
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
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.payments (tenant, case_id, invoice_id, session_id, amount_cents, raw)
			VALUES ($1,$2,$3,$4,$5,'{}'::jsonb) ON CONFLICT (session_id) DO NOTHING`,
			tenant, f.caseID, f.invoiceID, sessID+"#"+f.invoiceID, f.amount)
		s.finEvent(r, tenant, f.caseID, f.invoiceID, "PAYMENT_INITIATED", "NONE", f.amount, "PROVIDER", sessID, "tpa-fee-checkout")
	}
	s.logAudit(r.Context(), tenant, "", "TPA_FEE_CHECKOUT", map[string]any{
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

// stripeGet reads a Stripe object (same auth and error handling as stripePost).
func (s *server) stripeGet(path string) (map[string]any, error) {
	if s.cfg.StripeSecret == "" {
		return nil, fmt.Errorf("card payments not configured (STRIPE_SECRET_KEY unset)")
	}
	req, err := http.NewRequest("GET", "https://api.stripe.com"+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(s.cfg.StripeSecret, "")
	resp, err := stripeHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("stripe: bad response")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("stripe: HTTP %d", resp.StatusCode)
	}
	return out, nil
}

// reconcileOpenFeeCheckouts asks Stripe about every open fee checkout from
// the last two days and settles the ones that were paid. The webhook is the
// fast path; this makes a paid checkout open its disputes even when the
// webhook never arrives (on the dev cluster Stripe's endpoint points at
// another deployment, and webhooks can always be missed).
func (s *server) reconcileOpenFeeCheckouts(ctx context.Context) {
	if s.cfg.StripeSecret == "" {
		return
	}
	rows, err := s.db.Query(ctx, `
		SELECT tenant, id::text, session_id FROM public.payment_groups
		WHERE status='OPEN' AND session_id IS NOT NULL AND created_at > now() - interval '2 days'
		ORDER BY created_at LIMIT 50`)
	if err != nil {
		return
	}
	type open struct{ tenant, id, session string }
	var list []open
	for rows.Next() {
		var o open
		if rows.Scan(&o.tenant, &o.id, &o.session) == nil {
			list = append(list, o)
		}
	}
	rows.Close()
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	for _, o := range list {
		sess, err := s.stripeGet("/v1/checkout/sessions/" + url.PathEscape(o.session))
		if err != nil {
			continue
		}
		if sess["payment_status"] != "paid" {
			if sess["status"] == "expired" {
				_, _ = s.db.Exec(ctx, `UPDATE public.payment_groups SET status='EXPIRED' WHERE id::text=$1 AND status='OPEN'`, o.id)
			}
			continue
		}
		pi, _ := sess["payment_intent"].(string)
		email := ""
		if cd, ok := sess["customer_details"].(map[string]any); ok {
			email, _ = cd["email"].(string)
		}
		s.settlePaymentGroup(r, o.tenant, o.id, o.session, pi, email)
		slog.Info("fee checkout settled from Stripe (no webhook)", "group", o.id)
	}
}
