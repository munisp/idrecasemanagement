package main

import "testing"

// How an invoice was paid decides how it is refunded and which ledger leg the refund reverses.
func TestSourceFor(t *testing.T) {
	for _, c := range []struct {
		name                           string
		found                          bool
		provider, session, pi, inv, pt string
		want                           refundSource
	}{
		{"marked paid by hand", false, "", "", "", "inv1", "PROVIDER", refundSource{Method: "offline"}},
		{"one-invoice card checkout", true, "stripe", "cs_1", "pi_1", "inv1", "PROVIDER",
			refundSource{Method: "stripe", PaymentID: "p1", PaymentIntent: "pi_1", LedgerKey: "pi_1", LedgerParty: "PROVIDER"}},
		{"group card checkout", true, "stripe", "cs_g#inv1", "pi_g", "inv1", "PROVIDER",
			refundSource{Method: "stripe", PaymentID: "p1", PaymentIntent: "pi_g", LedgerKey: "pi_g:inv1", LedgerParty: "PROVIDER"}},
		{"check", true, "check", "chk:42", "", "inv1", "PROVIDER",
			refundSource{Method: "check", PaymentID: "p1", LedgerKey: "chk:42"}},
		{"payment row with no intent", true, "stripe", "cs_1", "", "inv1", "PROVIDER",
			refundSource{Method: "offline", PaymentID: "p1"}},
	} {
		if got := sourceFor(c.found, c.provider, c.session, c.pi, "p1", c.inv, c.pt); got != c.want {
			t.Errorf("%s: sourceFor = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestStripeRefundOutcome(t *testing.T) {
	for st, want := range map[string]string{"succeeded": "succeeded", "failed": "failed", "canceled": "failed",
		"pending": "", "requires_action": "", "": ""} {
		if got := stripeRefundOutcome(st); got != want {
			t.Errorf("stripeRefundOutcome(%q) = %q, want %q", st, got, want)
		}
	}
}

// A refund made in the Stripe dashboard is applied to a fee only when there is no doubt which fee and how much.
func TestChargeRefundedDecision(t *testing.T) {
	for _, c := range []struct {
		refunded, payment int64
		group             bool
		want              string
	}{
		{41200, 41200, false, "record"}, // the whole of a one-fee payment
		{10000, 41200, false, "ask"},    // part of it
		{41200, 82400, true, "ask"},     // a payment covering two fees
		{82400, 82400, true, "ask"},
		{0, 0, false, "ask"},
	} {
		if got := chargeRefundedDecision(c.refunded, c.payment, c.group); got != c.want {
			t.Errorf("chargeRefundedDecision(%d, %d, %v) = %q, want %q", c.refunded, c.payment, c.group, got, c.want)
		}
	}
}

// A second refund after the first came back gets its own ledger leg; a first refund keeps the payment's key.
func TestRetryLedgerKey(t *testing.T) {
	for _, c := range []struct {
		key  string
		n    int
		want string
	}{{"pi_1", 0, "pi_1"}, {"pi_1", 1, "pi_1:refund-retry-1"}, {"chk:9", 2, "chk:9:refund-retry-2"}, {"", 3, ""}} {
		if got := retryLedgerKey(c.key, c.n); got != c.want {
			t.Errorf("retryLedgerKey(%q, %d) = %q, want %q", c.key, c.n, got, c.want)
		}
	}
}

// What is left of a not-applied amount: its amount less what was refunded or applied, never below zero.
func TestUnappliedLeft(t *testing.T) {
	for _, c := range []struct {
		amount   int64
		resolved []int64
		want     int64
	}{{237641, nil, 237641}, {237641, []int64{12359}, 225282}, {1000, []int64{600, 400}, 0}, {1000, []int64{1500}, 0}} {
		if got := unappliedLeft(c.amount, c.resolved); got != c.want {
			t.Errorf("unappliedLeft(%d, %v) = %d, want %d", c.amount, c.resolved, got, c.want)
		}
	}
}
