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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	temporalclient "go.temporal.io/sdk/client"
	"time"
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
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20) // the filing packet form's positions can be long
	var in struct {
		Tenant              string        `json:"tenant"`
		Email               string        `json:"email"`
		Contact             string        `json:"contact_name"`
		Org                 string        `json:"org"`
		FilingPartyType     string        `json:"filing_party_type"` // PROVIDER|HEALTH_PLAN (default PROVIDER)
		DisputedAmountCents int64         `json:"disputed_amount_cents"`
		Packet              *filingPacket `json:"packet"` // the filing packet form, when the filer completed it
		// The side the dispute is against and where its notices go -- the health plan when a provider files. The
		// PDF packet has no plan email, so the filing page asks for it; the packet form's values fill blanks.
		OtherPartyName  string `json:"other_party_name"`
		OtherPartyEmail string `json:"other_party_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil ||
		in.Tenant == "" || in.Email == "" || !strings.Contains(in.Email, "@") {
		http.Error(w, `{"error":"tenant and a valid email are required"}`, http.StatusBadRequest)
		return
	}
	if packetRejected(w, in.Packet) {
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
	rsp := in.Packet.otherParty(respondent{Side: otherSide(fpt), Name: in.OtherPartyName, Email: in.OtherPartyEmail})
	if msg := rsp.check(); msg != "" {
		http.Error(w, `{"error":"`+jsonSafe(msg)+`"}`, http.StatusBadRequest)
		return
	}
	if msg := emailReachMsg(r.Context(), "the other party's email", rsp.Email); msg != "" {
		http.Error(w, `{"error":"`+jsonSafe(msg)+`"}`, http.StatusBadRequest)
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
	caseID, caseNumber, err := s.startAhcaCase(r, tenant, cfg, in.Email, in.Contact, in.Org, fpt, in.DisputedAmountCents, "")
	if err != nil {
		if isUniqueViolation(err) {
			http.Error(w, `{"error":"case_number already exists — retry"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"case creation failed"}`, http.StatusBadGateway)
		return
	}
	s.fileWithPacket(r, tenant, caseID, in.Packet, "the filer ("+in.Email+")")
	s.recordRespondent(r, tenant, caseID, rsp)
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
func (s *server) startAhcaCase(r *http.Request, tenant string, cfg *ProgramConfig, email, contactName, org, filingPartyType string, disputedAmountCents int64, serviceLine string) (caseID, caseNumber string, err error) {
	return s.startAhcaCaseOpt(r, tenant, cfg, email, contactName, org, filingPartyType, disputedAmountCents, serviceLine, false)
}

// startAhcaCaseOpt adds the bulk-invoicing-arrangement switch: when deferFee
// is true (filer org is on config.fees.invoiced_filer_orgs and the intake came
// through the bulk path), the dispute opens immediately on acceptance and the
// initial fee is raised as an OPEN invoice to be collected later — the second
// opening path in the AHCA 2026 "Payment before submission" rules. Every
// other filer still pays before the dispute exists.
func (s *server) startAhcaCaseOpt(r *http.Request, tenant string, cfg *ProgramConfig, email, contactName, org, filingPartyType string, disputedAmountCents int64, serviceLine string, deferFee bool) (caseID, caseNumber string, err error) {
	return s.startAhcaCaseFull(r, tenant, cfg, email, contactName, org, filingPartyType, disputedAmountCents, serviceLine, deferFee, false)
}

// startAhcaCaseFull adds batchPay: the fee invoice is raised and the case
// waits for payment, but no checkout or payment email of its own -- the
// filer pays for a whole bulk batch with one checkout (tpa_bulk_payments.go).
func (s *server) startAhcaCaseFull(r *http.Request, tenant string, cfg *ProgramConfig, email, contactName, org, filingPartyType string, disputedAmountCents int64, serviceLine string, deferFee, batchPay bool) (caseID, caseNumber string, err error) {
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
		INSERT INTO tenant_%s.cases (case_number, status, details, disputed_amount_cents, service_line)
		VALUES ($1, 'PENDING_INTAKE', $2, nullif($3,0), nullif($4,'')) RETURNING id`, sanitizeTenant(tenant)),
		caseNumber, detailsJSON, disputedAmountCents, serviceLine).Scan(&caseID); err != nil {
		return "", "", err
	}
	s.logAudit(r.Context(), tenant, caseID, "CASE_INTAKE_OPENED", map[string]any{
		"case_number": caseNumber, "requester_email": email, "requester_org": org,
		"disputed_amount_cents": disputedAmountCents,
	})
	s.recordTransition(r.Context(), tenant, caseID, axisLifecycle, "", "PENDING_INTAKE", "", "intake")

	s.ensureChecklist(r, tenant, caseID)
	// The next Reviewer in turn, on every intake path (autoassign.go) -- run on the way out, once the case is either open
	// or waiting on its fee: an unpaid one is skipped here and assigned when the payment opens it (activatePaidIntakeFor).
	defer s.autoAssignNewCase(r, tenant, caseID, caseNumber)

	// Program escalation trigger (G6): auto-route over-threshold disputes
	// to F&WA screening. initiateCase (the staff-only endpoint) already
	// calls this, but the public AHCA intake form -- the actual front door
	// for real disputes -- had its own INSERT here and never called it at
	// all: a $1M+ filing submitted through the real intake form never
	// escalated. Fires before the branches below so it covers every intake
	// path (bulk-invoiced, payment-gated, immediate) regardless of which
	// one returns early.
	s.escalationTrigger(r, tenant, caseID, disputedAmountCents)

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
	if cfg.Fees.InitialFeeCents > 0 && batchPay {
		if _, ierr := s.issueInitialFeeInvoice(r, tenant, caseID, caseNumber, cfg.Fees.InitialFeeCents); ierr == nil {
			if res, uerr := s.db.Exec(r.Context(), fmt.Sprintf(
				`UPDATE tenant_%s.cases SET status='AWAITING_PAYMENT', program_dates = coalesce(program_dates,'{}'::jsonb) - 'received_at', updated_at=now() WHERE id=$1`,
				sanitizeTenant(tenant)), caseID); uerr == nil {
				if res.RowsAffected() > 0 {
					s.recordTransition(r.Context(), tenant, caseID, axisLifecycle, "PENDING_INTAKE", "AWAITING_PAYMENT", "", "intake")
				}
				s.logAudit(r.Context(), tenant, caseID, "INTAKE_AWAITING_PAYMENT", map[string]any{
					"case_number": caseNumber, "fee_cents": cfg.Fees.InitialFeeCents, "paid_with": "bulk batch checkout",
				})
				return caseID, caseNumber, nil
			}
		}
	}
	if cfg.Fees.InitialFeeCents > 0 && !batchPay {
		cardURL, bankURL, cardFee, cerr := s.issueInitialFeeCheckout(r, tenant, caseID, caseNumber, cfg.Fees.InitialFeeCents)
		if cerr == nil {
			if res, uerr := s.db.Exec(r.Context(), fmt.Sprintf(
				`UPDATE tenant_%s.cases SET status='AWAITING_PAYMENT', program_dates = coalesce(program_dates,'{}'::jsonb) - 'received_at', updated_at=now() WHERE id=$1`,
				sanitizeTenant(tenant)), caseID); uerr == nil {
				if res.RowsAffected() > 0 {
					s.recordTransition(r.Context(), tenant, caseID, axisLifecycle, "PENDING_INTAKE", "AWAITING_PAYMENT", "", "intake")
				}
				s.logAudit(r.Context(), tenant, caseID, "INTAKE_AWAITING_PAYMENT", map[string]any{
					"case_number": caseNumber, "fee_cents": cfg.Fees.InitialFeeCents,
				})
				body := "Thank you for contacting us regarding the claims dispute resolution program.\n\n" +
					"One step remains before your dispute is opened: payment of the initial review fee (" + usd(cfg.Fees.InitialFeeCents) + ").\n\n" +
					payLinksText(cfg.Fees.InitialFeeCents, cardFee, cardURL, bankURL) + "\n\n" +
					"Once your payment is confirmed you will receive your case number, the filing " +
					"instructions and packet, and your secure document-upload link. Documents cannot " +
					"be accepted before payment."
				if merr := s.sendMail([]string{email}, nil, "Claims dispute — payment required to open your case", body); merr == nil {
					s.logCorrespondence(r, tenant, caseID, "OUT", "payment_required", "Claims dispute — payment required to open your case", body, []string{email}, nil, "system:ahca-intake")
				} else {
					s.logCorrespondence(r, tenant, caseID, "OUT", "payment_required", "Claims dispute — payment required to open your case", body, []string{email}, nil, "system:ahca-intake", merr.Error())
					typ := "EMAIL_DELIVERY_FAILED"
					if errors.Is(merr, errMailQueued) {
						typ = "EMAIL_QUEUED"
					}
					s.logActivity(r.Context(), tenant, caseID, typ, fmt.Sprintf("Payment-required email: %s", merr))
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
	s.activatePaidIntakeFor(r, tenant, caseID, true)
}

// activatePaidIntakeFor: tellFiler false when one payment opened many
// disputes, and the filer gets one notice for all of them instead.
func (s *server) activatePaidIntakeFor(r *http.Request, tenant, caseID string, tellFiler bool) {
	tbl := sanitizeTenant(tenant)
	res, err := s.db.Exec(r.Context(), fmt.Sprintf(
		`UPDATE tenant_%s.cases SET status='PENDING_INTAKE', updated_at=now()
		 WHERE id=$1 AND status='AWAITING_PAYMENT'`, tbl), caseID)
	if err != nil || res.RowsAffected() == 0 {
		return // not payment-gated, or already activated
	}
	s.recordTransition(r.Context(), tenant, caseID, axisLifecycle, "AWAITING_PAYMENT", "PENDING_INTAKE", "", "intake")
	// "Date Received" is the date the dispute was opened by payment -- not
	// the day someone asked how to file. The 60-day clock runs from it.
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(
		`UPDATE tenant_%s.cases SET program_dates = coalesce(program_dates,'{}'::jsonb) || jsonb_build_object('received_at', $2::text) WHERE id=$1`, tbl),
		caseID, time.Now().UTC().Format("2006-01-02"))
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
	s.autoAssignNewCase(r, tenant, caseID, caseNumber) // paid, so it now takes its turn in the rotation (autoassign.go)
	// The Reviewer is told the case now exists.
	s.notify(r, tenant, "role:CASE_MANAGER", "AHCA_INTAKE",
		fmt.Sprintf("Fee paid — case %s opened: confirm the filing packet", caseNumber), "#/cases/"+caseID)
	if tellFiler {
		s.tellFiler(r.Context(), tenant, caseID, "PAYMENT_RECEIVED", fmt.Sprintf("Fee received — %s is open", caseNumber))
	}
	s.tellRespondentOpened(r.Context(), tenant, caseID)
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
	tpl := s.effectiveTemplate(r, tenant, caseID, "submission_instructions")
	if tpl != nil {
		subject := s.renderTemplate(r, tenant, caseID, tpl.Subject)
		shareBase := strings.TrimRight(s.cfg.PortalBaseURL, "/")
		body := intakeLinksBody(s.renderTemplate(r, tenant, caseID, tpl.Body), caseNumber,
			shareBase+"/api/share/"+downloadToken, shareBase+"/api/share/"+uploadToken)
		if merr := s.sendMail([]string{email}, nil, subject, body); merr == nil {
			s.logCorrespondence(r, tenant, caseID, "OUT", tpl.Key, subject, body, []string{email}, nil, "system:ahca-intake")
			s.logCorrespondenceInquiry(r, tenant, caseID, email, fmt.Sprintf("%s: %s", tpl.Key, subject), "system:ahca-intake")
		} else {
			s.logCorrespondence(r, tenant, caseID, "OUT", tpl.Key, subject, body, []string{email}, nil, "system:ahca-intake", merr.Error())
			typ := "EMAIL_DELIVERY_FAILED"
			if errors.Is(merr, errMailQueued) {
				typ = "EMAIL_QUEUED"
			}
			s.logActivity(r.Context(), tenant, caseID, typ, fmt.Sprintf("Submission instructions email: %s", merr))
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
		INSERT INTO public.share_links (token, tenant, case_id, kind, object_key, expires_at, max_uses, max_files, created_by)
		VALUES ($1,$2,$3,$4, nullif($5,''), now() + make_interval(days => $6), $7, $7, 'system:ahca-intake')`,
		token, tenant, caseID, kind, objectKey, daysTTL, shareLinkUses(kind, 0))
	return token
}

// issueInitialFeeCheckout is issueInvoice + createCheckout's logic without
// the HTTP/RBAC wrapper (same reason as mintShareLink: no staff principal
// exists at public intake time). Mirrors both functions' SQL/Stripe calls
// exactly so the invoice this produces is indistinguishable from one a
// staffer issued through the ordinary endpoint.
// issueInitialFeeInvoice raises (or re-opens) the case's INITIAL_FEE invoice.
func (s *server) issueInitialFeeInvoice(r *http.Request, tenant, caseID, caseNumber string, amountCents int64) (string, error) {
	var invID string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.invoices (tenant, case_id, invoice_no, party, kind, amount_cents, due_date)
		VALUES ($1,$2,$3,'PROVIDER','INITIAL_FEE',$4, (now() + make_interval(days => 30))::date)
		ON CONFLICT (tenant, case_id, party, kind) DO UPDATE SET amount_cents=EXCLUDED.amount_cents, status='OPEN'
		RETURNING id`, tenant, caseID, caseNumber, amountCents).Scan(&invID); err != nil {
		return "", err
	}
	s.finEvent(r, tenant, caseID, invID, "INVOICE_ISSUED", "NONE", amountCents, "PROVIDER", caseNumber, "system:ahca-intake")
	return invID, nil
}

// The filer is offered both ways to pay (card_fee.go): a page for a card, which adds the card fee, and one for a
// bank account. It returns the two pages' links; the bank page's is "" when it could not be opened.
func (s *server) issueInitialFeeCheckout(r *http.Request, tenant, caseID, caseNumber string, amountCents int64) (cardURL, bankURL string, cardFee int64, err error) {
	invID, err := s.issueInitialFeeInvoice(r, tenant, caseID, caseNumber, amountCents)
	if err != nil {
		return "", "", 0, err
	}
	_, cardFee = s.cardFees(r.Context(), tenant, []string{invID})
	if cardURL, err = s.initialFeeSession(r, tenant, caseID, caseNumber, invID, amountCents, payByCard, cardFee); err != nil {
		return "", "", 0, err
	}
	if bankURL, err = s.initialFeeSession(r, tenant, caseID, caseNumber, invID, amountCents, payByBank, 0); err != nil {
		slog.Warn("bank payment page not opened", "case", caseNumber, "err", err)
		bankURL = ""
	}
	return cardURL, bankURL, cardFee, nil
}

// initialFeeSession opens one Stripe page for an initial fee, paid one way.
func (s *server) initialFeeSession(r *http.Request, tenant, caseID, caseNumber, invID string, amountCents int64, method string, cardFee int64) (string, error) {
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
	form.Set("payment_intent_data[metadata][invoice_id]", invID)
	form.Set("payment_intent_data[metadata][tenant]", tenant)
	if err := s.checkoutFor(form, method, cardFee, 1); err != nil {
		return "", err
	}
	sess, err := s.stripePost("/v1/checkout/sessions", form)
	if err != nil {
		return "", err
	}
	sessID, _ := sess["id"].(string)
	checkoutURL, _ := sess["url"].(string)
	raw, _ := json.Marshal(sess)
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.payments (tenant, case_id, invoice_id, session_id, amount_cents, raw, card_fee_cents)
		VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (session_id) DO UPDATE SET updated_at=now()`,
		tenant, caseID, invID, sessID, amountCents, raw, cardFee)
	s.finEvent(r, tenant, caseID, invID, "PAYMENT_INITIATED", "NONE", amountCents, "PROVIDER", sessID, "system:ahca-intake")
	return checkoutURL, nil
}

// intakeLinksBody is the email that opens a case for its filer: the
// template's own wording with its two links put in. Whatever the template
// says, the filer must end up with the case number and both links, so each
// one the wording leaves out is added underneath. A template with no wording
// at all gets the short standard text. Pure, so it is tested.
func intakeLinksBody(text, caseNumber, downloadURL, uploadURL string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "Thank you for contacting us regarding the claims dispute resolution program."
	}
	hasDownload := strings.Contains(text, "{download_link}")
	hasUpload := strings.Contains(text, "{share_link}")
	text = strings.ReplaceAll(text, "{download_link}", downloadURL)
	text = strings.ReplaceAll(text, "{share_link}", uploadURL)
	var extra []string
	if caseNumber != "" && !strings.Contains(text, caseNumber) {
		extra = append(extra, "Case number: "+caseNumber)
	}
	if !hasDownload {
		extra = append(extra, "Download the filing instructions and packet: "+downloadURL)
	}
	if !hasUpload {
		extra = append(extra, "Upload your completed filing packet: "+uploadURL)
	}
	if len(extra) > 0 {
		text += "\n\n" + strings.Join(extra, "\n")
	}
	return text
}
