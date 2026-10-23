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
func (s *server) sendMail(to, cc []string, subject, body string) error {
	cfg := s.cfg
	if cfg.SMTPHost == "" {
		return fmt.Errorf("smtp not configured")
	}
	var err error
	if to, err = cleanAddrs(to); err != nil {
		return err
	}
	if cc, err = cleanAddrs(cc); err != nil {
		return err
	}
	from := cfg.SMTPFrom
	var b strings.Builder
	writeMailHeaders(&b, from, to, cc, subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	writeAlternative(&b, body) // plain text + the same text as HTML (mailbody.go)
	return s.deliver(from, to, cc, []byte(b.String()))
}

// sendMailWithAttachment delivers a plain-text body plus one attachment
// (MIME multipart/mixed, base64 — RFC 2045). Used for report send-out: the
// body is the human summary, the CSV is the working artifact.
func (s *server) sendMailWithAttachment(to, cc []string, subject, body, filename string, content []byte) error {
	cfg := s.cfg
	if cfg.SMTPHost == "" {
		return fmt.Errorf("smtp not configured")
	}
	from := cfg.SMTPFrom
	boundary := "idre-" + fmt.Sprintf("%d", time.Now().UnixNano())
	var b strings.Builder
	writeMailHeaders(&b, from, to, cc, subject)
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	writeAlternative(&b, body)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/csv; name=%q\r\nContent-Disposition: attachment; filename=%q\r\nContent-Transfer-Encoding: base64\r\n\r\n",
		boundary, filename, filename)
	enc := base64.StdEncoding.EncodeToString(content)
	for i := 0; i < len(enc); i += 76 { // RFC 2045 line length
		end := i + 76
		if end > len(enc) {
			end = len(enc)
		}
		b.WriteString(enc[i:end] + "\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return s.deliver(from, to, cc, []byte(b.String()))
}

func writeMailHeaders(b *strings.Builder, from string, to, cc []string, subject string) {
	fmt.Fprintf(b, "From: %s\r\n", from)
	fmt.Fprintf(b, "To: %s\r\n", strings.Join(to, ", "))
	if len(cc) > 0 {
		fmt.Fprintf(b, "Cc: %s\r\n", strings.Join(cc, ", "))
	}
	fmt.Fprintf(b, "Subject: %s\r\n", headerText(subject))
	fmt.Fprintf(b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(b, "Message-ID: %s\r\n", messageID(from))
}

// smtpDeliver runs the SMTP transaction for an already-composed message (deliver, in mailqueue.go, paces and
// queues around it).
func (s *server) smtpDeliver(from string, to, cc []string, msg []byte) error {
	cfg := s.cfg
	all := append(append([]string{}, to...), cc...)
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
