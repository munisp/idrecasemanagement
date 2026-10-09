package main

// Physical check intake: photo/scan -> vault-sealed storage -> doc-intel
// OCR/ICR extraction (MICR, courtesy/legal amounts, memo) -> invoice match ->
// clearing confirmation -> settlement.
//
// Status machine: RECEIVED -> PROCESSED -> MATCHED | REVIEW -> CLEARED | REJECTED.
// Funds are NEVER treated as settled from an image: extraction links the check
// to an invoice (payment PENDING_CLEARING); only the clearing confirmation
// (staff or bank/lockbox webhook) marks the invoice PAID and posts TB legs.
// The image is evidence, not money.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// uploadCheck accepts a check photo/scan (mobile deposit style). Sealed via
// the vault and stored in MinIO like case documents; a check.uploaded event
// puts it on the doc-intel queue for extraction.
func (s *server) uploadCheck(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "CASE_MANAGER", "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires CASE_MANAGER, PM, FINANCE, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		http.Error(w, `{"error":"multipart form required"}`, http.StatusBadRequest)
		return
	}
	f, hdr, err := r.FormFile("check")
	if err != nil {
		http.Error(w, `{"error":"check file field required"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	img, err := io.ReadAll(io.LimitReader(f, 16<<20))
	if err != nil || len(img) < 1024 {
		http.Error(w, `{"error":"empty or unreadable image"}`, http.StatusBadRequest)
		return
	}
	ct := hdr.Header.Get("Content-Type")
	switch ct {
	case "image/jpeg", "image/png", "image/tiff", "image/webp":
	default:
		http.Error(w, `{"error":"image/jpeg, png, tiff or webp only"}`, http.StatusUnsupportedMediaType)
		return
	}

	checkID := newUUID()
	key := fmt.Sprintf("checks/%s/%s.bin", tenant, checkID)
	sealed, err := s.vaultSealDoc(r, tenant, key, img)
	if err != nil {
		http.Error(w, `{"error":"vault seal failed"}`, http.StatusBadGateway)
		return
	}
	if _, err = s.docs.mc.PutObject(r.Context(), docBucket, key,
		bytes.NewReader(sealed), int64(len(sealed)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		http.Error(w, `{"error":"storage failed"}`, http.StatusBadGateway)
		return
	}
	_, err = s.db.Exec(r.Context(),
		`INSERT INTO public.checks (id, tenant, status, object_key, content_type)
		 VALUES ($1,$2,'RECEIVED',$3,$4)`, checkID, tenant, key, ct)
	if err != nil {
		http.Error(w, `{"error":"persist failed"}`, http.StatusInternalServerError)
		return
	}
	s.publish(r.Context(), tenant, "documents", map[string]any{
		"type": "check.uploaded", "tenant": tenant, "check_id": checkID,
		"object_key": key, "content_type": ct,
	})
	s.logActivity(r.Context(), tenant, "", "CHECK_RECEIVED", "Check image received for extraction ("+checkID+")")
	s.logAudit(r.Context(), tenant, "", "CHECK_UPLOADED", map[string]any{
		"by": r.Context().Value(ctxPrincipal{}).(principal).Subject, "check_id": checkID,
		"content_type": ct, "bytes": len(img),
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"check_id": checkID, "status": "RECEIVED"})
}

// checkResult ingests doc-intel's extraction (worker-token auth) and matches
// the check to an open invoice: memo carrying the invoice number wins, else
// exact amount match on a single open invoice. Any amount mismatch or low
// confidence routes to REVIEW — extraction never auto-settles money.
func (s *server) checkResult(w http.ResponseWriter, r *http.Request) {
	// Route comment says "worker-token: doc-intel OCR" but nothing ever
	// enforced that -- any authenticated tenant member could POST fabricated
	// extraction results (routing/account numbers, amounts, confidence) for
	// any check, bypassing the OCR pipeline entirely. Only the pipeline's own
	// WORKER_TOKEN identity may report results.
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, serviceRole) {
		http.Error(w, `{"error":"forbidden: check results may only be reported by the extraction worker"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	checkID := chi.URLParam(r, "checkId")
	var in struct {
		RoutingNumber    string         `json:"routing_number"`
		AccountNumber    string         `json:"account_number"`
		CheckNumber      string         `json:"check_number"`
		AmountCents      *int64         `json:"amount_cents"`
		LegalAmountCents *int64         `json:"legal_amount_cents"`
		AmountMismatch   bool           `json:"amount_mismatch"`
		CheckDate        string         `json:"check_date"`
		PayerName        string         `json:"payer_name"`
		Memo             string         `json:"memo"`
		Confidence       string         `json:"confidence"`
		Detail           map[string]any `json:"detail"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	detail, _ := json.Marshal(in.Detail)
	_, err := s.db.Exec(r.Context(), `
		UPDATE public.checks SET status='PROCESSED', routing_number=$3, account_number=$4,
		       check_number=$5, amount_cents=$6, legal_amount_cents=$7, amount_mismatch=$8,
		       check_date=NULLIF($9,'')::date, payer_name=$10, memo=$11, confidence=$12,
		       extraction=$13, updated_at=now()
		WHERE id=$1 AND tenant=$2 AND status='RECEIVED'`,
		checkID, tenant, in.RoutingNumber, in.AccountNumber, in.CheckNumber,
		in.AmountCents, in.LegalAmountCents, in.AmountMismatch,
		in.CheckDate, in.PayerName, in.Memo, in.Confidence, detail)
	if err != nil {
		http.Error(w, `{"error":"not found or already processed"}`, http.StatusConflict)
		return
	}

	// --- match --------------------------------------------------------------
	var invID, caseID, matchedBy string
	matchable := !in.AmountMismatch && in.AmountCents != nil && in.Confidence != "low"
	if matchable && in.Memo != "" {
		// memo like "Inv IDRE-2026-000123" / raw invoice number
		err = s.db.QueryRow(r.Context(), `
			UPDATE public.checks c SET invoice_id=i.id, case_id=i.case_id, matched_by='memo_invoice_no'
			FROM public.invoices i
			WHERE c.id=$1 AND i.tenant=$2 AND i.status='OPEN' AND i.amount_cents=$3
			  AND (i.invoice_no IS NOT NULL AND $4 ILIKE '%'||i.invoice_no||'%')
			RETURNING c.invoice_id, c.case_id`,
			checkID, tenant, *in.AmountCents, in.Memo).Scan(&invID, &caseID)
		if err == nil {
			matchedBy = "memo_invoice_no"
		}
	}
	if matchable && invID == "" {
		err = s.db.QueryRow(r.Context(), `
			UPDATE public.checks c SET invoice_id=i.id, case_id=i.case_id, matched_by='amount'
			FROM public.invoices i
			WHERE c.id=$1 AND i.tenant=$2 AND i.status='OPEN' AND i.amount_cents=$3
			  AND (SELECT count(*) FROM public.invoices
			        WHERE tenant=$2 AND status='OPEN' AND amount_cents=$3) = 1
			RETURNING c.invoice_id, c.case_id`,
			checkID, tenant, *in.AmountCents).Scan(&invID, &caseID)
		if err == nil {
			matchedBy = "amount"
		}
	}

	status := "REVIEW"
	if invID != "" {
		status = "MATCHED"
		// payment row awaiting clearing — the image is evidence, not money
		s.db.Exec(r.Context(), `
			INSERT INTO public.payments (tenant, case_id, invoice_id, provider, session_id,
			                             amount_cents, status, payer_name)
			VALUES ($1,$2,$3,'check',$4,$5,'PENDING_CLEARING',$6)`,
			tenant, caseID, invID, "chk:"+checkID, *in.AmountCents, in.PayerName)
		s.finEvent(r, tenant, caseID, invID, "PAYMENT_INITIATED", "NONE", *in.AmountCents, "",
			"chk:"+checkID, "check-intake")
	}
	s.db.Exec(r.Context(),
		`UPDATE public.checks SET status=$3, updated_at=now() WHERE id=$1 AND tenant=$2`,
		checkID, tenant, status)
	s.logActivity(r.Context(), tenant, caseID, "CHECK_"+status,
		fmt.Sprintf("Check %s extracted (conf=%s, match=%s)", checkID, in.Confidence, orDash(matchedBy)))
	// Account/routing numbers deliberately left out of the audit payload.
	s.logAudit(r.Context(), tenant, caseID, "CHECK_PROCESSED", map[string]any{
		"by": r.Context().Value(ctxPrincipal{}).(principal).Subject, "check_id": checkID,
		"status": status, "matched_by": matchedBy, "invoice_id": invID,
		"amount_cents": in.AmountCents, "confidence": in.Confidence,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": status, "match": matchedBy, "invoice_id": invID})
}

// clearCheck records funds cleared (staff action or bank/lockbox webhook):
// invoice PAID + payment PAID + TB clearing→escrow leg + rules fire. This is
// the ONLY path that turns a check image into settled money.
func (s *server) clearCheck(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	// Money-moving, same separation-of-duties floor as settleInvoice: FINANCE/
	// PM/admins, not CASE_MANAGER -- the person who manages the case is not
	// the one who gets to also confirm its money cleared.
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "PM", "FINANCE", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden: requires PM, FINANCE, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	checkID := chi.URLParam(r, "checkId")
	var in struct {
		RemittanceRef string `json:"remittance_ref"` // bank deposit/lockbox ref
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	code, msg := s.settleClearedCheck(r, tenant, checkID, in.RemittanceRef,
		r.Context().Value(ctxPrincipal{}).(principal).Subject)
	if code != http.StatusOK {
		http.Error(w, msg, code)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "CLEARED"})
}

// settleClearedCheck is the shared settlement core for staff clearCheck and
// the bank/lockbox webhook — one path turns a matched check into money,
// whoever confirms it. Returns (httpStatus, errorJSON).
func (s *server) settleClearedCheck(r *http.Request, tenant, checkID, remittanceRef, actor string) (int, string) {
	var invID, caseID string
	var amount int64
	err := s.db.QueryRow(r.Context(), `
		UPDATE public.checks SET status='CLEARED', cleared_at=now(), updated_at=now()
		WHERE id=$1 AND tenant=$2 AND status='MATCHED'
		RETURNING invoice_id, case_id, amount_cents`, checkID, tenant).
		Scan(&invID, &caseID, &amount)
	if err != nil {
		return http.StatusConflict, `{"error":"check not in MATCHED state"}`
	}
	if _, err = s.db.Exec(r.Context(), `
		UPDATE public.invoices SET status='PAID', paid_at=now()::date, remittance_ref=$3
		WHERE tenant=$1 AND id=$2 AND status='OPEN'`, tenant, invID,
		orDash(remittanceRef)); err != nil {
		return http.StatusInternalServerError, `{"error":"invoice settle failed"}`
	}
	s.db.Exec(r.Context(), `
		UPDATE public.payments SET status='PAID', updated_at=now()
		WHERE session_id=$1 AND provider='check'`, "chk:"+checkID)

	// Ledger leg: money is real now (clearing -> escrow), deterministic id.
	s.postPaymentLedger(tenant, caseID, "chk:"+checkID, "", uint64(amount), false)

	s.finEvent(r, tenant, caseID, invID, "PAYMENT_PAID", "IN", amount, "",
		orDash(remittanceRef), actor)
	s.maybeAdvanceStatus(r, tenant, caseID) // all fees PAID => CLOSED_PAID
	s.autoChecklist(r, tenant, caseID)
	s.logActivity(r.Context(), tenant, caseID, "CHECK_CLEARED",
		fmt.Sprintf("Check %s cleared — invoice %s paid ($%d.%02d)", checkID, invID, amount/100, amount%100))
	s.fireEventRules(r, tenant, "invoice.settled", map[string]any{
		"case_id": caseID, "invoice_id": invID, "amount_cents": amount,
		"method": "check", "remittance_ref": remittanceRef, "tenant": tenant,
	})
	s.logAudit(r.Context(), tenant, caseID, "CHECK_CLEARED", map[string]any{
		"by": actor, "check_id": checkID,
		"invoice_id": invID, "amount_cents": amount, "remittance_ref": remittanceRef,
	})
	return http.StatusOK, ""
}

// listChecks: review queue + status filtering for staff.
func (s *server) listChecks(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	// Check images carry real bank routing/account numbers -- financial
	// data, financial-read gate, not just case staff.
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, financialReadRoles...) {
		http.Error(w, `{"error":"forbidden: requires a financial/management role"}`, http.StatusForbidden)
		return
	}
	status := r.URL.Query().Get("status")
	limit, offset := pageParams(r, 50, 500)
	var total int
	if err := s.db.QueryRow(r.Context(),
		`SELECT count(*) FROM public.checks WHERE tenant=$1 AND ($2='' OR status=$2)`,
		tenant, status).Scan(&total); err != nil {
		http.Error(w, `{"error":"query"}`, http.StatusInternalServerError)
		return
	}
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT id, status, check_number, amount_cents AS courtesy_amount_cents, legal_amount_cents, memo,
		       COALESCE(to_char(check_date,'YYYY-MM-DD'),'') AS check_date,
		       routing_number, account_number, confidence, amount_mismatch,
		       invoice_id, case_id, payer_name, created_at
		FROM public.checks WHERE tenant=$1 AND ($2='' OR status=$2)
		ORDER BY created_at DESC, id DESC LIMIT %d OFFSET %d`, limit, offset), tenant, status)
	if err != nil {
		http.Error(w, `{"error":"query"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": rows,
		"total": total, "next_offset": nextOffset(offset, limit, total)})
}

// checkImage streams the vault-unsealed check scan for the portal preview.
// Same financial-read gate as listChecks — the image carries full MICR data.
func (s *server) checkImage(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, financialReadRoles...) {
		http.Error(w, `{"error":"forbidden: requires a financial/management role"}`, http.StatusForbidden)
		return
	}
	checkID := chi.URLParam(r, "checkId")
	var key, ct string
	if err := s.db.QueryRow(r.Context(),
		`SELECT object_key, content_type FROM public.checks WHERE id=$1 AND tenant=$2`,
		checkID, tenant).Scan(&key, &ct); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	obj, err := s.docs.mc.GetObject(r.Context(), docBucket, key, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, `{"error":"storage"}`, http.StatusBadGateway)
		return
	}
	defer obj.Close()
	sealed, err := io.ReadAll(obj)
	if err != nil {
		http.Error(w, `{"error":"storage"}`, http.StatusBadGateway)
		return
	}
	img, err := s.vaultOpenDoc(r, tenant, key, sealed)
	if err != nil {
		http.Error(w, `{"error":"unseal failed"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.Write(img)
}
