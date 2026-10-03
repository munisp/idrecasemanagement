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
	"fmt"
	"net/smtp"
	"strings"
)

// sendMail delivers one message. Returns nil on acceptance by the relay.
func (s *server) sendMail(to, cc []string, subject, body string) error {
	cfg := s.cfg
	if cfg.SMTPHost == "" {
		return fmt.Errorf("smtp not configured")
	}
	from := cfg.SMTPFrom
	all := append(append([]string{}, to...), cc...)
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	if len(cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", strings.Join(cc, ", "))
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(body)

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
		if _, err := w.Write([]byte(b.String())); err != nil {
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
	return smtp.SendMail(addr, auth, from, all, []byte(b.String()))
}
