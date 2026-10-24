// Author: Patrick Munis at newwave
package main

// fee_payments.go — outside filers pay their disputes' initial fees by card.
//
// A TPA (for the providers it files for) or a health plan pays with one
// Stripe checkout for a scope it picks: one dispute ("Pay now"), one provider
// ("Pay all" on the provider), one bulk upload, or everything unpaid. Every
// scope goes through feeCheckout:
//
//   - the server finds the fees (open INITIAL_FEE invoices of the payer's own
//     open disputes) and their total; the browser sends no amounts;
//   - any other open checkout that could take the same money is expired
//     first, so no fee is paid twice; a repeated click returns the checkout
//     already open for exactly the same fees instead of opening another;
//   - the checkout is recorded in payment_groups (what it covers, its state).
//
// A checkout is settled by whichever comes first: Stripe's webhook, the
// payer's return to the portal (confirm), or the 20-second reconcile pass
// (reconcileOpenFeeCheckouts). Settlement checks the amount Stripe took
// against the group, settles only the invoices in the group, and refunds a
// fee that turns out to be paid already (two checkouts completed at once).
//
// A dispute's fee is, in words: Paid, Unpaid, Pending (Stripe took the
// payment, not confirmed yet), Failed (the last attempt was declined or
// failed), or Refunded.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// payer is the organization paying: a TPA or a health plan.
type payer struct {
	kind, id, name, email string // kind: tpa | plan
}

func (py payer) originCol() string {
	if py.kind == "plan" {
		return "health_plan_id"
	}
	return "tpa_id"
}

func (py payer) home() string {
	if py.kind == "plan" {
		return "#/healthplan"
	}
	return "#/tpa"
}

func (py payer) org() string { return py.kind + ":" + py.id }

// payerFor: the organization the caller pays for (kind "tpa" or "hp").
func (s *server) payerFor(r *http.Request, tenant, kind string) (payer, error) {
	if kind == "hp" {
		o, err := s.healthPlanForPrincipal(r, tenant)
		if err != nil {
			return payer{}, err
		}
		return payer{"plan", o.ID, o.Name, o.ContactEmail}, nil
	}
	o, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		return payer{}, err
	}
	return payer{"tpa", o.ID, o.Name, o.ContactEmail}, nil
}

// feeScope narrows what a checkout pays for.
type feeScope struct {
	label                     string // on the Stripe line item
	batchID, clientID, caseID string
	back                      string // portal page to come back to; "" = the payer's own page
}

type unpaidFee struct {
	invoiceID, caseID, caseNumber string
	amount                        int64
}

// unpaidFees: the open initial fees of the payer's own open disputes, in scope.
// A withdrawn, closed or ineligible dispute is not charged.
func (s *server) unpaidFees(ctx context.Context, tenant string, py payer, sc feeScope) ([]unpaidFee, error) {
	q := fmt.Sprintf(`
		SELECT f.id::text, f.case_id, coalesce(c.case_number,''), f.amount_cents
		FROM public.case_origin o
		JOIN tenant_%s.cases c ON c.id::text = o.case_id AND c.status NOT IN ('WITHDRAWN','CLOSED','INELIGIBLE')
		JOIN public.invoices f ON f.tenant=o.tenant AND f.case_id=o.case_id AND f.kind='INITIAL_FEE'
		                      AND f.status='OPEN' AND f.amount_cents > 0
		WHERE o.tenant=$1 AND o.%s::text=$2`, sanitizeTenant(tenant), py.originCol())
	args := []any{tenant, py.id}
	if sc.batchID != "" {
		args = append(args, sc.batchID)
		q += fmt.Sprintf(` AND o.case_id IN (SELECT case_id FROM public.tpa_dispute_batch_items WHERE batch_id::text=$%d AND status='FILED')`, len(args))
	}
	if sc.clientID != "" {
		args = append(args, sc.clientID)
		q += fmt.Sprintf(` AND (o.client_id::text=$%d OR o.hp_party_id::text=$%d)`, len(args), len(args))
	}
	if sc.caseID != "" {
		args = append(args, sc.caseID)
		q += fmt.Sprintf(` AND o.case_id=$%d`, len(args))
	}
	rows, err := s.db.Query(ctx, q+` ORDER BY c.case_number`, args...)
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

// orgFees: GET /tpa/fees, /hp/fees -- what the organization owes right now.
func (s *server) orgFees(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
			return
		}
		fees, err := s.unpaidFees(r.Context(), tenant, py, feeScope{})
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		var total int64
		ids := make([]string, 0, len(fees))
		for _, f := range fees {
			total += f.amount
			ids = append(ids, f.invoiceID)
		}
		_, cardFee := s.cardFees(r.Context(), tenant, ids)
		writeJSON(w, http.StatusOK, map[string]any{"unpaid": len(fees), "amount_cents": total, "card_fee_cents": cardFee})
	}
}

// tpaFeesCheckout: POST /tpa/fees/checkout -- every unpaid fee of the TPA's disputes.
func (s *server) tpaFeesCheckout(w http.ResponseWriter, r *http.Request) {
	s.scopedCheckout(w, r, "tpa", func(py payer) (feeScope, int) { return feeScope{label: "unpaid disputes"}, 0 })
}

// hpFeesCheckout: POST /hp/fees/checkout -- every unpaid fee of the plan's own filings.
func (s *server) hpFeesCheckout(w http.ResponseWriter, r *http.Request) {
	s.scopedCheckout(w, r, "hp", func(py payer) (feeScope, int) { return feeScope{label: "unpaid disputes"}, 0 })
}

// tpaBulkCheckout: POST /tpa/disputes/bulk/{batchId}/checkout -- a bulk upload's filed, unpaid disputes.
func (s *server) tpaBulkCheckout(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	s.scopedCheckout(w, r, "tpa", func(py payer) (feeScope, int) {
		var ref string
		if s.db.QueryRow(r.Context(), `SELECT batch_ref FROM public.tpa_dispute_batches WHERE tenant=$1 AND tpa_id::text=$2 AND id::text=$3`,
			tenant, py.id, chi.URLParam(r, "batchId")).Scan(&ref) != nil {
			return feeScope{}, http.StatusNotFound
		}
		return feeScope{label: "bulk upload " + ref, batchID: chi.URLParam(r, "batchId")}, 0
	})
}

// tpaClientCheckout: POST /tpa/clients/{clientId}/fees/checkout -- one provider's unpaid disputes.
func (s *server) tpaClientCheckout(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	s.scopedCheckout(w, r, "tpa", func(py payer) (feeScope, int) {
		var name string
		if s.db.QueryRow(r.Context(), `SELECT party_name FROM public.tpa_clients WHERE tenant=$1 AND tpa_id::text=$2 AND id::text=$3`,
			tenant, py.id, chi.URLParam(r, "clientId")).Scan(&name) != nil {
			return feeScope{}, http.StatusNotFound
		}
		return feeScope{label: name, clientID: chi.URLParam(r, "clientId")}, 0
	})
}

// disputePay: POST /tpa/disputes/{caseId}/pay, /hp/disputes/{caseId}/pay -- one dispute.
func (s *server) disputePay(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		s.scopedCheckout(w, r, kind, func(py payer) (feeScope, int) {
			caseID := chi.URLParam(r, "caseId")
			if !s.payerOwnsCase(r.Context(), tenant, py, caseID) {
				return feeScope{}, http.StatusNotFound
			}
			back := ""
			if r.URL.Query().Get("from") == "case" {
				back = "#/cases/" + caseID
			}
			return feeScope{label: "dispute", caseID: caseID, back: back}, 0
		})
	}
}

func (s *server) payerOwnsCase(ctx context.Context, tenant string, py payer, caseID string) bool {
	var ok bool
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM public.case_origin WHERE tenant=$1 AND case_id=$2 AND %s::text=$3)`,
		py.originCol()), tenant, caseID, py.id).Scan(&ok)
	return ok
}

func (s *server) scopedCheckout(w http.ResponseWriter, r *http.Request, kind string, scope func(payer) (feeScope, int)) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	py, err := s.payerFor(r, tenant, kind)
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
		return
	}
	sc, code := scope(py)
	if code != 0 {
		http.Error(w, `{"error":"not found"}`, code)
		return
	}
	// A TPA chooses each time: Stripe's page (the default), each client's own
	// saved card or bank account, or the TPA's (tpa_saved_payments.go).
	var in savedPayRequest
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
	if kind == "tpa" && (in.Source == "CLIENT" || in.Source == "TPA") {
		s.feeChargeSaved(w, r, tenant, py, sc, in)
		return
	}
	s.feeCheckout(w, r, tenant, py, sc, payMethodOf(in.Method))
}

// feeCheckout opens (or reuses) one Stripe checkout for the unpaid fees in scope, paid one way (card_fee.go): by
// card, the fees plus their card fees; from a bank account, the fees alone.
func (s *server) feeCheckout(w http.ResponseWriter, r *http.Request, tenant string, py payer, sc feeScope, method string) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	fees, err := s.unpaidFees(r.Context(), tenant, py, sc)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if len(fees) == 0 {
		http.Error(w, `{"error":"nothing to pay: these disputes are paid (or no fee is due)"}`, http.StatusConflict)
		return
	}
	ids := make([]string, 0, len(fees))
	var total int64
	byAmount := map[int64]int{}
	for _, f := range fees {
		ids = append(ids, f.invoiceID)
		total += f.amount
		byAmount[f.amount]++
	}
	perCard, cardFee := map[string]int64{}, int64(0)
	if method == payByCard {
		perCard, cardFee = s.cardFees(r.Context(), tenant, ids)
	}
	charge := total + cardFee
	// A second click (or a second tab) for exactly the same fees gets the
	// checkout already open, not a new one that would expire it.
	var reuseID, reuseURL string
	if s.db.QueryRow(r.Context(), `
		SELECT id::text, checkout_url FROM public.payment_groups
		WHERE tenant=$1 AND status='OPEN' AND checkout_url IS NOT NULL AND created_at > now() - interval '30 minutes'
		  AND invoice_ids @> $2::uuid[] AND invoice_ids <@ $2::uuid[] AND amount_cents=$3 AND coalesce(pay_by,'card')=$4
		ORDER BY created_at DESC LIMIT 1`, tenant, ids, charge, method).Scan(&reuseID, &reuseURL) == nil {
		writeJSON(w, http.StatusOK, map[string]any{"checkout_url": reuseURL, "disputes": len(fees), "amount_cents": charge,
			"card_fee_cents": cardFee, "payment_group_id": reuseID, "reused": true})
		return
	}
	if !s.expireOverlapping(r.Context(), tenant, ids) {
		http.Error(w, `{"error":"an earlier payment for some of these disputes is being completed — check again in a minute"}`, http.StatusConflict)
		return
	}
	var tpaArg, planArg, batchArg, clientArg, caseArg any
	if py.kind == "plan" {
		planArg = py.id
	} else {
		tpaArg = py.id
	}
	if sc.batchID != "" {
		batchArg = sc.batchID
	}
	if sc.clientID != "" {
		clientArg = sc.clientID
	}
	if sc.caseID != "" {
		caseArg = sc.caseID
	}
	var groupID string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.payment_groups (tenant, batch_id, tpa_id, plan_id, client_id, case_id, scope, invoice_ids, amount_cents, created_by,
		                                   card_fee_cents, pay_by)
		VALUES ($1,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6,$7,$8::uuid[],$9,$10,$11,$12) RETURNING id::text`,
		tenant, batchArg, tpaArg, planArg, clientArg, caseArg, sc.label, ids, charge, p.Subject, cardFee, method).Scan(&groupID); err != nil {
		slog.Error("payment group", "err", err)
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	back := sc.back
	if back == "" {
		back = py.home()
	}
	base := strings.TrimRight(s.cfg.PortalBaseURL, "/") + "/app.html" + back
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("success_url", base+"?paid=1&g="+groupID)
	form.Set("cancel_url", base+"?cancelled="+groupID)
	if py.email != "" {
		form.Set("customer_email", py.email)
	}
	n := 0
	for amount, qty := range byAmount {
		form.Set(fmt.Sprintf("line_items[%d][quantity]", n), strconv.Itoa(qty))
		form.Set(fmt.Sprintf("line_items[%d][price_data][currency]", n), "usd")
		form.Set(fmt.Sprintf("line_items[%d][price_data][unit_amount]", n), strconv.FormatInt(amount, 10))
		name := "IDRE initial review fee — " + sc.label
		if len(fees) == 1 {
			name = "IDRE initial review fee — dispute " + fees[0].caseNumber
		}
		form.Set(fmt.Sprintf("line_items[%d][price_data][product_data][name]", n), name)
		n++
	}
	if err := s.checkoutFor(form, method, cardFee, n); err != nil {
		_, _ = s.db.Exec(r.Context(), `DELETE FROM public.payment_groups WHERE id::text=$1`, groupID)
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusFailedDependency)
		return
	}
	form.Set("metadata[tenant]", tenant)
	form.Set("metadata[payment_group_id]", groupID)
	form.Set("metadata[payer_org]", py.name)
	form.Set("payment_intent_data[metadata][payment_group_id]", groupID)
	form.Set("payment_intent_data[metadata][tenant]", tenant)
	// Which methods Stripe's page offers is the payment method configuration
	// checkoutFor set (card or bank). A bank debit settles in days:
	// checkout.session.async_payment_* opens the dispute.
	sess, err := s.stripePost("/v1/checkout/sessions", form)
	if err != nil {
		_, _ = s.db.Exec(r.Context(), `DELETE FROM public.payment_groups WHERE id::text=$1`, groupID)
		http.Error(w, `{"error":"the payment page could not be opened: `+jsonSafe(err.Error())+`"}`, http.StatusFailedDependency) // not 502: Cloudflare would hide the reason
		return
	}
	sessID, _ := sess["id"].(string)
	checkoutURL, _ := sess["url"].(string)
	_, _ = s.db.Exec(r.Context(), `UPDATE public.payment_groups SET session_id=$2, checkout_url=$3, updated_at=now() WHERE id::text=$1`, groupID, sessID, checkoutURL)
	for _, f := range fees {
		_, _ = s.db.Exec(r.Context(), `
			INSERT INTO public.payments (tenant, case_id, invoice_id, session_id, amount_cents, payer_org, raw, card_fee_cents)
			VALUES ($1,$2,$3,$4,$5,$6,'{}'::jsonb,$7) ON CONFLICT (session_id) DO NOTHING`,
			tenant, f.caseID, f.invoiceID, sessID+"#"+f.invoiceID, f.amount, py.name, perCard[f.invoiceID])
		s.finEvent(r, tenant, f.caseID, f.invoiceID, "PAYMENT_INITIATED", "NONE", f.amount, "PROVIDER", sessID, "fee-checkout")
	}
	s.logAudit(r.Context(), tenant, sc.caseID, "FEE_CHECKOUT", map[string]any{
		"org": py.org(), "by": p.Subject, "scope": sc.label, "batch_id": sc.batchID, "client_id": sc.clientID,
		"payment_group_id": groupID, "disputes": len(fees), "amount_cents": charge, "card_fee_cents": cardFee, "pay_by": method})
	writeJSON(w, http.StatusCreated, map[string]any{
		"checkout_url": checkoutURL, "disputes": len(fees), "amount_cents": charge, "card_fee_cents": cardFee, "payment_group_id": groupID})
}

// expireOverlapping closes every open checkout that could take money for one
// of these invoices -- earlier group checkouts and each fee's own emailed
// link. false: one of them is being completed right now.
func (s *server) expireOverlapping(ctx context.Context, tenant string, ids []string) bool {
	rows, err := s.db.Query(ctx, `
		SELECT 'group', id::text, coalesce(session_id,'') FROM public.payment_groups
		WHERE tenant=$1 AND status IN ('OPEN','PROCESSING','REVIEW') AND invoice_ids && $2::uuid[]
		UNION
		SELECT 'single', '', session_id FROM public.payments
		WHERE tenant=$1 AND status='PENDING' AND invoice_id = ANY($2::uuid[]) AND session_id LIKE 'cs_%' AND session_id NOT LIKE '%#%'`, tenant, ids)
	if err != nil {
		return false
	}
	type open struct{ kind, id, sid string }
	var list []open
	for rows.Next() {
		var o open
		if rows.Scan(&o.kind, &o.id, &o.sid) == nil {
			list = append(list, o)
		}
	}
	rows.Close()
	for _, o := range list {
		if o.sid != "" {
			if _, err := s.stripePost("/v1/checkout/sessions/"+url.PathEscape(o.sid)+"/expire", url.Values{}); err != nil &&
				!strings.Contains(err.Error(), "expired") {
				// "complete" sessions cannot be expired: the money is taken or on its way.
				return false
			}
		}
		if o.kind == "group" {
			s.closeGroup(ctx, tenant, o.id, "EXPIRED", "replaced by a newer checkout")
		} else {
			_, _ = s.db.Exec(ctx, `UPDATE public.payments SET status='EXPIRED', updated_at=now() WHERE tenant=$1 AND session_id=$2 AND status='PENDING'`, tenant, o.sid)
		}
	}
	return true
}

// closeGroup ends an unpaid checkout (EXPIRED | CANCELLED | FAILED) and its per-fee rows.
func (s *server) closeGroup(ctx context.Context, tenant, groupID, status, why string) {
	var sid string
	if s.db.QueryRow(ctx, `
		UPDATE public.payment_groups SET status=$3, last_error=coalesce(NULLIF($4,''), last_error), updated_at=now()
		WHERE tenant=$1 AND id::text=$2 AND status IN ('OPEN','PROCESSING') RETURNING coalesce(session_id,'')`,
		tenant, groupID, status, why).Scan(&sid) != nil || sid == "" {
		return
	}
	rowStatus := "EXPIRED"
	if status == "FAILED" {
		rowStatus = "FAILED"
	}
	_, _ = s.db.Exec(ctx, `UPDATE public.payments SET status=$3, last_error=NULLIF($4,''), updated_at=now()
		WHERE tenant=$1 AND session_id LIKE $2 || '#%' AND status='PENDING'`, tenant, sid, rowStatus, why)
}

// payerGroup loads a checkout of the caller's organization.
func (s *server) payerGroup(r *http.Request, tenant string, py payer, groupID string) (sid string, ok bool) {
	err := s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT coalesce(session_id,'') FROM public.payment_groups WHERE tenant=$1 AND id::text=$2 AND %s::text=$3`,
		map[string]string{"tpa": "tpa_id", "plan": "plan_id"}[py.kind]), tenant, groupID, py.id).Scan(&sid)
	return sid, err == nil
}

// checkoutConfirm: POST /tpa/fees/checkout/{groupId}/confirm (and /hp/...) --
// the payer is back from Stripe: settle now instead of waiting for the webhook.
func (s *server) checkoutConfirm(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		gid := chi.URLParam(r, "groupId")
		sid, ok := s.payerGroup(r, tenant, py, gid)
		if !ok {
			http.Error(w, `{"error":"no such payment"}`, http.StatusNotFound)
			return
		}
		state := s.reconcileGroup(context.WithoutCancel(r.Context()), tenant, gid, sid)
		writeJSON(w, http.StatusOK, map[string]any{"status": state})
	}
}

// checkoutCancel: POST /tpa/fees/checkout/{groupId}/cancel (and /hp/...) --
// the payer left the checkout: close it so it cannot be paid later by mistake.
func (s *server) checkoutCancel(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		gid := chi.URLParam(r, "groupId")
		sid, ok := s.payerGroup(r, tenant, py, gid)
		if !ok {
			http.Error(w, `{"error":"no such payment"}`, http.StatusNotFound)
			return
		}
		// Paid after all (a second tab)? Then it is settled, not cancelled.
		if st := s.reconcileGroup(r.Context(), tenant, gid, sid); st == "PAID" || st == "PROCESSING" {
			writeJSON(w, http.StatusOK, map[string]any{"status": st})
			return
		}
		if sid != "" {
			if _, err := s.stripePost("/v1/checkout/sessions/"+url.PathEscape(sid)+"/expire", url.Values{}); err != nil &&
				!strings.Contains(err.Error(), "expired") {
				writeJSON(w, http.StatusOK, map[string]any{"status": "PROCESSING"})
				return
			}
		}
		s.closeGroup(r.Context(), tenant, gid, "CANCELLED", "cancelled at checkout")
		s.logAudit(r.Context(), tenant, "", "FEE_CHECKOUT_CANCELLED", map[string]any{"org": py.org(), "payment_group_id": gid,
			"by": r.Context().Value(ctxPrincipal{}).(principal).Subject})
		writeJSON(w, http.StatusOK, map[string]any{"status": "CANCELLED"})
	}
}

// reconcileGroup asks Stripe where a checkout stands and acts on it. It
// returns the group's state after: PAID | PROCESSING | OPEN | FAILED | EXPIRED | CANCELLED.
func (s *server) reconcileGroup(ctx context.Context, tenant, groupID, sid string) string {
	var status string
	var amount int64
	if s.db.QueryRow(ctx, `SELECT status, amount_cents FROM public.payment_groups WHERE tenant=$1 AND id::text=$2`, tenant, groupID).Scan(&status, &amount) != nil {
		return ""
	}
	if status == "PAID" || sid == "" {
		return status
	}
	sess, err := s.stripeGet("/v1/checkout/sessions/" + url.PathEscape(sid) + "?expand[]=payment_intent")
	if err != nil {
		return status
	}
	pi, piID := map[string]any{}, ""
	switch v := sess["payment_intent"].(type) {
	case map[string]any:
		pi, piID = v, fmt.Sprint(v["id"])
	case string:
		piID = v
	}
	email := ""
	if cd, ok := sess["customer_details"].(map[string]any); ok {
		email, _ = cd["email"].(string)
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	switch {
	case sess["payment_status"] == "paid":
		if got := toInt64(sess["amount_total"]); got != amount {
			s.flagPayment(ctx, tenant, groupID, fmt.Sprintf("Stripe took %s for a checkout of %s — not settled, check it", usd(got), usd(amount)))
			return status
		}
		s.settlePaymentGroup(r, tenant, groupID, sid, piID, email)
		return "PAID"
	case sess["status"] == "complete": // taken, not confirmed yet (bank debits)
		_, _ = s.db.Exec(ctx, `UPDATE public.payment_groups SET status='PROCESSING', updated_at=now() WHERE tenant=$1 AND id::text=$2 AND status='OPEN'`, tenant, groupID)
		return "PROCESSING"
	case sess["status"] == "expired":
		if status == "OPEN" {
			s.closeGroup(ctx, tenant, groupID, "EXPIRED", "")
			return "EXPIRED"
		}
		return status
	}
	// Still open: a declined card shows on the payment intent.
	if e, ok := pi["last_payment_error"].(map[string]any); ok {
		msg := fmt.Sprint(e["message"])
		_, _ = s.db.Exec(ctx, `UPDATE public.payment_groups SET last_error=$3, updated_at=now() WHERE tenant=$1 AND id::text=$2 AND coalesce(last_error,'')<>$3`, tenant, groupID, msg)
	}
	return status
}

// usd: cents as dollars, grouped the way people read them ($1,500.00).
func usd(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	d := fmt.Sprintf("%d", c/100)
	for i := len(d) - 3; i > 0; i -= 3 {
		d = d[:i] + "," + d[i:]
	}
	return fmt.Sprintf("%s$%s.%02d", sign, d, c%100)
}

// flagPayment: something about a payment needs a person (finance).
func (s *server) flagPayment(ctx context.Context, tenant, groupID, why string) {
	// REVIEW: money may have been taken, so the payer sees it pending (not
	// failed), and the reconcile pass leaves it to finance. The reason is for
	// finance; the payer is not shown it.
	_, _ = s.db.Exec(ctx, `UPDATE public.payment_groups SET status='REVIEW', last_error=$3, updated_at=now() WHERE tenant=$1 AND id::text=$2 AND status<>'PAID'`, tenant, groupID, why)
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	s.notify(r, tenant, "role:FINANCE", "PAYMENT_NEEDS_ATTENTION", why, "#/payments")
	s.logAudit(ctx, tenant, "", "PAYMENT_FLAGGED", map[string]any{"payment_group_id": groupID, "reason": why, "by": "system"})
}

// settlePaymentGroup settles a paid checkout: each invoice in it is settled,
// posted to the ledger and its dispute opened -- only those invoices. A fee
// already paid another way is refunded (it was paid twice). Idempotent: a
// group is settled once.
func (s *server) settlePaymentGroup(r *http.Request, tenant, groupID, sessionID, paymentIntent, payerEmail string) {
	var invoiceIDs []string
	var tpaID, planID string
	if err := s.db.QueryRow(r.Context(), `
		UPDATE public.payment_groups g SET status='PAID', paid_at=now(), payment_intent=$3, last_error=NULL, updated_at=now()
		WHERE g.tenant=$1 AND g.id::text=$2 AND g.status <> 'PAID'
		RETURNING g.invoice_ids::text[], coalesce(g.tpa_id::text, (SELECT b.tpa_id::text FROM public.tpa_dispute_batches b WHERE b.id=g.batch_id), ''),
		          coalesce(g.plan_id::text,'')`, tenant, groupID, paymentIntent).Scan(&invoiceIDs, &tpaID, &planID); err != nil {
		return // unknown or already settled
	}
	org := ""
	if tpaID != "" {
		org = "tpa:" + tpaID
	} else if planID != "" {
		org = "plan:" + planID
	}
	opened, refunded := 0, 0
	for _, invID := range invoiceIDs {
		var caseID, party string
		var amount int64
		if err := s.db.QueryRow(r.Context(), `
			UPDATE public.invoices SET status='PAID', remittance_ref=$3, paid_at=now()::date
			WHERE tenant=$1 AND id::text=$2 AND status='OPEN' RETURNING case_id, amount_cents, party`,
			tenant, invID, paymentIntent).Scan(&caseID, &amount, &party); err != nil {
			// Paid already (another checkout completed at the same time): this
			// share of the payment is money taken twice -- give it back.
			if s.refundDuplicate(r, tenant, invID, sessionID+"#"+invID, paymentIntent, org) {
				refunded++
			}
			continue
		}
		var cardFee int64
		_ = s.db.QueryRow(r.Context(), `
			UPDATE public.payments SET status='PAID', payment_intent=$3, payer_email=$4, last_error=NULL, updated_at=now()
			WHERE tenant=$1 AND session_id=$2 RETURNING card_fee_cents`, tenant, sessionID+"#"+invID, paymentIntent, payerEmail).Scan(&cardFee)
		s.finEvent(r, tenant, caseID, invID, "PAYMENT_PAID", "IN", amount, party, paymentIntent, "stripe")
		if cardFee > 0 {
			s.finEvent(r, tenant, caseID, invID, "CARD_FEE_PAID", "IN", cardFee, party, paymentIntent, "stripe")
		}
		s.logAudit(r.Context(), tenant, caseID, "INVOICE_SETTLED", map[string]any{
			"by": "stripe", "invoice_id": invID, "action": "PAY", "status": "PAID", "amount_cents": amount,
			"party": party, "remittance_ref": paymentIntent, "payment_group_id": groupID, "org": org})
		// One payment intent covers many invoices: each gets its own ledger leg.
		s.postPaymentLedger(tenant, caseID, paymentIntent+":"+invID, party, uint64(amount), false)
		s.activatePaidIntakeFor(r, tenant, caseID, false)
		s.maybeAdvanceStatus(r, tenant, caseID)
		opened++
		s.logActivity(r.Context(), tenant, caseID, "PAYMENT_RECEIVED", paidWords(amount, cardFee, paymentIntent))
	}
	slog.Info("fee checkout settled", "group", groupID, "invoices", len(invoiceIDs), "opened", opened, "refunded", refunded)
	kind, id, home := "tpa", tpaID, "#/tpa"
	if tpaID == "" {
		kind, id, home = "plan", planID, "#/healthplan"
	}
	if opened > 0 {
		s.notifyOrg(r.Context(), tenant, kind, id, "", "PAYMENT_RECEIVED",
			fmt.Sprintf("Payment received — %d dispute%s now open", opened, map[bool]string{true: " is", false: "s are"}[opened == 1]), home)
	}
	if refunded > 0 {
		s.notifyOrg(r.Context(), tenant, kind, id, "", "PAYMENT_REFUNDED",
			fmt.Sprintf("%d fee%s in your payment had been paid already and %s refunded to your card", refunded,
				map[bool]string{true: "", false: "s"}[refunded == 1], map[bool]string{true: "was", false: "were"}[refunded == 1]), home)
	}
}

// refundDuplicate gives back a fee paid a second time. true: refunded.
func (s *server) refundDuplicate(r *http.Request, tenant, invID, paymentRow, paymentIntent, org string) bool {
	var caseID string
	var amount int64
	if s.db.QueryRow(r.Context(), `
		UPDATE public.payments SET status='REFUNDING', payment_intent=$3, updated_at=now()
		WHERE tenant=$1 AND session_id=$2 AND status NOT IN ('PAID','REFUNDED','REFUNDING') RETURNING case_id, amount_cents + card_fee_cents`,
		tenant, paymentRow, paymentIntent).Scan(&caseID, &amount) != nil || amount <= 0 || paymentIntent == "" {
		return false
	}
	// Its own refunds row (kind DUPLICATE): Stripe's events for it then never touch the invoice the other payment
	// pays, and the idempotency key makes a retried settle refund it once.
	var refundID string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.refunds (tenant, case_id, invoice_id, payment_id, kind, method, amount_cents, status, payment_intent, requested_by)
		SELECT $1, $2, $3::uuid, id, 'DUPLICATE', 'stripe', $4, 'PENDING', $5, 'system' FROM public.payments WHERE tenant=$1 AND session_id=$6
		RETURNING id::text`, tenant, caseID, invID, amount, paymentIntent, paymentRow).Scan(&refundID); err != nil {
		slog.Error("duplicate refund row", "err", err)
	}
	form := url.Values{}
	form.Set("payment_intent", paymentIntent)
	form.Set("amount", strconv.FormatInt(amount, 10))
	form.Set("reason", "duplicate")
	form.Set("metadata[invoice_id]", invID)
	form.Set("metadata[tenant]", tenant)
	form.Set("metadata[refund_id]", refundID)
	res, err := s.stripePostIdem("/v1/refunds", form, "idre-dup-refund-"+paymentRow)
	if err != nil {
		if refundID != "" {
			_, _ = s.db.Exec(r.Context(), `UPDATE public.refunds SET status='FAILED', failure=$2, updated_at=now() WHERE id=$1::uuid`, refundID, err.Error())
		}
		_, _ = s.db.Exec(r.Context(), `UPDATE public.payments SET status='REFUND_DUE', last_error=$3, updated_at=now() WHERE tenant=$1 AND session_id=$2`, tenant, paymentRow, err.Error())
		s.notify(r, tenant, "role:FINANCE", "PAYMENT_NEEDS_ATTENTION",
			fmt.Sprintf("A fee of %s was paid twice and the automatic refund failed (%s) — refund it by hand", usd(amount), paymentIntent), "#/cases/"+caseID)
		return false
	}
	if refundID != "" {
		sid, _ := res["id"].(string)
		_, _ = s.db.Exec(r.Context(), `UPDATE public.refunds SET stripe_refund_id=nullif($2,''), updated_at=now() WHERE id=$1::uuid`, refundID, sid)
		if st, _ := res["status"].(string); stripeRefundOutcome(st) == "succeeded" {
			s.completeRefund(r.Context(), tenant, refundID, "system")
		}
	}
	_, _ = s.db.Exec(r.Context(), `UPDATE public.payments SET status='REFUNDED', updated_at=now() WHERE tenant=$1 AND session_id=$2`, tenant, paymentRow)
	s.logAudit(r.Context(), tenant, caseID, "DUPLICATE_PAYMENT_REFUNDED", map[string]any{
		"by": "system", "invoice_id": invID, "amount_cents": amount, "remittance_ref": paymentIntent, "org": org})
	s.logActivity(r.Context(), tenant, caseID, "PAYMENT_REFUNDED",
		fmt.Sprintf("Initial fee %s was paid twice; the second payment was refunded (%s)", usd(amount), paymentIntent))
	return true
}

// settleSingleSession settles a single-invoice checkout (the emailed payment
// link, or a staff-issued checkout). Idempotent, amount-checked; a fee paid
// already another way is refunded.
func (s *server) settleSingleSession(r *http.Request, tenant, sessID, invID, caseID, paymentIntent, email string, amountTotal int64, raw []byte, payerName, payerOrg string) {
	var payStatus string
	var rowAmount, cardFee int64
	if s.db.QueryRow(r.Context(), `SELECT status, amount_cents, card_fee_cents FROM public.payments WHERE tenant=$1 AND session_id=$2`, tenant, sessID).Scan(&payStatus, &rowAmount, &cardFee) != nil {
		return
	}
	if payStatus == "PAID" || payStatus == "REFUNDED" {
		return // this event was handled already
	}
	// By card, the page took the fee and its card fee.
	if amountTotal > 0 && rowAmount > 0 && amountTotal != rowAmount+cardFee {
		r2, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "/", nil)
		s.notify(r2, tenant, "role:FINANCE", "PAYMENT_NEEDS_ATTENTION",
			fmt.Sprintf("Stripe took %s for a %s fee checkout (%s) — not settled, check it", usd(amountTotal), usd(rowAmount+cardFee), sessID), "#/cases/"+caseID)
		return
	}
	if raw == nil {
		raw = []byte("{}")
	}
	var amount int64
	var party string
	if s.db.QueryRow(r.Context(), `
		UPDATE public.invoices SET status='PAID', remittance_ref=$3, paid_at=now()::date
		WHERE tenant=$1 AND id::text=$2 AND status='OPEN' RETURNING amount_cents, party`,
		tenant, invID, paymentIntent).Scan(&amount, &party) != nil {
		org := ""
		var t, p string
		if s.db.QueryRow(r.Context(), `SELECT coalesce(tpa_id::text,''), coalesce(health_plan_id::text,'') FROM public.case_origin WHERE tenant=$1 AND case_id=$2`, tenant, caseID).Scan(&t, &p) == nil {
			if t != "" {
				org = "tpa:" + t
			} else if p != "" {
				org = "plan:" + p
			}
		}
		s.refundDuplicate(r, tenant, invID, sessID, paymentIntent, org)
		return
	}
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.payments SET status='PAID', payment_intent=$3, payer_email=$4, payer_name=NULLIF($6,''), payer_org=coalesce(NULLIF($7,''), payer_org),
		       raw=$5, last_error=NULL, updated_at=now()
		WHERE session_id=$1 AND tenant=$2`, sessID, tenant, paymentIntent, email, raw, payerName, payerOrg)
	s.finEvent(r, tenant, caseID, invID, "PAYMENT_PAID", "IN", amount, party, paymentIntent, "stripe")
	if cardFee > 0 {
		s.finEvent(r, tenant, caseID, invID, "CARD_FEE_PAID", "IN", cardFee, party, paymentIntent, "stripe")
	}
	s.logAudit(r.Context(), tenant, caseID, "INVOICE_SETTLED", map[string]any{
		"by": "stripe", "invoice_id": invID, "action": "PAY", "status": "PAID",
		"amount_cents": amount, "card_fee_cents": cardFee, "party": party, "remittance_ref": paymentIntent,
	})
	s.postPaymentLedger(tenant, caseID, paymentIntent, party, uint64(amount), false) // clearing → escrow
	s.activatePaidIntake(r, tenant, caseID)
	s.logActivity(r.Context(), tenant, caseID, "PAYMENT_RECEIVED", paidWords(amount, cardFee, paymentIntent)+" — invoice settled, ledger posted")
	s.fireEventRules(r, tenant, "invoice.settled", map[string]any{
		"case_id": caseID, "invoice_id": invID, "amount_cents": amount,
		"party": party, "method": "card", "remittance_ref": paymentIntent, "tenant": tenant,
	})
	s.maybeAdvanceStatus(r, tenant, caseID)
	s.autoChecklist(r, tenant, caseID)
}

// reconcileOpenFeeCheckouts asks Stripe about every unsettled fee checkout of
// the last two days -- group checkouts and single emailed links -- and acts on
// what it says. The webhook is the fast path; this makes a paid checkout open
// its disputes when the webhook never arrives (on the dev cluster Stripe's
// endpoint points at another deployment, and webhooks can always be missed).
func (s *server) reconcileOpenFeeCheckouts(ctx context.Context) {
	if s.cfg.StripeSecret == "" {
		return
	}
	rows, err := s.db.Query(ctx, `
		SELECT tenant, id::text, session_id FROM public.payment_groups
		WHERE status IN ('OPEN','PROCESSING') AND session_id IS NOT NULL AND source='CHECKOUT' AND created_at > now() - interval '2 days'
		ORDER BY created_at LIMIT 50`)
	if err != nil {
		return
	}
	type open struct{ tenant, id, session string }
	var groups []open
	for rows.Next() {
		var o open
		if rows.Scan(&o.tenant, &o.id, &o.session) == nil {
			groups = append(groups, o)
		}
	}
	rows.Close()
	for _, o := range groups {
		s.reconcileGroup(ctx, o.tenant, o.id, o.session)
	}
	srows, err := s.db.Query(ctx, `
		SELECT tenant, session_id, invoice_id::text, case_id FROM public.payments
		WHERE status='PENDING' AND session_id LIKE 'cs_%' AND session_id NOT LIKE '%#%' AND created_at > now() - interval '2 days'
		ORDER BY created_at LIMIT 50`)
	if err != nil {
		return
	}
	type single struct{ tenant, sid, inv, caseID string }
	var singles []single
	for srows.Next() {
		var o single
		if srows.Scan(&o.tenant, &o.sid, &o.inv, &o.caseID) == nil {
			singles = append(singles, o)
		}
	}
	srows.Close()
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	for _, o := range singles {
		sess, err := s.stripeGet("/v1/checkout/sessions/" + url.PathEscape(o.sid))
		if err != nil {
			continue
		}
		switch {
		case sess["payment_status"] == "paid":
			pi, _ := sess["payment_intent"].(string)
			email := ""
			if cd, ok := sess["customer_details"].(map[string]any); ok {
				email, _ = cd["email"].(string)
			}
			raw, _ := json.Marshal(sess)
			s.settleSingleSession(r, o.tenant, o.sid, o.inv, o.caseID, pi, email, toInt64(sess["amount_total"]), raw, "", "")
		case sess["status"] == "expired":
			_, _ = s.db.Exec(ctx, `UPDATE public.payments SET status='EXPIRED', updated_at=now() WHERE tenant=$1 AND session_id=$2 AND status='PENDING'`, o.tenant, o.sid)
		}
	}
}

// feeInfo is a dispute's initial fee, in the payer's terms.
type feeInfo struct {
	State       string `json:"fee_state"` // PAID | UNPAID | PENDING | FAILED | REFUNDED | "" (no fee)
	AmountCents int64  `json:"fee_cents"`
	Error       string `json:"fee_error,omitempty"`
	Checkout    bool   `json:"fee_checkout_open,omitempty"` // a checkout is open for it now
}

// feeStates: the initial fee state of each of these disputes.
func (s *server) feeStates(ctx context.Context, tenant string, caseIDs []string) map[string]feeInfo {
	out := map[string]feeInfo{}
	if len(caseIDs) == 0 {
		return out
	}
	rows, err := s.db.Query(ctx, `
		SELECT f.case_id, f.status, f.amount_cents,
		       coalesce(g.status,''), coalesce(g.last_error,''),
		       EXISTS (SELECT 1 FROM public.payments p WHERE p.tenant=f.tenant AND p.invoice_id=f.id AND p.status='PENDING'
		                 AND p.session_id LIKE 'cs_%' AND p.created_at > now() - interval '24 hours')
		FROM public.invoices f
		LEFT JOIN LATERAL (SELECT status, last_error FROM public.payment_groups g
		                   WHERE g.tenant=f.tenant AND f.id = ANY(g.invoice_ids) ORDER BY g.created_at DESC LIMIT 1) g ON true
		WHERE f.tenant=$1 AND f.kind='INITIAL_FEE' AND f.case_id = ANY($2)`, tenant, caseIDs)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var caseID, inv, gStatus, gErr string
		var amount int64
		var openSingle bool
		if rows.Scan(&caseID, &inv, &amount, &gStatus, &gErr, &openSingle) != nil {
			continue
		}
		fi := feeInfo{AmountCents: amount}
		switch {
		case inv == "PAID":
			fi.State = "PAID"
		case inv == "REFUNDED":
			fi.State = "REFUNDED"
		case inv != "OPEN":
			fi.State = inv
		case gStatus == "PROCESSING":
			fi.State = "PENDING"
		case gStatus == "REVIEW":
			fi.State, fi.Error = "PENDING", "Payment received; our finance team is confirming it"
		case gStatus == "FAILED" || (gStatus == "OPEN" && gErr != ""):
			fi.State, fi.Error = "FAILED", gErr
		default:
			fi.State = "UNPAID"
			fi.Checkout = gStatus == "OPEN" || openSingle
		}
		out[caseID] = fi
	}
	return out
}

// disputeFee: GET /tpa/disputes/{caseId}/fee, /hp/disputes/{caseId}/fee -- one dispute's fee.
func (s *server) disputeFee(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		caseID := chi.URLParam(r, "caseId")
		if err != nil || !s.payerOwnsCase(r.Context(), tenant, py, caseID) {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, s.feeStates(r.Context(), tenant, []string{caseID})[caseID])
	}
}

// attachFeeStates adds each row's fee state (fee_state, fee_cents,
// fee_error, fee_checkout_open) by its "case_id".
func (s *server) attachFeeStates(ctx context.Context, tenant string, rows []map[string]any) {
	ids := []string{}
	for _, row := range rows {
		if id := fmt.Sprint(row["case_id"]); id != "" && id != "<nil>" {
			ids = append(ids, id)
		}
	}
	states := s.feeStates(ctx, tenant, ids)
	for _, row := range rows {
		fi, ok := states[fmt.Sprint(row["case_id"])]
		if !ok {
			continue
		}
		row["fee_state"], row["fee_cents"], row["fee_error"], row["fee_checkout_open"] = fi.State, fi.AmountCents, fi.Error, fi.Checkout
	}
}
