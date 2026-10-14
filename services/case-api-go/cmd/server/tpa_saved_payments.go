package main

// tpa_saved_payments.go — a TPA pays its disputes' fees with a saved card or
// US bank account (ACH debit), and chooses each time who pays:
//
//   - CLIENT: each client's own card or bank account is charged for that
//     client's disputes (one charge per client);
//   - TPA:    the TPA's own card or bank account is charged once for all;
//   - CHECKOUT: as before, Stripe's page (card or bank), paid there and then.
//
// Cards and bank accounts are saved on Stripe's page (Checkout in setup mode):
// no card or account number reaches this server. A bank account is verified
// by Stripe (instantly through the bank's login, or by two micro-deposits the
// payer confirms) and Stripe records the debit authorization (the mandate).
//
// A saved-method payment is run by the FeePaymentWorkflow on Temporal (task
// queue idre-payments, idre-workflows-py/fee_payments.py): it charges each
// group through the internal endpoints below, waits for bank debits to clear
// (days), wakes on Stripe's webhook or checks on a back-off, settles each
// group the moment it clears, marks declines and returns failed, and tells the
// TPA how the run went. Settling is the same as for a checkout
// (settlePaymentGroup), so a fee is never paid twice.
//
//   POST   /tpa/payment-methods/setup                  {client_id?} -> {setup_url}
//   POST   /tpa/payment-methods/setup/{sessionId}/confirm          (back from Stripe)
//   GET    /tpa/payment-methods[?client_id=]            the TPA's own, or one client's
//   DELETE /tpa/payment-methods/{methodId}
//   POST   /tpa/fees/options                            {case_id|client_id|batch_id}: what the dialog offers
//   any Pay endpoint with {"source":"CLIENT"|"TPA", ...}: charge saved methods
//   GET    /tpa/fees/runs/{runId}                       how a saved-method payment is going
//
//   internal (X-Internal-Auth = VAULT_INTERNAL_TOKEN; the workflow's activities):
//   POST /internal/payments/{tenant}/groups/{groupId}/charge
//   POST /internal/payments/{tenant}/groups/{groupId}/check
//   POST /internal/payments/{tenant}/groups/{groupId}/give-up
//   POST /internal/payments/{tenant}/runs/{runId}/summary

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	temporalclient "go.temporal.io/sdk/client"
)

const feePayQueue = "idre-payments"

// stripeCall is stripePost/stripeGet with an idempotency key and Stripe's
// error object (a decline carries its message, code and payment intent).
func (s *server) stripeCall(method, path string, form url.Values, idem string) (out, stripeErr map[string]any, err error) {
	if s.cfg.StripeSecret == "" {
		return nil, nil, fmt.Errorf("card payments not configured (STRIPE_SECRET_KEY unset)")
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, "https://api.stripe.com"+path, body)
	if err != nil {
		return nil, nil, err
	}
	req.SetBasicAuth(s.cfg.StripeSecret, "")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := stripeHTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(raw, &out) != nil {
		return nil, nil, fmt.Errorf("stripe: bad response")
	}
	if resp.StatusCode >= 400 {
		e, _ := out["error"].(map[string]any)
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if e != nil {
			msg = fmt.Sprint(e["message"])
		}
		return nil, e, fmt.Errorf("stripe: %s", msg)
	}
	return out, nil, nil
}

// ---- saved cards and bank accounts ----

type savedMethod struct {
	ID         string `json:"id"`
	ClientID   string `json:"client_id,omitempty"`
	Method     string `json:"method"` // CARD | ACH
	Label      string `json:"label"`
	Brand      string `json:"brand,omitempty"`
	BankName   string `json:"bank_name,omitempty"`
	Last4      string `json:"last4"`
	Status     string `json:"status"` // ACTIVE | PENDING_VERIFICATION | FAILED
	Chargeable bool   `json:"chargeable"`
	VerifyURL  string `json:"verify_url,omitempty"`
	CreatedAt  string `json:"created_at"`
}

func (m savedMethod) words() string {
	if m.Method == "ACH" {
		bank := m.BankName
		if bank == "" {
			bank = "bank account"
		}
		return fmt.Sprintf("%s ····%s", bank, m.Last4)
	}
	brand := m.Brand
	if brand == "" {
		brand = "card"
	}
	return fmt.Sprintf("%s ····%s", strings.ToUpper(brand[:1])+strings.ToLower(brand[1:]), m.Last4)
}

// savedMethods: the TPA's own (clientID "") or one client's methods, newest first.
// Chargeable: saved with Stripe and usable now (a placeholder token from the
// old form, or a bank account still waiting for its micro-deposits, is not).
func (s *server) savedMethods(ctx context.Context, tenant, tpaID, clientID string) []savedMethod {
	where := "client_id IS NULL"
	args := []any{tenant, tpaID}
	if clientID != "" {
		where = "client_id::text=$3"
		args = append(args, clientID)
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, coalesce(client_id::text,''), method, coalesce(label,''), coalesce(brand,''), coalesce(bank_name,''), last4,
		       status, (status='ACTIVE' AND verified AND coalesce(processor_ref,'') LIKE 'pm\_%' AND coalesce(stripe_customer,'')<>''),
		       coalesce(verify_url,''), created_at::text
		FROM public.tpa_payment_methods WHERE tenant=$1 AND tpa_id::text=$2 AND `+where+` AND status<>'REMOVED'
		ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []savedMethod
	for rows.Next() {
		var m savedMethod
		if rows.Scan(&m.ID, &m.ClientID, &m.Method, &m.Label, &m.Brand, &m.BankName, &m.Last4, &m.Status, &m.Chargeable, &m.VerifyURL, &m.CreatedAt) == nil {
			out = append(out, m)
		}
	}
	return out
}

// stripeCustomerFor: the payer's Stripe customer, created on first use.
func (s *server) stripeCustomerFor(ctx context.Context, tenant, ownerKind, ownerID, name, email string) (string, error) {
	var id string
	if s.db.QueryRow(ctx, `SELECT customer_id FROM public.stripe_customers WHERE tenant=$1 AND owner_kind=$2 AND owner_id::text=$3`,
		tenant, ownerKind, ownerID).Scan(&id) == nil {
		return id, nil
	}
	form := url.Values{}
	form.Set("name", name)
	if email != "" {
		form.Set("email", email)
	}
	form.Set("metadata[tenant]", tenant)
	form.Set("metadata[owner_kind]", ownerKind)
	form.Set("metadata[owner_id]", ownerID)
	c, _, err := s.stripeCall("POST", "/v1/customers", form, "cust-"+tenant+"-"+ownerKind+"-"+ownerID)
	if err != nil {
		return "", err
	}
	id, _ = c["id"].(string)
	if _, err := s.db.Exec(ctx, `INSERT INTO public.stripe_customers (tenant, owner_kind, owner_id, customer_id) VALUES ($1,$2,$3::uuid,$4)
		ON CONFLICT DO NOTHING`, tenant, ownerKind, ownerID, id); err != nil {
		return "", err
	}
	_ = s.db.QueryRow(ctx, `SELECT customer_id FROM public.stripe_customers WHERE tenant=$1 AND owner_kind=$2 AND owner_id::text=$3`,
		tenant, ownerKind, ownerID).Scan(&id) // a racing request's customer wins
	return id, nil
}

// tpaPaymentSetup: POST /tpa/payment-methods/setup -- open Stripe's page to
// save a card or bank account for the TPA itself or for one of its clients.
func (s *server) tpaPaymentSetup(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		ClientID string `json:"client_id"`
		Label    string `json:"label"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&in)
	ownerKind, ownerID, name, email := "tpa", org.ID, org.Name, org.ContactEmail
	if in.ClientID != "" {
		if s.db.QueryRow(r.Context(), `SELECT party_name, coalesce(contact_email,'') FROM public.tpa_clients WHERE tenant=$1 AND tpa_id::text=$2 AND id::text=$3`,
			tenant, org.ID, in.ClientID).Scan(&name, &email) != nil {
			http.Error(w, `{"error":"client not found"}`, http.StatusNotFound)
			return
		}
		ownerKind, ownerID = "tpa_client", in.ClientID
	}
	cust, err := s.stripeCustomerFor(r.Context(), tenant, ownerKind, ownerID, name, email)
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe("the payment page could not be opened: "+err.Error())+`"}`, http.StatusFailedDependency)
		return
	}
	back := strings.TrimRight(s.cfg.PortalBaseURL, "/") + "/app.html#/tpa"
	form := url.Values{}
	form.Set("mode", "setup")
	form.Set("currency", "usd")
	form.Set("customer", cust)
	// The methods offered (card, US bank account) are the Stripe Dashboard's
	// setting: Checkout no longer takes payment_method_types.
	// Only what can be charged again later without the payer: cards and US
	// bank accounts. Buy-now-pay-later and wallets are left out.
	for i, t := range []string{"klarna", "cashapp", "amazon_pay", "affirm", "afterpay_clearpay", "alipay", "wechat_pay", "paypal", "revolut_pay", "zip"} {
		form.Set(fmt.Sprintf("excluded_payment_method_types[%d]", i), t)
	}
	form.Set("payment_method_options[us_bank_account][verification_method]", "automatic")
	form.Set("success_url", back+"?setup={CHECKOUT_SESSION_ID}")
	form.Set("cancel_url", back+"?setup_cancelled=1")
	meta := map[string]string{"tenant": tenant, "purpose": "tpa_payment_method", "tpa_id": org.ID, "client_id": in.ClientID,
		"label": strings.TrimSpace(in.Label), "added_by": p.Subject}
	for k, v := range meta {
		form.Set("metadata["+k+"]", v)
		form.Set("setup_intent_data[metadata]["+k+"]", v)
	}
	sess, _, err := s.stripeCall("POST", "/v1/checkout/sessions", form, "")
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe("the payment page could not be opened: "+err.Error())+`"}`, http.StatusFailedDependency)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TPA_PAYMENT_SETUP_STARTED", map[string]any{"org": "tpa:" + org.ID, "by": p.Subject, "client_id": in.ClientID})
	writeJSON(w, http.StatusCreated, map[string]any{"setup_url": sess["url"], "session_id": sess["id"]})
}

// completeSetup records the card or bank account a setup session saved.
// Idempotent (the return page, the webhook and the reconcile pass may all
// call it). Returns the method's status.
func (s *server) completeSetup(ctx context.Context, sessionID string) (savedMethod, error) {
	sess, _, err := s.stripeCall("GET", "/v1/checkout/sessions/"+url.PathEscape(sessionID)+"?expand[]=setup_intent.payment_method", nil, "")
	if err != nil {
		return savedMethod{}, err
	}
	meta, _ := sess["metadata"].(map[string]any)
	if fmt.Sprint(meta["purpose"]) != "tpa_payment_method" {
		return savedMethod{}, fmt.Errorf("not a payment-method setup")
	}
	si, _ := sess["setup_intent"].(map[string]any)
	if si == nil {
		return savedMethod{}, fmt.Errorf("the card or account was not saved")
	}
	return s.recordSetupIntent(ctx, si, meta, fmt.Sprint(sess["customer"]))
}

func (s *server) recordSetupIntent(ctx context.Context, si map[string]any, meta map[string]any, customer string) (savedMethod, error) {
	tenant, tpaID, clientID := fmt.Sprint(meta["tenant"]), fmt.Sprint(meta["tpa_id"]), fmt.Sprint(meta["client_id"])
	if clientID == "<nil>" {
		clientID = ""
	}
	pm, _ := si["payment_method"].(map[string]any)
	if pm == nil {
		if id, ok := si["payment_method"].(string); ok && id != "" {
			pm, _, _ = s.stripeCall("GET", "/v1/payment_methods/"+url.PathEscape(id), nil, "")
		}
	}
	if pm == nil {
		return savedMethod{}, fmt.Errorf("the card or account was not saved")
	}
	m := savedMethod{Label: strings.TrimSpace(fmt.Sprint(meta["label"]))}
	if m.Label == "<nil>" {
		m.Label = ""
	}
	switch pm["type"] {
	case "card":
		c, _ := pm["card"].(map[string]any)
		m.Method, m.Brand, m.Last4 = "CARD", strings.ToUpper(fmt.Sprint(c["brand"])), fmt.Sprint(c["last4"])
	case "link": // a card kept in the payer's Link wallet: reusable off-session like a card
		m.Method, m.Brand, m.Last4 = "CARD", "LINK", "link"
	case "us_bank_account":
		b, _ := pm["us_bank_account"].(map[string]any)
		m.Method, m.BankName, m.Last4 = "ACH", fmt.Sprint(b["bank_name"]), fmt.Sprint(b["last4"])
	default:
		return savedMethod{}, fmt.Errorf("unsupported payment method %v", pm["type"])
	}
	m.Status = "ACTIVE"
	switch si["status"] {
	case "succeeded":
	case "requires_action", "processing":
		m.Status = "PENDING_VERIFICATION" // micro-deposits on their way
		if na, ok := si["next_action"].(map[string]any); ok {
			if v, ok := na["verify_with_microdeposits"].(map[string]any); ok {
				m.VerifyURL, _ = v["hosted_verification_url"].(string)
			}
		}
	default:
		m.Status = "FAILED"
	}
	mandate, _ := si["mandate"].(string)
	var clientArg any
	if clientID != "" {
		clientArg = clientID
	}
	addedBy := fmt.Sprint(meta["added_by"])
	err := s.db.QueryRow(ctx, `
		INSERT INTO public.tpa_payment_methods
		  (tenant, tpa_id, client_id, method, label, brand, bank_name, last4, processor_ref, stripe_customer, setup_intent, mandate,
		   verify_url, auth_received, verified, status, added_by)
		VALUES ($1,$2::uuid,$3::uuid,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),$8,$9,$10,$11,NULLIF($12,''),NULLIF($13,''),
		        $4='ACH', $14='ACTIVE', $14, $15)
		ON CONFLICT (processor_ref) WHERE processor_ref LIKE 'pm\_%' DO UPDATE SET
		  status=EXCLUDED.status, verified=EXCLUDED.verified, mandate=coalesce(EXCLUDED.mandate, tpa_payment_methods.mandate),
		  verify_url=EXCLUDED.verify_url, updated_at=now()
		RETURNING id::text, created_at::text`,
		tenant, tpaID, clientArg, m.Method, m.Label, m.Brand, m.BankName, m.Last4, fmt.Sprint(pm["id"]), customer,
		fmt.Sprint(si["id"]), mandate, m.VerifyURL, m.Status, addedBy).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return savedMethod{}, err
	}
	m.ClientID, m.Chargeable = clientID, m.Status == "ACTIVE"
	if clientID != "" {
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
		s.recomputeClientStatus(r, tenant, clientID)
	}
	s.logAudit(ctx, tenant, "", "TPA_PAYMENT_ADDED", map[string]any{"org": "tpa:" + tpaID, "by": addedBy, "client_id": clientID,
		"method": m.Method, "last4": m.Last4, "status": m.Status, "via": "stripe"})
	return m, nil
}

// tpaPaymentSetupConfirm: back from Stripe's page.
func (s *server) tpaPaymentSetupConfirm(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
		return
	}
	m, err := s.completeSetup(r.Context(), chi.URLParam(r, "sessionId"))
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusBadRequest)
		return
	}
	var owner string
	_ = s.db.QueryRow(r.Context(), `SELECT tpa_id::text FROM public.tpa_payment_methods WHERE id::text=$1`, m.ID).Scan(&owner)
	if owner != org.ID {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// tpaSavedMethodsList: GET /tpa/payment-methods[?client_id=].
func (s *server) tpaSavedMethodsList(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payment_methods": s.savedMethods(r.Context(), tenant, org.ID, r.URL.Query().Get("client_id"))})
}

// tpaSavedMethodRemove: DELETE /tpa/payment-methods/{methodId} -- detached at
// Stripe too, so it can never be charged again.
func (s *server) tpaSavedMethodRemove(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
		return
	}
	var pm, clientID string
	if s.db.QueryRow(r.Context(), `
		UPDATE public.tpa_payment_methods SET status='REMOVED', updated_at=now()
		WHERE tenant=$1 AND tpa_id::text=$2 AND id::text=$3 AND status<>'REMOVED'
		RETURNING coalesce(processor_ref,''), coalesce(client_id::text,'')`, tenant, org.ID, chi.URLParam(r, "methodId")).Scan(&pm, &clientID) != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if strings.HasPrefix(pm, "pm_") {
		_, _, _ = s.stripeCall("POST", "/v1/payment_methods/"+url.PathEscape(pm)+"/detach", url.Values{}, "")
	}
	if clientID != "" {
		s.recomputeClientStatus(r, tenant, clientID)
	}
	s.logAudit(r.Context(), tenant, "", "TPA_PAYMENT_REMOVED", map[string]any{"org": "tpa:" + org.ID, "by": p.Subject, "client_id": clientID, "method_id": chi.URLParam(r, "methodId")})
	writeJSON(w, http.StatusOK, map[string]any{"status": "REMOVED"})
}

// ---- paying with saved methods ----

type feeByClient struct {
	ClientID   string        `json:"client_id"`
	Name       string        `json:"name"`
	Disputes   int           `json:"disputes"`
	Amount     int64         `json:"amount_cents"`
	Methods    []savedMethod `json:"methods"`
	invoiceIDs []string
	caseIDs    []string
}

// feesByClient splits the fees in scope by the client each dispute is for.
func (s *server) feesByClient(ctx context.Context, tenant string, py payer, fees []unpaidFee) []*feeByClient {
	caseIDs := make([]string, 0, len(fees))
	for _, f := range fees {
		caseIDs = append(caseIDs, f.caseID)
	}
	client := map[string]string{}
	names := map[string]string{}
	rows, err := s.db.Query(ctx, `
		SELECT o.case_id, coalesce(o.client_id::text,''), coalesce(c.party_name,'')
		FROM public.case_origin o LEFT JOIN public.tpa_clients c ON c.id=o.client_id
		WHERE o.tenant=$1 AND o.case_id = ANY($2)`, tenant, caseIDs)
	if err == nil {
		for rows.Next() {
			var cid, clid, name string
			if rows.Scan(&cid, &clid, &name) == nil {
				client[cid], names[clid] = clid, name
			}
		}
		rows.Close()
	}
	by := map[string]*feeByClient{}
	var order []*feeByClient
	for _, f := range fees {
		clid := client[f.caseID]
		g := by[clid]
		if g == nil {
			g = &feeByClient{ClientID: clid, Name: names[clid]}
			if clid == "" {
				g.Name = "Disputes not filed for a client"
			}
			by[clid] = g
			order = append(order, g)
		}
		g.Disputes++
		g.Amount += f.amount
		g.invoiceIDs = append(g.invoiceIDs, f.invoiceID)
		g.caseIDs = append(g.caseIDs, f.caseID)
	}
	for _, g := range order {
		if g.ClientID != "" {
			g.Methods = s.savedMethods(ctx, tenant, py.id, g.ClientID)
		}
	}
	return order
}

// tpaFeeOptions: POST /tpa/fees/options -- what the Pay dialog offers for a
// scope: the total, each client's share with its saved methods, the TPA's own.
func (s *server) tpaFeeOptions(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	py, err := s.payerFor(r, tenant, "tpa")
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
		return
	}
	var in struct {
		CaseID   string `json:"case_id"`
		ClientID string `json:"client_id"`
		BatchID  string `json:"batch_id"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&in)
	if in.CaseID != "" && !s.payerOwnsCase(r.Context(), tenant, py, in.CaseID) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	fees, err := s.unpaidFees(r.Context(), tenant, py, feeScope{caseID: in.CaseID, clientID: in.ClientID, batchID: in.BatchID})
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	var total int64
	for _, f := range fees {
		total += f.amount
	}
	writeJSON(w, http.StatusOK, map[string]any{"disputes": len(fees), "amount_cents": total,
		"clients": s.feesByClient(r.Context(), tenant, py, fees), "tpa_methods": s.savedMethods(r.Context(), tenant, py.id, "")})
}

type savedPayRequest struct {
	Source          string            `json:"source"`            // CLIENT | TPA | CHECKOUT
	PaymentMethodID string            `json:"payment_method_id"` // TPA: which of its methods
	ClientMethods   map[string]string `json:"client_methods"`    // CLIENT: client_id -> method (default: its newest chargeable)
}

// feeChargeSaved charges the fees in scope to saved methods and hands the
// charges to the FeePaymentWorkflow.
func (s *server) feeChargeSaved(w http.ResponseWriter, r *http.Request, tenant string, py payer, sc feeScope, in savedPayRequest) {
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
	type charge struct {
		label, clientID string
		method          savedMethod
		invoiceIDs      []string
		caseIDs         []string
		amount          int64
	}
	var charges []charge
	var skipped []map[string]any
	pick := func(ms []savedMethod, want string) (savedMethod, bool) {
		for _, m := range ms {
			if m.Chargeable && (want == "" || m.ID == want) {
				return m, true
			}
		}
		return savedMethod{}, false
	}
	switch in.Source {
	case "TPA":
		m, ok := pick(s.savedMethods(r.Context(), tenant, py.id, ""), in.PaymentMethodID)
		if !ok {
			http.Error(w, `{"error":"add a card or bank account for your organization first (Payment methods), or pick one that is ready"}`, http.StatusConflict)
			return
		}
		c := charge{label: sc.label, method: m}
		for _, f := range fees {
			c.invoiceIDs, c.caseIDs, c.amount = append(c.invoiceIDs, f.invoiceID), append(c.caseIDs, f.caseID), c.amount+f.amount
		}
		charges = append(charges, c)
	case "CLIENT":
		for _, g := range s.feesByClient(r.Context(), tenant, py, fees) {
			m, ok := pick(g.Methods, in.ClientMethods[g.ClientID])
			if !ok {
				skipped = append(skipped, map[string]any{"client_id": g.ClientID, "name": g.Name, "disputes": g.Disputes, "amount_cents": g.Amount,
					"reason": "no card or bank account ready on file for this client"})
				continue
			}
			charges = append(charges, charge{label: g.Name, clientID: g.ClientID, method: m, invoiceIDs: g.invoiceIDs, caseIDs: g.caseIDs, amount: g.Amount})
		}
		if len(charges) == 0 {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "none of these clients has a card or bank account ready on file — add one in the Clients tab, or pay with your organization's", "skipped": skipped})
			return
		}
	default:
		http.Error(w, `{"error":"source must be CLIENT, TPA or CHECKOUT"}`, http.StatusBadRequest)
		return
	}
	var all []string
	for _, c := range charges {
		all = append(all, c.invoiceIDs...)
	}
	if !s.expireOverlapping(r.Context(), tenant, all) {
		http.Error(w, `{"error":"an earlier payment for some of these disputes is being completed — check again in a minute"}`, http.StatusConflict)
		return
	}
	var runID string
	_ = s.db.QueryRow(r.Context(), `SELECT gen_random_uuid()::text`).Scan(&runID)
	source := map[string]string{"TPA": "TPA_METHOD", "CLIENT": "CLIENT_METHOD"}[in.Source]
	wfID := "fee-pay-" + runID
	var groups []string
	var out []map[string]any
	for _, c := range charges {
		var clientArg, caseArg any
		if c.clientID != "" {
			clientArg = c.clientID
		}
		if sc.caseID != "" {
			caseArg = sc.caseID
		}
		var batchArg any
		if sc.batchID != "" {
			batchArg = sc.batchID
		}
		var gid string
		if err := s.db.QueryRow(r.Context(), `
			INSERT INTO public.payment_groups (tenant, batch_id, tpa_id, client_id, case_id, scope, invoice_ids, amount_cents, created_by,
			                                   status, source, payment_method_id, run_id, workflow_id)
			VALUES ($1,$2::uuid,$3::uuid,$4::uuid,$5,$6,$7::uuid[],$8,$9,'PROCESSING',$10,$11::uuid,$12::uuid,$13) RETURNING id::text`,
			tenant, batchArg, py.id, clientArg, caseArg, c.label, c.invoiceIDs, c.amount, p.Subject, source, c.method.ID, runID, wfID).Scan(&gid); err != nil {
			slog.Error("saved payment group", "err", err)
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		_, _ = s.db.Exec(r.Context(), `UPDATE public.payment_groups SET session_id='pg_'||id::text WHERE id::text=$1`, gid)
		for i, inv := range c.invoiceIDs {
			var amount int64
			_ = s.db.QueryRow(r.Context(), `SELECT amount_cents FROM public.invoices WHERE id::text=$1`, inv).Scan(&amount)
			_, _ = s.db.Exec(r.Context(), `
				INSERT INTO public.payments (tenant, case_id, invoice_id, session_id, amount_cents, payer_org, raw)
				VALUES ($1,$2,$3,$4,$5,$6,'{}'::jsonb) ON CONFLICT (session_id) DO NOTHING`,
				tenant, c.caseIDs[i], inv, "pg_"+gid+"#"+inv, amount, py.name)
			s.finEvent(r, tenant, c.caseIDs[i], inv, "PAYMENT_INITIATED", "NONE", amount, "PROVIDER", "pg_"+gid, "saved-"+strings.ToLower(c.method.Method))
		}
		groups = append(groups, gid)
		out = append(out, map[string]any{"payment_group_id": gid, "for": c.label, "disputes": len(c.invoiceIDs), "amount_cents": c.amount,
			"method": c.method.words(), "rail": c.method.Method})
	}
	if _, err := s.tc.ExecuteWorkflow(r.Context(), temporalclient.StartWorkflowOptions{ID: wfID, TaskQueue: feePayQueue},
		"FeePaymentWorkflow", map[string]any{"tenant": tenant, "run_id": runID, "group_ids": groups}); err != nil {
		slog.Error("fee payment workflow", "err", err)
		for _, g := range groups {
			s.closeGroup(r.Context(), tenant, g, "FAILED", "the payment could not be started — nothing was charged; try again")
		}
		http.Error(w, `{"error":"the payment could not be started — nothing was charged; try again"}`, http.StatusServiceUnavailable)
		return
	}
	s.logAudit(r.Context(), tenant, sc.caseID, "FEE_CHARGE_SAVED", map[string]any{"org": py.org(), "by": p.Subject, "scope": sc.label,
		"source": source, "run_id": runID, "charges": len(groups), "skipped": len(skipped), "batch_id": sc.batchID, "client_id": sc.clientID})
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": runID, "source": in.Source, "charges": out, "skipped": skipped})
}

// tpaFeeRun: GET /tpa/fees/runs/{runId} -- each charge of a saved-method payment.
func (s *server) tpaFeeRun(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	py, err := s.payerFor(r, tenant, "tpa")
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
		return
	}
	rows, err := s.queryRows(r, `
		SELECT g.id::text AS payment_group_id, coalesce(g.scope,'') AS for, g.status, g.amount_cents, cardinality(g.invoice_ids) AS disputes,
		       coalesce(g.last_error,'') AS error, g.source, coalesce(m.method,'') AS rail,
		       coalesce(m.brand,'') AS brand, coalesce(m.bank_name,'') AS bank_name, coalesce(m.last4,'') AS last4,
		       coalesce(g.paid_at::text,'') AS paid_at
		FROM public.payment_groups g LEFT JOIN public.tpa_payment_methods m ON m.id=g.payment_method_id
		WHERE g.tenant=$1 AND g.tpa_id::text=$2 AND g.run_id::text=$3 ORDER BY g.created_at`, tenant, py.id, chi.URLParam(r, "runId"))
	if err != nil || len(rows) == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": chi.URLParam(r, "runId"), "charges": rows})
}

// ---- the workflow's activities (internal) ----

func internalAuthed(w http.ResponseWriter, r *http.Request) bool {
	tok := os.Getenv("VAULT_INTERNAL_TOKEN")
	if tok == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Auth")), []byte(tok)) != 1 {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

type savedGroup struct {
	id, tenant, status, source, runID, label, clientID, tpaID, mandate string
	amount                                                             int64
	method                                                             savedMethod
	pm, customer, pi                                                   string
}

func (s *server) loadSavedGroup(ctx context.Context, tenant, gid string) (savedGroup, error) {
	g := savedGroup{id: gid, tenant: tenant}
	err := s.db.QueryRow(ctx, `
		SELECT g.status, g.source, coalesce(g.run_id::text,''), coalesce(g.scope,''), coalesce(g.client_id::text,''), coalesce(g.tpa_id::text,''),
		       g.amount_cents, coalesce(g.payment_intent,''),
		       m.id::text, m.method, coalesce(m.brand,''), coalesce(m.bank_name,''), m.last4, coalesce(m.processor_ref,''),
		       coalesce(m.stripe_customer,''), coalesce(m.mandate,''), m.status
		FROM public.payment_groups g JOIN public.tpa_payment_methods m ON m.id=g.payment_method_id
		WHERE g.tenant=$1 AND g.id::text=$2`, tenant, gid).Scan(&g.status, &g.source, &g.runID, &g.label, &g.clientID, &g.tpaID,
		&g.amount, &g.pi, &g.method.ID, &g.method.Method, &g.method.Brand, &g.method.BankName, &g.method.Last4, &g.pm,
		&g.customer, &g.mandate, &g.method.Status)
	return g, err
}

// failSavedGroup: the charge did not go through; the disputes stay unpaid.
func (s *server) failSavedGroup(ctx context.Context, g savedGroup, why string) {
	s.closeGroup(ctx, g.tenant, g.id, "FAILED", why)
	s.logAudit(ctx, g.tenant, "", "FEE_CHARGE_FAILED", map[string]any{"org": "tpa:" + g.tpaID, "by": "system", "payment_group_id": g.id,
		"run_id": g.runID, "method": g.method.words(), "reason": why})
}

// piOutcome settles, fails or leaves pending a group from its payment intent.
func (s *server) piOutcome(ctx context.Context, g savedGroup, pi map[string]any) string {
	piID := fmt.Sprint(pi["id"])
	if piID != "" && piID != "<nil>" {
		_, _ = s.db.Exec(ctx, `UPDATE public.payment_groups SET payment_intent=$3, updated_at=now() WHERE tenant=$1 AND id::text=$2`, g.tenant, g.id, piID)
	}
	switch pi["status"] {
	case "succeeded":
		if got := toInt64(pi["amount_received"]); got != g.amount {
			s.flagPayment(ctx, g.tenant, g.id, fmt.Sprintf("Stripe took %s for a charge of %s — not settled, check it", usd(got), usd(g.amount)))
			return "REVIEW"
		}
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
		s.settlePaymentGroup(r, g.tenant, g.id, "pg_"+g.id, piID, "")
		return "PAID"
	case "processing":
		return "PROCESSING"
	case "requires_payment_method", "canceled":
		why := "the payment did not go through"
		if e, ok := pi["last_payment_error"].(map[string]any); ok && e["message"] != nil {
			why = fmt.Sprint(e["message"])
		}
		s.failSavedGroup(ctx, g, why)
		return "FAILED"
	case "requires_action":
		s.failSavedGroup(ctx, g, "the card's bank asked for a confirmation that cannot be given for a saved card — pay on Stripe's page instead")
		return "FAILED"
	}
	return "PROCESSING"
}

// internalChargeGroup: create (or, on a retried activity, find again by its
// idempotency key) the payment for one group.
func (s *server) internalChargeGroup(w http.ResponseWriter, r *http.Request) {
	if !internalAuthed(w, r) {
		return
	}
	tenant, gid := chi.URLParam(r, "tenant"), chi.URLParam(r, "groupId")
	g, err := s.loadSavedGroup(r.Context(), tenant, gid)
	if err != nil {
		http.Error(w, `{"error":"no such payment"}`, http.StatusNotFound)
		return
	}
	if g.status != "PROCESSING" || g.pi != "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": g.status}) // charged already: the check follows it
		return
	}
	if g.method.Status != "ACTIVE" || !strings.HasPrefix(g.pm, "pm_") {
		s.failSavedGroup(r.Context(), g, "the "+g.method.words()+" is no longer on file")
		writeJSON(w, http.StatusOK, map[string]any{"status": "FAILED"})
		return
	}
	form := url.Values{}
	form.Set("amount", fmt.Sprint(g.amount))
	form.Set("currency", "usd")
	form.Set("customer", g.customer)
	form.Set("payment_method", g.pm)
	form.Set("confirm", "true")
	form.Set("off_session", "true")
	form.Set("description", "IDRE initial review fee — "+g.label)
	// Stripe no longer takes payment_method_types: the saved method decides,
	// and nothing that needs the payer at a redirect is allowed off-session.
	form.Set("automatic_payment_methods[enabled]", "true")
	form.Set("automatic_payment_methods[allow_redirects]", "never")
	if g.method.Method == "ACH" && g.mandate != "" {
		form.Set("mandate", g.mandate)
	}
	for k, v := range map[string]string{"tenant": tenant, "payment_group_id": gid, "run_id": g.runID, "source": g.source} {
		form.Set("metadata["+k+"]", v)
	}
	pi, se, err := s.stripeCall("POST", "/v1/payment_intents", form, "fee-charge-"+gid)
	if err != nil {
		if se != nil {
			// A decline: Stripe answers 402 with the payment intent it made.
			if p, ok := se["payment_intent"].(map[string]any); ok {
				if p["last_payment_error"] == nil {
					p["last_payment_error"] = map[string]any{"message": se["message"]}
				}
				writeJSON(w, http.StatusOK, map[string]any{"status": s.piOutcome(r.Context(), g, p)})
				return
			}
			if t := fmt.Sprint(se["type"]); t == "card_error" || t == "invalid_request_error" {
				s.failSavedGroup(r.Context(), g, fmt.Sprint(se["message"]))
				writeJSON(w, http.StatusOK, map[string]any{"status": "FAILED"})
				return
			}
		}
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusBadGateway) // Stripe unreachable: the activity retries
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": s.piOutcome(r.Context(), g, pi)})
}

// internalCheckGroup: where a pending charge (a bank debit) has got to.
func (s *server) internalCheckGroup(w http.ResponseWriter, r *http.Request) {
	if !internalAuthed(w, r) {
		return
	}
	tenant, gid := chi.URLParam(r, "tenant"), chi.URLParam(r, "groupId")
	g, err := s.loadSavedGroup(r.Context(), tenant, gid)
	if err != nil {
		http.Error(w, `{"error":"no such payment"}`, http.StatusNotFound)
		return
	}
	if g.status != "PROCESSING" {
		writeJSON(w, http.StatusOK, map[string]any{"status": g.status})
		return
	}
	if g.pi == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "NOT_CHARGED"})
		return
	}
	pi, _, err := s.stripeCall("GET", "/v1/payment_intents/"+url.PathEscape(g.pi), nil, "")
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": s.piOutcome(r.Context(), g, pi)})
}

// internalGiveUpGroup: a bank debit still pending after the workflow's wait.
// Money may yet arrive, so finance looks at it (never marked failed here).
func (s *server) internalGiveUpGroup(w http.ResponseWriter, r *http.Request) {
	if !internalAuthed(w, r) {
		return
	}
	tenant, gid := chi.URLParam(r, "tenant"), chi.URLParam(r, "groupId")
	s.flagPayment(r.Context(), tenant, gid, "a bank debit has been pending for longer than its window — check it with Stripe")
	writeJSON(w, http.StatusOK, map[string]any{"status": "REVIEW"})
}

// internalRunSummary tells the TPA how a saved-method payment went, once
// every charge in it has an outcome.
func (s *server) internalRunSummary(w http.ResponseWriter, r *http.Request) {
	if !internalAuthed(w, r) {
		return
	}
	tenant, runID := chi.URLParam(r, "tenant"), chi.URLParam(r, "runId")
	rows, err := s.db.Query(r.Context(), `
		SELECT coalesce(g.tpa_id::text,''), g.status, g.amount_cents, cardinality(g.invoice_ids), coalesce(g.scope,''), coalesce(g.last_error,''),
		       m.method, coalesce(m.brand,''), coalesce(m.bank_name,''), m.last4
		FROM public.payment_groups g JOIN public.tpa_payment_methods m ON m.id=g.payment_method_id
		WHERE g.tenant=$1 AND g.run_id::text=$2 ORDER BY g.created_at`, tenant, runID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	var tpaID string
	var paid, failed, review int
	var paidAmt int64
	var lines []string
	for rows.Next() {
		var st, label, why string
		var amount int64
		var n int
		var m savedMethod
		if rows.Scan(&tpaID, &st, &amount, &n, &label, &why, &m.Method, &m.Brand, &m.BankName, &m.Last4) != nil {
			continue
		}
		switch st {
		case "PAID":
			paid += n
			paidAmt += amount
		case "FAILED":
			failed += n
			lines = append(lines, fmt.Sprintf("%s: %s %s was not charged — %s", label, m.words(), usd(amount), why))
		default:
			review += n
		}
	}
	rows.Close()
	if tpaID == "" {
		http.Error(w, `{"error":"no such run"}`, http.StatusNotFound)
		return
	}
	body := fmt.Sprintf("Payment finished: %d dispute%s paid (%s)", paid, map[bool]string{true: "", false: "s"}[paid == 1], usd(paidAmt))
	if failed > 0 {
		body += fmt.Sprintf("; %d not paid. %s", failed, strings.Join(lines, "; "))
	}
	if review > 0 {
		body += fmt.Sprintf("; %d being confirmed by our finance team", review)
	}
	typ := "PAYMENT_RUN_FINISHED"
	if failed > 0 {
		typ = "PAYMENT_DECLINED"
	}
	s.notifyOrg(r.Context(), tenant, "tpa", tpaID, "", typ, body, "#/tpa")
	writeJSON(w, http.StatusOK, map[string]any{"paid": paid, "failed": failed, "review": review})
}

// signalFeeRun wakes a run's workflow when Stripe reports on one of its charges.
func (s *server) signalFeeRun(ctx context.Context, meta map[string]string) {
	if meta["run_id"] == "" || meta["payment_group_id"] == "" {
		return
	}
	if err := s.tc.SignalWorkflow(ctx, "fee-pay-"+meta["run_id"], "", "payment_update", meta["payment_group_id"]); err != nil {
		slog.Warn("fee run signal", "run", meta["run_id"], "err", err) // the workflow's own checks still find it
	}
}

// reconcilePendingSetups: bank accounts waiting for micro-deposits become
// usable once confirmed, without waiting on a webhook.
func (s *server) reconcilePendingSetups(ctx context.Context) {
	if s.cfg.StripeSecret == "" {
		return
	}
	rows, err := s.db.Query(ctx, `
		SELECT tenant, tpa_id::text, coalesce(client_id::text,''), coalesce(label,''), coalesce(added_by,''), setup_intent, coalesce(stripe_customer,'')
		FROM public.tpa_payment_methods WHERE status='PENDING_VERIFICATION' AND setup_intent IS NOT NULL
		  AND updated_at < now() - interval '2 minutes' AND created_at > now() - interval '14 days' LIMIT 20`)
	if err != nil {
		return
	}
	type pend struct{ tenant, tpa, client, label, by, si, cust string }
	var list []pend
	for rows.Next() {
		var p pend
		if rows.Scan(&p.tenant, &p.tpa, &p.client, &p.label, &p.by, &p.si, &p.cust) == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	for _, p := range list {
		_, _ = s.db.Exec(ctx, `UPDATE public.tpa_payment_methods SET updated_at=now() WHERE setup_intent=$1`, p.si)
		si, _, err := s.stripeCall("GET", "/v1/setup_intents/"+url.PathEscape(p.si)+"?expand[]=payment_method", nil, "")
		if err != nil || si["status"] == "requires_action" {
			continue
		}
		_, _ = s.recordSetupIntent(ctx, si, map[string]any{"tenant": p.tenant, "tpa_id": p.tpa, "client_id": p.client, "label": p.label, "added_by": p.by}, p.cust)
	}
}

// handleSavedPaymentEvent: the webhook's share. true when the event was ours.
func (s *server) handleSavedPaymentEvent(ctx context.Context, typ string, obj json.RawMessage) bool {
	switch typ {
	case "checkout.session.completed":
		var sess struct {
			ID       string            `json:"id"`
			Mode     string            `json:"mode"`
			Metadata map[string]string `json:"metadata"`
		}
		if json.Unmarshal(obj, &sess) != nil || sess.Mode != "setup" {
			return false
		}
		if sess.Metadata["purpose"] == "tpa_payment_method" {
			if _, err := s.completeSetup(ctx, sess.ID); err != nil {
				slog.Warn("payment method setup", "session", sess.ID, "err", err)
			}
		}
		return true
	case "setup_intent.succeeded", "setup_intent.setup_failed":
		var si struct {
			Metadata map[string]string `json:"metadata"`
			Customer string            `json:"customer"`
		}
		if json.Unmarshal(obj, &si) != nil || si.Metadata["purpose"] != "tpa_payment_method" {
			return false
		}
		var m map[string]any
		_ = json.Unmarshal(obj, &m)
		meta := map[string]any{}
		for k, v := range si.Metadata {
			meta[k] = v
		}
		_, _ = s.recordSetupIntent(ctx, m, meta, si.Customer)
		return true
	case "payment_intent.succeeded", "payment_intent.processing", "payment_intent.payment_failed", "payment_intent.canceled":
		var pi struct {
			Metadata map[string]string `json:"metadata"`
		}
		if json.Unmarshal(obj, &pi) == nil && pi.Metadata["run_id"] != "" {
			s.signalFeeRun(ctx, pi.Metadata)
			return typ != "payment_intent.payment_failed" // the older handler still records the decline message
		}
	}
	return false
}
