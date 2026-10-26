// Author: Patrick Munis at newwave
// mailer.go — outbound email via SMTP (stdlib net/smtp, vendor-neutral).
// Works with the state's own SMTP relay, Amazon SES SMTP, Mailgun SMTP, or
// any RFC-compliant submission endpoint — set via env, no code change.
// Fail-closed semantics live at the call site: when SMTP is not configured,
// QA approval still records SENT in the correspondence log but the activity
// notes "delivery skipped — SMTP not configured", so nothing is ever silently
// dropped.
package main

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// cleanAddrs tidies addresses typed by hand -- spaces, and a full stop, comma or semicolon carried over from a
// sentence ("…@gmail.com.") -- and refuses one that is still not an email address, in words a person can act on
// instead of the relay's "501 Bad recipient address syntax".
func cleanAddrs(list []string) ([]string, error) {
	out := make([]string, 0, len(list))
	for _, a := range list {
		a = strings.Trim(strings.TrimSpace(a), ".,;:<>\"' ")
		if a == "" {
			continue
		}
		if parsed, err := mail.ParseAddress(a); err != nil || parsed.Address != a || !strings.Contains(a[strings.LastIndex(a, "@")+1:], ".") {
			return nil, fmt.Errorf("%q is not a valid email address — correct it and send again", a)
		}
		out = append(out, a)
	}
	return out, nil
}

// sendMail delivers one message. Returns nil on acceptance by the relay.
func (s *server) sendMail(tenant string, to, cc []string, subject, body string) error {
	_, err := s.sendMailID(tenant, to, cc, subject, body)
	return err
}

// sendMailID is sendMail, returning the Message-ID the mail carries, so the mail log can record it and a reply
// (In-Reply-To) can be filed under the case the mail was about (mailfiling.go).
func (s *server) sendMailID(tenant string, to, cc []string, subject, body string) (string, error) {
	if s.cfg.SMTPHost == "" {
		return "", fmt.Errorf("smtp not configured")
	}
	var err error
	if to, err = cleanAddrs(to); err != nil {
		return "", err
	}
	if cc, err = cleanAddrs(cc); err != nil {
		return "", err
	}
	if id, sent := s.viaCompanyOutlook(tenant, to, cc, subject, body, nil); sent {
		return id, nil
	}
	header, envelope := s.systemFrom(tenant)
	id := messageID(envelope)
	var b strings.Builder
	writeMailHeaders(&b, header, to, cc, subject, id)
	b.WriteString("MIME-Version: 1.0\r\n")
	writeAlternative(&b, body) // plain text + the same text as HTML (mailbody.go)
	return id, s.deliver(envelope, to, cc, []byte(b.String()))
}

// sendMailWithAttachment delivers a plain-text body plus one attachment
// (MIME multipart/mixed, base64 — RFC 2045). Used for report send-out: the
// body is the human summary, the CSV is the working artifact.
func (s *server) sendMailWithAttachment(tenant string, to, cc []string, subject, body, filename string, content []byte) error {
	if s.cfg.SMTPHost == "" {
		return fmt.Errorf("smtp not configured")
	}
	if _, sent := s.viaCompanyOutlook(tenant, to, cc, subject, body, []outAttachment{{Name: filename, ContentType: "text/csv", Content: content}}); sent {
		return nil
	}
	header, from := s.systemFrom(tenant)
	var b strings.Builder
	writeMailHeaders(&b, header, to, cc, subject, messageID(from))
	b.WriteString("MIME-Version: 1.0\r\n")
	writeMixed(&b, body, []outAttachment{{Name: filename, ContentType: "text/csv", Content: content}})
	return s.deliver(from, to, cc, []byte(b.String()))
}

// writeMixed writes a multipart/mixed body: the text (plain + HTML, writeAlternative), then each file in base64
// (RFC 2045 lines of 76).
func writeMixed(b *strings.Builder, body string, atts []outAttachment) {
	boundary := "idre-" + fmt.Sprintf("%d", time.Now().UnixNano())
	fmt.Fprintf(b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(b, "--%s\r\n", boundary)
	writeAlternative(b, body)
	for _, a := range atts {
		name := strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(a.Name)
		fmt.Fprintf(b, "--%s\r\nContent-Type: %s; name=%q\r\nContent-Disposition: attachment; filename=%q\r\nContent-Transfer-Encoding: base64\r\n\r\n",
			boundary, orWord(a.ContentType, "application/octet-stream"), headerText(name), headerText(name))
		enc := base64.StdEncoding.EncodeToString(a.Content)
		for i := 0; i < len(enc); i += 76 {
			end := i + 76
			if end > len(enc) {
				end = len(enc)
			}
			b.WriteString(enc[i:end] + "\r\n")
		}
	}
	fmt.Fprintf(b, "--%s--\r\n", boundary)
}

func writeMailHeaders(b *strings.Builder, from string, to, cc []string, subject, msgID string) {
	fmt.Fprintf(b, "From: %s\r\n", from)
	fmt.Fprintf(b, "To: %s\r\n", strings.Join(to, ", "))
	if len(cc) > 0 {
		fmt.Fprintf(b, "Cc: %s\r\n", strings.Join(cc, ", "))
	}
	fmt.Fprintf(b, "Subject: %s\r\n", headerText(subject))
	fmt.Fprintf(b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(b, "Message-ID: %s\r\n", msgID)
}

// smtpDeliver runs the SMTP transaction for an already-composed message (deliver, in mailqueue.go, paces and
// queues around it).
func (s *server) smtpDeliver(from string, to, cc []string, msg []byte) error {
	cfg := s.cfg
	// Test addresses (EMAIL_TEST_DOMAINS: dev's .test suites) can never take mail: they are left off the envelope
	// rather than spent from the provider's quota. Mail only to test addresses is not sent at all.
	all := []string{}
	for _, a := range append(append([]string{}, to...), cc...) {
		if !emailTestDomain(strings.ToLower(a[strings.LastIndex(a, "@")+1:])) {
			all = append(all, a)
		}
	}
	if len(all) == 0 {
		slog.Info("mail to test addresses only, not sent", "to", to, "cc", cc)
		return nil
	}
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	if cfg.SMTPPort == 465 { // implicit TLS
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: cfg.SMTPHost})
		if err != nil {
			return err
		}
		c, err := smtp.NewClient(conn, cfg.SMTPHost)
		if err != nil {
			return err
		}
		defer c.Close()
		if cfg.SMTPUser != "" {
			if err := c.Auth(smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPass, cfg.SMTPHost)); err != nil {
				return err
			}
		}
		if err := c.Mail(from); err != nil {
			return err
		}
		for _, rcpt := range all {
			if err := c.Rcpt(rcpt); err != nil {
				return err
			}
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(msg); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return c.Quit()
	}
	// 587/25: SendMail upgrades via STARTTLS when the server advertises it.
	var auth smtp.Auth
	if cfg.SMTPUser != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPass, cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, from, all, msg)
}
