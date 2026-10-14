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
	"net/http"
	"strings"
)

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

// jsonSafe keeps an upstream error message from breaking a hand-built JSON body.
func jsonSafe(s string) string {
	return strings.NewReplacer(`"`, `'`, `\`, `/`, "\n", " ").Replace(s)
}

// emailBatchFiled tells the TPA, once per batch per filing round, which of
// its disputes were filed and where to pay for them -- instead of one
// payment email per dispute.
func (s *server) emailBatchFiled(ctx context.Context, tenant, batchID string, caseNumbers []string) {
	var ref, email, name, tpaID string
	if err := s.db.QueryRow(ctx, `
		SELECT b.batch_ref, coalesce(t.contact_email,''), t.name, t.id::text FROM public.tpa_dispute_batches b
		JOIN public.tpas t ON t.id=b.tpa_id WHERE b.tenant=$1 AND b.id::text=$2`, tenant, batchID).Scan(&ref, &email, &name, &tpaID); err != nil {
		return
	}
	fees, _ := s.batchUnpaidFees(ctx, tenant, batchID)
	var total int64
	for _, f := range fees {
		total += f.amount
	}
	notice := fmt.Sprintf("Bulk filing %s: %d dispute%s filed", ref, len(caseNumbers), map[bool]string{true: "", false: "s"}[len(caseNumbers) == 1])
	if len(fees) > 0 {
		notice += fmt.Sprintf(" — $%d.%02d in fees to pay under Disputes", total/100, total%100)
	}
	s.notifyOrg(ctx, tenant, "tpa", tpaID, "", "BATCH_FILED", notice, "#/tpa")
	if email == "" {
		return
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
