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
	writeAlternative(&b, body)
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
		`#fid" style`, `href="mailto:flcdr@capitolbridge.com"`, "Health Plan’s", "<br>"} {
		if !strings.Contains(h, want) {
			t.Errorf("HTML is missing %q", want)
		}
	}
	if strings.Contains(h, `#fid." style`) {
		t.Error("the sentence's full stop became part of the link")
	}
}

func TestMailHTMLEscapes(t *testing.T) {
	if h := mailHTML("<script>alert(1)</script> & co"); strings.Contains(h, "<script>") || !strings.Contains(h, "&amp; co") {
		t.Errorf("text must not become markup: %s", h)
	}
}
