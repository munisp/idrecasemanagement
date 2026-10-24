package main

// card_fee.go — the credit card transaction fee on an initial fee (AHCA price sheet, line 6).
//
// An initial fee is its invoice's amount. Paid by card it is that amount plus the card fee of the fee schedule's
// entry in force on the day the invoice was raised (fees.initial_fee_schedule[].card_fee_cents); paid from a bank
// account or by check it is the amount alone. Review invoices carry no card fee.
//
// So a payment page must know how it will be paid. Stripe's checkout no longer takes the methods per session; a
// session names a payment method configuration instead. A card payment's page uses "IDRE card" (cards and card
// wallets) and adds the fee as its own line; a bank payment's page uses "IDRE bank account". Both configurations
// are looked up by name, and created on the Stripe account the first time they are needed.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	payByCard = "card"
	payByBank = "bank"
)

// payMethodOf is a requested way to pay: bank (a US bank account) or, by default, card.
func payMethodOf(v string) string {
	if v == payByBank || v == "ach" || v == "us_bank_account" {
		return payByBank
	}
	return payByCard
}

// cardFees is the card fee each of these invoices owes when paid by card, and their total.
func (s *server) cardFees(ctx context.Context, tenant string, invoiceIDs []string) (map[string]int64, int64) {
	out := map[string]int64{}
	if len(invoiceIDs) == 0 {
		return out, 0
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	cfg := s.loadProgram(r, tenant)
	if cfg == nil || len(cfg.Fees.InitialFeeSchedule) == 0 {
		return out, 0
	}
	rows, err := s.db.Query(ctx, `SELECT id::text, created_at FROM public.invoices WHERE tenant=$1 AND id::text = ANY($2) AND kind='INITIAL_FEE'`,
		tenant, invoiceIDs)
	if err != nil {
		return out, 0
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var id string
		var at time.Time
		if rows.Scan(&id, &at) != nil {
			continue
		}
		if st, ok := stepAt(cfg.Fees.InitialFeeSchedule, at, cfg.location()); ok && st.CardFeeCents > 0 {
			out[id] = st.CardFeeCents
			total += st.CardFeeCents
		}
	}
	return out, total
}

// payMethodConfigs: Stripe payment method configuration ids by name, once found or made.
var payMethodConfigs sync.Map

var payMethodConfigNames = map[string]string{payByCard: "IDRE card", payByBank: "IDRE bank account"}

// payMethodConfigOn: what each configuration offers.
var payMethodConfigOn = map[string]map[string]bool{
	payByCard: {"card": true, "apple_pay": true, "google_pay": true},
	payByBank: {"us_bank_account": true},
}

// payMethodConfig is the id of the Stripe payment method configuration that offers only this way to pay.
func (s *server) payMethodConfig(method string) (string, error) {
	name := payMethodConfigNames[method]
	if v, ok := payMethodConfigs.Load(name); ok {
		return v.(string), nil
	}
	list, err := s.stripeGet("/v1/payment_method_configurations?limit=100")
	if err != nil {
		return "", err
	}
	data, _ := list["data"].([]any)
	var methods []string
	for _, x := range data {
		c, _ := x.(map[string]any)
		if c["name"] == name && c["active"] == true {
			id := fmt.Sprint(c["id"])
			payMethodConfigs.Store(name, id)
			return id, nil
		}
		if c["is_default"] == true {
			for k, v := range c {
				if m, ok := v.(map[string]any); ok && m["display_preference"] != nil {
					methods = append(methods, k)
				}
			}
		}
	}
	// Not made yet: every method the account offers is turned off but this one's.
	form := url.Values{}
	form.Set("name", name)
	for _, m := range methods {
		pref := "off"
		if payMethodConfigOn[method][m] {
			pref = "on"
		}
		form.Set(m+"[display_preference][preference]", pref)
	}
	made, err := s.stripePost("/v1/payment_method_configurations", form)
	if err != nil {
		return "", err
	}
	id := fmt.Sprint(made["id"])
	payMethodConfigs.Store(name, id)
	return id, nil
}

// checkoutFor sets a Stripe checkout session up for one way to pay. For a card it adds the card fee as its own
// line (line item index n), when there is one.
func (s *server) checkoutFor(form url.Values, method string, cardFee int64, n int) error {
	pmc, err := s.payMethodConfig(method)
	if err != nil {
		return fmt.Errorf("the payment page for this way to pay could not be set up: %w", err)
	}
	form.Set("payment_method_configuration", pmc)
	if method == payByCard && cardFee > 0 {
		form.Set(fmt.Sprintf("line_items[%d][quantity]", n), "1")
		form.Set(fmt.Sprintf("line_items[%d][price_data][currency]", n), "usd")
		form.Set(fmt.Sprintf("line_items[%d][price_data][unit_amount]", n), strconv.FormatInt(cardFee, 10))
		form.Set(fmt.Sprintf("line_items[%d][price_data][product_data][name]", n), "Credit card transaction fee")
	}
	form.Set("metadata[pay_by]", method)
	form.Set("metadata[card_fee_cents]", strconv.FormatInt(map[bool]int64{true: cardFee}[method == payByCard], 10))
	return nil
}

// paidWords: the activity line for a fee paid through Stripe.
func paidWords(amount, cardFee int64, ref string) string {
	if cardFee > 0 {
		return fmt.Sprintf("Payment of %s received by card, plus the %s card transaction fee (%s)", usd(amount), usd(cardFee), ref)
	}
	return fmt.Sprintf("Payment of %s received (%s)", usd(amount), ref)
}

// payLinksText: the pay links of a fee in an email, each with what it charges.
func payLinksText(amount, cardFee int64, cardURL, bankURL string) string {
	card := "Pay by card: " + cardURL
	if cardFee > 0 {
		card = fmt.Sprintf("Pay by card (%s, which includes a %s credit card transaction fee): %s", usd(amount+cardFee), usd(cardFee), cardURL)
	}
	if bankURL == "" {
		return card
	}
	return card + "\n" + fmt.Sprintf("Pay from a bank account (%s, no card fee; clears in 3–5 business days): %s", usd(amount), bankURL)
}
