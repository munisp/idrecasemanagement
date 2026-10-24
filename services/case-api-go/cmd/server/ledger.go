// ledger.go — opening ledger accounts on first use, and keeping one
// deployment's ledger ids apart from another's.
//
// Nothing used to create the accounts the transfers name. TigerBeetle refuses
// a transfer between accounts it has never seen (TransferDebitAccountNotFound),
// and every posting is fail-open with only a log line, so the FL ledger stayed
// empty while payments and invoices went through. postLedger now opens both
// accounts (idempotent: AccountExists is success) before each transfer.
//
// The TigerBeetle cluster is shared with other apps, and idre-prod on the dev
// cluster was copied from idre: the same tenants, cases and invoice ids. Every
// account and transfer id is a hash of those, so the two deployments would
// post into each other's accounts. LEDGER_NAMESPACE is mixed into every id;
// empty (idre) keeps the ids exactly as they were.
package main

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	tb_types "github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

var ledgerNamespace = envOr("LEDGER_NAMESPACE", "")

// ledgerKey prefixes a hashed id's input with the deployment's namespace.
func ledgerKey(s string) string {
	if ledgerNamespace == "" {
		return s
	}
	return ledgerNamespace + "|" + s
}

// ledgerTransferID is a deterministic transfer id for an idempotency key.
func ledgerTransferID(key string) tb_types.Uint128 {
	h := sha256.Sum256([]byte(ledgerKey(key)))
	var b [16]byte
	copy(b[:], h[:16])
	return tb_types.BytesToUint128(b)
}

// ledgerAcct names one account of a tenant's ledger.
type ledgerAcct struct {
	code  uint32
	party string
}

// openedAccounts remembers accounts this process has already opened, so a
// busy path doesn't ask TigerBeetle again on every posting.
var openedAccounts sync.Map

// openAccounts creates the accounts if they don't exist yet.
func (s *server) openAccounts(tenant string, accts ...ledgerAcct) error {
	var batch []tb_types.Account
	for _, a := range accts {
		id := acctID(tenant, a.code, a.party)
		if _, done := openedAccounts.Load(hex128(id)); done {
			continue
		}
		batch = append(batch, tb_types.Account{
			ID: id, Ledger: tenantLedgerID(tenant), Code: uint16(a.code),
		})
	}
	if len(batch) == 0 {
		return nil
	}
	res, err := s.tb.CreateAccounts(batch)
	if err != nil {
		return err
	}
	failed := map[uint32]bool{}
	for _, r := range res {
		if r.Result != tb_types.AccountOK && r.Result != tb_types.AccountExists {
			failed[r.Index] = true
			slog.Error("ledger account not opened", "tenant", tenant, "code", batch[r.Index].Code, "result", r.Result.String())
		}
	}
	for i, a := range batch {
		if !failed[uint32(i)] {
			openedAccounts.Store(hex128(a.ID), true)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d ledger account(s) not opened", len(failed))
	}
	return nil
}

// postLedger opens the debit and credit accounts and creates the transfer
// between them. Ledger and Code are filled in; t carries the rest. A
// post/void of a pending transfer leaves both accounts zero (TigerBeetle
// takes them from the pending transfer) and opens nothing.
func (s *server) postLedger(tenant string, debit, credit ledgerAcct, t tb_types.Transfer) ([]tb_types.TransferEventResult, error) {
	t.Ledger = tenantLedgerID(tenant)
	t.Code = ledgerCodeIDRE
	finishing := t.Flags&(tb_types.TransferFlags{PostPendingTransfer: true, VoidPendingTransfer: true}.ToUint16()) != 0
	if finishing {
		t.DebitAccountID, t.CreditAccountID = tb_types.Uint128{}, tb_types.Uint128{}
	} else {
		if err := s.openAccounts(tenant, debit, credit); err != nil {
			return nil, err
		}
		t.DebitAccountID = acctID(tenant, debit.code, debit.party)
		t.CreditAccountID = acctID(tenant, credit.code, credit.party)
	}
	return s.tb.CreateTransfers([]tb_types.Transfer{t})
}

// backfillLeg is one past money movement the ledger should hold.
type backfillLeg struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
	CaseID string `json:"case_id"`
	Amount int64  `json:"amount_cents"`
	Debit  uint32 `json:"debit_code"`
	Credit uint32 `json:"credit_code"`
	Result string `json:"result"`

	debit, credit ledgerAcct
	id, retryID   tb_types.Uint128
}

// backfillRetryID is the id a backfill posts under when the live id is
// burnt: TigerBeetle remembers an id whose transfer failed (here: the
// accounts didn't exist yet) and refuses it for ever (TransferIDAlreadyFailed).
// Deterministic, so running the backfill again finds it instead of posting
// twice; the live path never retries a past payment, so it can't double up.
func backfillRetryID(key string) tb_types.Uint128 { return ledgerTransferID(key + "|backfill") }

// ledgerBackfill replays money already recorded in the database into the
// ledger, with the same accounts and transfer ids the live paths use, so a
// replay can never count a payment twice (TransferExists). Dry run unless
// ?apply=true. Covered: Stripe payments and refunds, cleared checks. Manual
// fee transfers (POST /fees/transfer) leave no database record and can't be
// replayed; service invoices and payables are counted and reported.
func (s *server) ledgerBackfill(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	if s.tb == nil {
		http.Error(w, `{"error":"ledger unavailable"}`, http.StatusBadGateway)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	apply := r.URL.Query().Get("apply") == "true"
	var legs []backfillLeg
	add := func(source, ref, caseID string, amount int64, debit, credit ledgerAcct, id tb_types.Uint128, key string) {
		if amount <= 0 {
			return
		}
		legs = append(legs, backfillLeg{Source: source, Ref: ref, CaseID: caseID, Amount: amount,
			Debit: debit.code, Credit: credit.code, debit: debit, credit: credit, id: id, retryID: backfillRetryID(key)})
	}

	// Stripe: the webhook posts the invoice's amount and party when the
	// checkout completes, and the refunded amount when a charge is refunded.
	rows, err := s.db.Query(r.Context(), `
		SELECT coalesce(p.case_id::text,''), p.payment_intent, coalesce(i.party,''),
		       coalesce(i.amount_cents, p.amount_cents), p.status,
		       CASE WHEN p.status='REFUNDED' THEN coalesce((p.raw->>'amount_refunded')::bigint, p.amount_cents) ELSE 0 END,
		       EXISTS(SELECT 1 FROM public.refunds rf WHERE rf.payment_id=p.id AND rf.kind='FEE')
		FROM public.payments p
		LEFT JOIN public.invoices i ON i.id::text = p.invoice_id::text AND i.tenant = p.tenant
		WHERE p.tenant=$1 AND p.provider='stripe' AND p.status IN ('PAID','REFUNDED')
		  AND coalesce(p.payment_intent,'') <> ''
		ORDER BY p.created_at`, tenant)
	if err != nil {
		slog.Error("ledger backfill: payments query", "err", err)
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var caseID, pi, party, status string
		var amount, refunded int64
		var tracked bool
		if rows.Scan(&caseID, &pi, &party, &amount, &status, &refunded, &tracked) != nil {
			continue
		}
		d, c, id := paymentLeg(pi, party, false)
		add("stripe-payment", pi, caseID, amount, d, c, id, paymentLegKey(pi, false))
		if status == "REFUNDED" && !tracked { // a refund from before public.refunds; tracked ones are posted below
			d, c, id = paymentLeg(pi, "", true)
			add("stripe-refund", pi, caseID, refunded, d, c, id, paymentLegKey(pi, true))
		}
	}
	rows.Close()

	// Refunds (refunds.go): each reverses its payment's own leg, on that leg's escrow party.
	rows, err = s.db.Query(r.Context(), `
		SELECT case_id, ledger_key, coalesce(party,''), amount_cents FROM public.refunds
		WHERE tenant=$1 AND kind='FEE' AND status='SUCCEEDED' AND coalesce(ledger_key,'') <> '' ORDER BY created_at`, tenant)
	if err != nil {
		slog.Error("ledger backfill: refunds query", "err", err)
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var caseID, key, party string
		var amount int64
		if rows.Scan(&caseID, &key, &party, &amount) != nil {
			continue
		}
		d, c, id := paymentLeg(key, party, true)
		add("refund", key, caseID, amount, d, c, id, paymentLegKey(key, true))
	}
	rows.Close()

	// Cleared checks: settleClearedCheck posts "chk:<id>" with no party.
	rows, err = s.db.Query(r.Context(), `
		SELECT id::text, coalesce(case_id::text,''), amount_cents FROM public.checks
		WHERE tenant=$1 AND status='CLEARED' ORDER BY created_at`, tenant)
	if err != nil {
		slog.Error("ledger backfill: checks query", "err", err)
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var id, caseID string
		var amount int64
		if rows.Scan(&id, &caseID, &amount) != nil {
			continue
		}
		d, c, tid := paymentLeg("chk:"+id, "", false)
		add("check", "chk:"+id, caseID, amount, d, c, tid, paymentLegKey("chk:"+id, false))
	}
	rows.Close()

	// Which of these the ledger already holds.
	ids := make([]tb_types.Uint128, 0, 2*len(legs))
	for i := range legs {
		ids = append(ids, legs[i].id, legs[i].retryID)
	}
	have := map[string]bool{}
	if len(ids) > 0 {
		got, err := s.tb.LookupTransfers(ids)
		if err != nil {
			http.Error(w, `{"error":"ledger lookup failed"}`, http.StatusBadGateway)
			return
		}
		for _, t := range got {
			have[hex128(t.ID)] = true
		}
	}
	var posted, already, failed, total int64
	for i := range legs {
		l := &legs[i]
		total += l.Amount
		switch {
		case have[hex128(l.id)] || have[hex128(l.retryID)]:
			l.Result = "already in ledger"
			already++
		case !apply:
			l.Result = "would post"
		default:
			res, err := s.postLedger(tenant, l.debit, l.credit, tb_types.Transfer{ID: l.id, Amount: tb_types.ToUint128(uint64(l.Amount))})
			if err == nil && len(res) == 1 && res[0].Result == tb_types.TransferIDAlreadyFailed {
				res, err = s.postLedger(tenant, l.debit, l.credit, tb_types.Transfer{ID: l.retryID, Amount: tb_types.ToUint128(uint64(l.Amount))})
			}
			l.Result = "posted"
			if err != nil {
				l.Result, failed = "failed: "+err.Error(), failed+1
				continue
			}
			for _, rr := range res {
				if rr.Result != tb_types.TransferOK && rr.Result != tb_types.TransferExists {
					l.Result = "rejected: " + rr.Result.String()
				}
			}
			if l.Result == "posted" {
				posted++
			} else {
				failed++
			}
		}
	}
	var svcInvoices, payables int64
	_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM public.billing_invoices WHERE tenant=$1`, tenant).Scan(&svcInvoices)
	_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM public.payables WHERE tenant=$1`, tenant).Scan(&payables)
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant": tenant, "namespace": ledgerNamespace, "applied": apply,
		"legs": legs, "total_cents": total, "posted": posted, "already_in_ledger": already, "failed": failed,
		"not_replayed": map[string]any{"service_invoices": svcInvoices, "payables": payables,
			"fee_transfers": "no database record"},
	})
}
