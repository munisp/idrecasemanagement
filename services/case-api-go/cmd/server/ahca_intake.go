package main

// Public pre-case intake for programmed tenants (FL AHCA CDR today, any
// future state that adopts program_rules the same way). Mirrors
// publicApply's shape (per-IP throttle, tenant validated) but produces a
// real case row instead of a stakeholder_applications row: AHCA's Filing
// Party needs upload/download links before any case exists, and the
// share-link system is hard-wired to a case_id that must already exist.
// Decision (scoped with the user): create the case row immediately rather
// than fork a parallel intake-scoped document system -- this reuses
// ShareBox, documents, notes, and correspondence completely unmodified.
//
// PAYMENT GATE: when the program charges an initial review fee, the case is
// created in AWAITING_PAYMENT and the party receives only a payment link;
// the case number, filing instructions, and upload link are released by
// activatePaidIntake the moment the fee settles (Stripe webhook or offline
// settlement), and shareUpload refuses AWAITING_PAYMENT cases outright.
// With no fee configured the intake completes immediately (PENDING_INTAKE).
// An abandoned shell case with no packet is closed CLOSED_REFUNDED by
// AhcaDisputeWorkflow's own 7-day refund-window wait.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	temporalclient "go.temporal.io/sdk/client"
)

func (s *server) publicAhcaIntake(w http.ResponseWriter, r *http.Request) {
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = strings.Split(fwd, ",")[0]
	}
	if !s.rateLimit("ahca-intake:"+ip, 10, 3600) {
		http.Error(w, `{"error":"rate limited — try again later"}`, http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in struct {
		Tenant              string `json:"tenant"`
		Email               string `json:"email"`
		Contact             string `json:"contact_name"`
		Org                 string `json:"org"`
		FilingPartyType     string `json:"filing_party_type"` // PROVIDER|HEALTH_PLAN (default PROVIDER)
		DisputedAmountCents int64  `json:"disputed_amount_cents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		in.Tenant == "" || in.Email == "" || !strings.Contains(in.Email, "@") {
		http.Error(w, `{"error":"tenant and a valid email are required"}`, http.StatusBadRequest)
		return
	}
	fpt := strings.ToUpper(strings.TrimSpace(in.FilingPartyType))
	if fpt == "" {
		fpt = "PROVIDER"
	}
	if fpt != "PROVIDER" && fpt != "HEALTH_PLAN" {
		http.Error(w, `{"error":"filing_party_type must be PROVIDER or HEALTH_PLAN"}`, http.StatusBadRequest)
		return
	}
	tenant := strings.ToLower(strings.TrimSpace(in.Tenant))
	var exists bool
	if err := s.db.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM public.state_config WHERE tenant=$1)`, tenant).Scan(&exists); err != nil || !exists {
		http.Error(w, `{"error":"unknown state program"}`, http.StatusBadRequest)
		return
	}
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		http.Error(w, `{"error":"this tenant has no pre-case intake program configured"}`, http.StatusBadRequest)
		return
	}
	caseID, caseNumber, err := s.startAhcaCase(r, tenant, cfg, in.Email, in.Contact, in.Org, fpt, in.DisputedAmountCents)
	if err != nil {
		if isUniqueViolation(err) {
			http.Error(w, `{"error":"case_number already exists — retry"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"case creation failed"}`, http.StatusBadGateway)
		return
	}
	// A payment-gated intake must not disclose the case identifiers to the
	// public caller — the party learns the case number only in the
	// post-payment email.
	var status string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&status)
	if status == "AWAITING_PAYMENT" {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status": "AWAITING_PAYMENT", "payment_required": true,
			"message": "A secure payment link for the initial review fee has been emailed to you. Your case number and upload link follow once payment is confirmed.",
		})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"case_id": caseID, "case_number": caseNumber, "status": "PENDING_INTAKE", "filing_party_type": fpt,
	})
}

// startAhcaCase is the real intake moment shared by two entry points: the
// public no-login front door (publicAhcaIntake, above) and createIntake's
// programmed-tenant branch (programops.go) -- staff turning an inbound
// phone/email request into a case, which per the source documents is the
// actual standard path (there was never a public self-service web form in
// the real Capitol Bridge process; AHCA's Filing Party has always reached
// them by phone or email). Both produce the exact same real case, links,
// invoice, and email -- only the trigger differs.
func (s *server) startAhcaCase(r *http.Request, tenant string, cfg *ProgramConfig, email, contactName, org, filingPartyType string, disputedAmountCents int64) (caseID, caseNumber string, err error) {
	return s.startAhcaCaseOpt(r, tenant, cfg, email, contactName, org, filingPartyType, disputedAmountCents, false)
}

// startAhcaCaseOpt adds the bulk-invoicing-arrangement switch: when deferFee
// is true (filer org is on config.fees.invoiced_filer_orgs and the intake came
// through the bulk path), the dispute opens immediately on acceptance and the
// initial fee is raised as an OPEN invoice to be collected later — the second
// opening path in the AHCA 2026 "Payment before submission" rules. Every
// other filer still pays before the dispute exists.
func (s *server) startAhcaCaseOpt(r *http.Request, tenant string, cfg *ProgramConfig, email, contactName, org, filingPartyType string, disputedAmountCents int64, deferFee bool) (caseID, caseNumber string, err error) {
	caseNumber = s.nextCaseNumber(r, tenant, cfg)
	// requester_* lands in details so the pre-case intake list can show who
	// asked and search by email/org -- previously nothing persisted this on
	// the case at all (only used in-flight, for the email and the
	// call_log/inquiry_log backfill below), so the real-case intake path
	// had no way to display requester info anywhere once the case existed.
	detailsJSON, _ := json.Marshal(map[string]string{
		"requester_email": email, "requester_contact_name": contactName, "requester_org": org,
		"filing_party_type": filingPartyType,
	})
	if err = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		INSERT INTO tenant_%s.cases (case_number, status, details, disputed_amount_cents)
		VALUES ($1, 'PENDING_INTAKE', $2, nullif($3,0)) RETURNING id`, sanitizeTenant(tenant)),
		caseNumber, detailsJSON, disputedAmountCents).Scan(&caseID); err != nil {
		return "", "", err
	}
	s.logAudit(r.Context(), tenant, caseID, "CASE_INTAKE_OPENED", map[string]any{
		"case_number": caseNumber, "requester_email": email, "requester_org": org,
		"disputed_amount_cents": disputedAmountCents,
	})

	s.ensureChecklist(r, tenant, caseID)

	// Backfill pre-case calls/inquiries logged against this email before the
	// case existed -- PLUM CRM's own documented gap was that such contacts
	// never got linked to the case once one opened; here they do.
	_, _ = s.db.Exec(r.Context(),
		`UPDATE public.call_log SET case_id=$1 WHERE tenant=$2 AND requester_email=$3 AND case_id IS NULL`,
		caseID, tenant, email)
	_, _ = s.db.Exec(r.Context(),
		`UPDATE public.inquiry_log SET case_id=$1 WHERE tenant=$2 AND requester_email=$3 AND case_id IS NULL`,
		caseID, tenant, email)

	wfID := fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), caseNumber)
	if _, werr := s.tc.ExecuteWorkflow(r.Context(), temporalclient.StartWorkflowOptions{
		ID: wfID, TaskQueue: "idre-ahca",
	}, "AhcaDisputeWorkflow", map[string]any{
		"tenant": tenant, "case_id": caseID, "case_number": caseNumber,
	}); werr != nil {
		return caseID, caseNumber, werr
	}

	// BULK INVOICING ARRANGEMENT: an approved bulk filer's dispute is opened
	// by the acceptance itself (Date Received starts now); the initial review
	// fee becomes an OPEN invoice settled under the arrangement. The upload
	// link is issued because the bulk acceptance — not a card payment — is
	// what opens this dispute.
	if deferFee && cfg.Fees.InitialFeeCents > 0 {
		if _, ierr := s.db.Exec(r.Context(), `
			INSERT INTO public.invoices (tenant, case_id, invoice_no, party, kind, amount_cents, due_date)
			VALUES ($1,$2,$3,$4,'INITIAL_FEE',$5, (now() + make_interval(days => 30))::date)
			ON CONFLICT (tenant, case_id, party, kind) DO NOTHING`,
			tenant, caseID, caseNumber, filingPartyType, cfg.Fees.InitialFeeCents); ierr != nil {
			return caseID, caseNumber, ierr
		}
		s.logAudit(r.Context(), tenant, caseID, "INTAKE_FEE_DEFERRED", map[string]any{
			"case_number": caseNumber, "fee_cents": cfg.Fees.InitialFeeCents,
			"basis": "bulk invoicing arrangement", "org": org,
		})
		s.sendIntakeLinks(r, tenant, caseID, caseNumber, email)
		s.notify(r, tenant, "*", "AHCA_INTAKE",
			fmt.Sprintf("Bulk-accepted intake (fee invoiced): %s (%s)", org, email), "#/cases/"+caseID)
		return caseID, caseNumber, nil
	}

	// PAYMENT-GATED INTAKE: when the program charges an initial review fee,
	// the case is created in AWAITING_PAYMENT and the party receives ONLY a
	// payment link. The case number, filing instructions, and the secure
	// upload link are released exclusively by activatePaidIntake once the fee
	// settles (Stripe webhook or offline settlement) — and the share-upload
	// endpoint independently refuses AWAITING_PAYMENT cases, so documents
	// cannot reach the docket before payment by any path.
	if cfg.Fees.InitialFeeCents > 0 {
		checkoutURL, cerr := s.issueInitialFeeCheckout(r, tenant, caseID, caseNumber, cfg.Fees.InitialFeeCents)
		if cerr == nil {
			if _, uerr := s.db.Exec(r.Context(), fmt.Sprintf(
				`UPDATE tenant_%s.cases SET status='AWAITING_PAYMENT', updated_at=now() WHERE id=$1`,
				sanitizeTenant(tenant)), caseID); uerr == nil {
				s.logAudit(r.Context(), tenant, caseID, "INTAKE_AWAITING_PAYMENT", map[string]any{
					"case_number": caseNumber, "fee_cents": cfg.Fees.InitialFeeCents,
				})
				body := fmt.Sprintf(
					"Thank you for contacting us regarding the claims dispute resolution program.\n\n"+
						"One step remains before your dispute is opened: payment of the initial review fee ($%d.%02d).\n\n"+
						"Pay securely here: %s\n\n"+
						"Once your payment is confirmed you will receive your case number, the filing "+
						"instructions and packet, and your secure document-upload link. Documents cannot "+
						"be accepted before payment.",
					cfg.Fees.InitialFeeCents/100, cfg.Fees.InitialFeeCents%100, checkoutURL)
				if merr := s.sendMail([]string{email}, nil, "Claims dispute — payment required to open your case", body); merr == nil {
					s.logCorrespondence(r, tenant, caseID, "OUT", "payment_required", "Claims dispute — payment required to open your case", body, []string{email}, nil, "system:ahca-intake")
				} else {
					s.logCorrespondence(r, tenant, caseID, "OUT", "payment_required", "Claims dispute — payment required to open your case", body, []string{email}, nil, "system:ahca-intake", merr.Error())
					s.logActivity(r.Context(), tenant, caseID, "EMAIL_DELIVERY_FAILED",
						fmt.Sprintf("Payment-required email SMTP delivery failed: %s", merr))
				}
				s.notify(r, tenant, "*", "AHCA_INTAKE",
					fmt.Sprintf("Intake awaiting fee payment: %s (%s)", org, email), "#/cases/"+caseID)
				return caseID, caseNumber, nil
			}
		} else {
			s.logActivity(r.Context(), tenant, caseID, "CHECKOUT_FAILED",
				fmt.Sprintf("Initial fee checkout session could not be created: %s", cerr))
		}
	}

	// No initial fee configured (or Stripe unavailable for this deployment) —
	// the intake completes immediately, links included, exactly as before.
	s.sendIntakeLinks(r, tenant, caseID, caseNumber, email)

	s.notify(r, tenant, "*", "AHCA_INTAKE",
		fmt.Sprintf("New filing instructions request: %s (%s)", org, email), "#/cases/"+caseID)
	return caseID, caseNumber, nil
}

// activatePaidIntake releases a payment-gated intake: the ONLY producer of
// the case number, filing instructions, and upload link for an
// AWAITING_PAYMENT case. Idempotent — the status guard makes a duplicate
// webhook replay a no-op. Called from the Stripe webhook and from
// settleInvoice's offline PAY path (check/ACH).
func (s *server) activatePaidIntake(r *http.Request, tenant, caseID string) {
	tbl := sanitizeTenant(tenant)
	res, err := s.db.Exec(r.Context(), fmt.Sprintf(
		`UPDATE tenant_%s.cases SET status='PENDING_INTAKE', updated_at=now()
		 WHERE id=$1 AND status='AWAITING_PAYMENT'`, tbl), caseID)
	if err != nil || res.RowsAffected() == 0 {
		return // not payment-gated, or already activated
	}
	var caseNumber string
	var details []byte
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT case_number, coalesce(details,'{}'::jsonb) FROM tenant_%s.cases WHERE id=$1`, tbl),
		caseID).Scan(&caseNumber, &details); err != nil {
		return
	}
	var d map[string]any
	_ = json.Unmarshal(details, &d)
	email, _ := d["requester_email"].(string)
	s.logAudit(r.Context(), tenant, caseID, "INTAKE_ACTIVATED", map[string]any{
		"case_number": caseNumber, "trigger": "fee payment settled",
	})
	s.logActivity(r.Context(), tenant, caseID, "INTAKE_ACTIVATED",
		"Initial review fee paid — case number and secure upload link released to the filing party")
	if email != "" {
		s.sendIntakeLinks(r, tenant, caseID, caseNumber, email)
	}
	s.notify(r, tenant, "*", "AHCA_INTAKE",
		fmt.Sprintf("Fee paid — intake activated: %s", caseNumber), "#/cases/"+caseID)
}

// sendIntakeLinks mints the filing-instructions download link and the
// upload-only packet link and emails them (with the case number) via the
// submission_instructions template. Shared by the no-fee immediate path and
// by activatePaidIntake's payment-gated release.
func (s *server) sendIntakeLinks(r *http.Request, tenant, caseID, caseNumber, email string) {
	downloadToken := s.mintShareLink(r, tenant, caseID, "download", "", 30)
	uploadToken := s.mintShareLink(r, tenant, caseID, "upload", "", 30)

	// submission_instructions has no qa_role in config (sends immediately,
	// per draftCorrespondence's own semantics for a template with none) --
	// there's no account/contact to resolve "requester" against yet, so the
	// email just submitted is the recipient, directly.
	var tpl *CorrTemplate
	for _, t := range s.corrTemplates(r, tenant) {
		if t.Key == "submission_instructions" {
			cp := t
			tpl = &cp
			break
		}
	}
	if tpl != nil {
		subject := s.renderTemplate(r, tenant, caseID, tpl.Subject)
		body := fmt.Sprintf(
			"Thank you for contacting us regarding the claims dispute resolution program.\n\n"+
				"Case number: %s\n\n"+
				"Download the filing instructions and packet: https://idre.newfire.app/api/share/%s\n"+
				"Upload your completed filing packet: https://idre.newfire.app/api/share/%s",
			caseNumber, downloadToken, uploadToken,
		)
		if merr := s.sendMail([]string{email}, nil, subject, body); merr == nil {
			s.logCorrespondence(r, tenant, caseID, "OUT", tpl.Key, subject, body, []string{email}, nil, "system:ahca-intake")
		} else {
			s.logCorrespondence(r, tenant, caseID, "OUT", tpl.Key, subject, body, []string{email}, nil, "system:ahca-intake", merr.Error())
			s.logActivity(r.Context(), tenant, caseID, "EMAIL_DELIVERY_FAILED",
				fmt.Sprintf("Submission instructions SMTP delivery failed: %s", merr))
		}
	}
}

// mintShareLink is createShareLink's logic without the HTTP/RBAC wrapper,
// for server-initiated links (the public intake endpoint has no staff
// principal to gate against). kind/objectKey/daysTTL match createShareLink's
// own defaults and validation.
func (s *server) mintShareLink(r *http.Request, tenant, caseID, kind, objectKey string, daysTTL int) string {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.share_links (token, tenant, case_id, kind, object_key, expires_at, max_uses, created_by)
		VALUES ($1,$2,$3,$4, nullif($5,''), now() + make_interval(days => $6), 1, 'system:ahca-intake')`,
		token, tenant, caseID, kind, objectKey, daysTTL)
	return token
}

// issueInitialFeeCheckout is issueInvoice + createCheckout's logic without
// the HTTP/RBAC wrapper (same reason as mintShareLink: no staff principal
// exists at public intake time). Mirrors both functions' SQL/Stripe calls
// exactly so the invoice this produces is indistinguishable from one a
// staffer issued through the ordinary endpoint.
func (s *server) issueInitialFeeCheckout(r *http.Request, tenant, caseID, caseNumber string, amountCents int64) (string, error) {
	var invID string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.invoices (tenant, case_id, invoice_no, party, kind, amount_cents, due_date)
		VALUES ($1,$2,$3,'PROVIDER','INITIAL_FEE',$4, (now() + make_interval(days => 30))::date)
		ON CONFLICT (tenant, case_id, party, kind) DO UPDATE SET amount_cents=EXCLUDED.amount_cents, status='OPEN'
		RETURNING id`, tenant, caseID, caseNumber, amountCents).Scan(&invID); err != nil {
		return "", err
	}
	s.finEvent(r, tenant, caseID, invID, "INVOICE_ISSUED", "NONE", amountCents, "PROVIDER", caseNumber, "system:ahca-intake")

	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("success_url", s.cfg.PortalBaseURL+"/#/cases/"+caseID+"?paid=1")
	form.Set("cancel_url", s.cfg.PortalBaseURL+"/#/cases/"+caseID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", "usd")
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(amountCents, 10))
	form.Set("line_items[0][price_data][product_data][name]",
		fmt.Sprintf("IDRE initial review fee — invoice %s (PROVIDER)", caseNumber))
	form.Set("metadata[invoice_id]", invID)
	form.Set("metadata[tenant]", tenant)
	form.Set("metadata[case_id]", caseID)
	sess, err := s.stripePost("/v1/checkout/sessions", form)
	if err != nil {
		return "", err
	}
	sessID, _ := sess["id"].(string)
	checkoutURL, _ := sess["url"].(string)
	raw, _ := json.Marshal(sess)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.payments (tenant, case_id, invoice_id, session_id, amount_cents, raw)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (session_id) DO UPDATE SET updated_at=now()`,
		tenant, caseID, invID, sessID, amountCents, raw)
	s.finEvent(r, tenant, caseID, invID, "PAYMENT_INITIATED", "NONE", amountCents, "PROVIDER", sessID, "system:ahca-intake")
	return checkoutURL, nil
}
