package main

import (
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func TestMailKeepsItsLayout(t *testing.T) {
	body := "Case Number: FL26-1\n\nDear Pat,\n\nPay securely here: https://checkout.stripe.com/c/pay/cs_test_" + strings.Repeat("Ab9", 120) + "#fid.\n" +
		"Questions? email flcdr@capitolbridge.com or call.\n\nThe Health Plan’s response is due."
	var b strings.Builder
	writeMailHeaders(&b, "no-reply@idre.test", []string{"a@b.test"}, nil, "Your account — Acme", messageID("no-reply@idre.test"))
	b.WriteString("MIME-Version: 1.0\r\n")
	writeAlternative(&b, body, "Capitol Bridge")
	raw := b.String()
	for _, ln := range strings.Split(raw, "\r\n") {
		if len(ln) > 998 {
			t.Fatalf("a line of %d characters: mail servers break those", len(ln))
		}
	}
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if subj, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject")); subj != "Your account — Acme" {
		t.Errorf("subject arrives as %q", subj)
	}
	if msg.Header.Get("Date") == "" || msg.Header.Get("Message-ID") == "" {
		t.Error("Date and Message-ID headers are missing")
	}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	mr := multipart.NewReader(msg.Body, params["boundary"])
	parts := map[string]string{}
	for {
		p, err := mr.NextPart() // decodes quoted-printable
		if err != nil {
			break
		}
		data, _ := io.ReadAll(p)
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		parts[ct] = string(data)
	}
	if strings.ReplaceAll(parts["text/plain"], "\r\n", "\n") != body {
		t.Errorf("the plain text changed in transit:\n%q", parts["text/plain"])
	}
	h := parts["text/html"]
	for _, want := range []string{"<p style=\"margin:0 0 14px\">Dear Pat,</p>", `href="https://checkout.stripe.com/c/pay/cs_test_`,
		`#fid" style`, `>Pay securely here</a>`, `href="mailto:flcdr@capitolbridge.com"`, "Health Plan’s", ">Capitol Bridge<", ">FL26-1<"} {
		if !strings.Contains(h, want) {
			t.Errorf("HTML is missing %q", want)
		}
	}
	if strings.Contains(h, `#fid." style`) {
		t.Error("the sentence's full stop became part of the link")
	}
}

func TestMailHTMLEscapes(t *testing.T) {
	if h := mailHTML("<script>alert(1)</script> & co", "<b>Brand</b>"); strings.Contains(h, "<script>") || strings.Contains(h, "<b>Brand") || !strings.Contains(h, "&amp; co") {
		t.Errorf("text must not become markup: %s", h)
	}
}

func TestMailPresentation(t *testing.T) {
	long := "https://checkout.stripe.com/g/pay/cs_test_" + strings.Repeat("x", 300)
	body := "Thank you for contacting us.\n\nPay by card ($127.59, which includes a $3.99 credit card transaction fee): " + long +
		"\nPay from a bank account ($123.60, no card fee; clears in 3–5 business days): " + long + "2\n\n" +
		"Invoice: INV-7\nAmount due: $50.00\n\n- first thing\n- second thing\n\nSee " + long + " for more.\n\nhttps://idre.newfire.app/api/share/abc"
	h := mailHTML(body, "")
	for _, want := range []string{
		`>Pay by card</a>`, "$127.59, which includes a $3.99 credit card transaction fee",
		`>Pay from a bank account</a>`, "$123.60, no card fee; clears in 3–5 business days",
		`>Invoice<`, `>INV-7<`, `>Amount due<`, `<li style="margin:0 0 6px">first thing</li>`,
		`>checkout.stripe.com/g/pay/cs_test_xxx…</a>`, `>Open the link</a>`, `href="https://idre.newfire.app/api/share/abc"`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("HTML is missing %q", want)
		}
	}
	if strings.Contains(h, ">"+long) {
		t.Error("a long link is shown in full")
	}
	if strings.Contains(h, "background:#1e4d6b;padding:16px") {
		t.Error("no brand, no heading")
	}
	// A sentence that merely contains a colon and a link mid-way stays a sentence.
	if h := mailHTML("Please see the letter linked below.\nhttps://x.test/s/1\n\nPlease find invoice 7 at the secure link below.\nhttps://x.test/s/2", ""); !strings.Contains(h, ">Open the letter</a>") || !strings.Contains(h, ">View the invoice</a>") {
		t.Errorf("a link alone is named from the sentence before it: %s", h)
	}
	if h := mailHTML("Note: see https://example.com/a and https://example.com/b today", ""); strings.Contains(h, "Open the link") || strings.Contains(h, "display:inline-block") {
		t.Errorf("a sentence became a button: %s", h)
	}
}
