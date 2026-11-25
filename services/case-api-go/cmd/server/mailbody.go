// mailbody.go — how an outgoing email is put together, so it reads the same in the inbox as on the screen it was
// written on.
//
// Every message carries a plain-text part and an HTML part (multipart/alternative). Both are quoted-printable, so
// no line is longer than mail servers allow: a long link (a payment link, a secure upload link) is never broken
// in two, and accented letters and typographic quotes arrive intact. The HTML part keeps the text's paragraphs and
// line breaks and makes links and email addresses clickable; mail programs that show plain text get the text as
// written. The subject is encoded (RFC 2047) when it has anything beyond plain ASCII, such as a dash.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"mime"
	"mime/quotedprintable"
	"regexp"
	"strings"
	"time"
)

// headerText encodes a header value that is not plain ASCII ("Your account — Acme" -> =?utf-8?q?...?=).
func headerText(v string) string {
	for _, c := range v {
		if c > 126 || c < 32 {
			return mime.QEncoding.Encode("utf-8", v)
		}
	}
	return v
}

// messageID: a unique id in the sender's own domain.
func messageID(from string) string {
	domain := "localhost"
	if i := strings.LastIndex(from, "@"); i >= 0 {
		domain = strings.Trim(from[i+1:], "> ")
	}
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), randHex(12), domain)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func qp(s string) string {
	var b strings.Builder
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.String()
}

// writeAlternative writes a multipart/alternative entity (its own Content-Type header included) for body.
func writeAlternative(b *strings.Builder, body string) {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	boundary := "alt-" + randHex(12)
	fmt.Fprintf(b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n%s\r\n", boundary, qp(body))
	fmt.Fprintf(b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n%s\r\n", boundary, qp(mailHTML(body)))
	fmt.Fprintf(b, "--%s--\r\n", boundary)
}

var (
	mailLinkRe  = regexp.MustCompile(`https?://[^\s<>"]+`)
	mailEmailRe = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
)

// mailHTML lays the text out as HTML: a paragraph per blank-line-separated block, its line breaks kept, links and
// email addresses clickable. Nothing in the text is treated as markup.
func mailHTML(text string) string {
	var paras []string
	for _, block := range regexp.MustCompile(`\n[ \t]*\n+`).Split(strings.TrimSpace(text), -1) {
		lines := strings.Split(block, "\n")
		for i, ln := range lines {
			lines[i] = linkify(strings.TrimRight(ln, " \t"))
		}
		paras = append(paras, `<p style="margin:0 0 14px">`+strings.Join(lines, "<br>\n")+"</p>")
	}
	return `<!doctype html><html><body style="margin:0;padding:0;background:#ffffff">` +
		`<div style="max-width:640px;padding:20px 16px;font-family:Arial,Helvetica,sans-serif;font-size:15px;line-height:1.55;color:#1f2328">` +
		strings.Join(paras, "\n") + `</div></body></html>`
}

// linkify escapes one line and turns its links and email addresses into anchors. A link's trailing full stop or
// bracket is the sentence's, not the link's.
func linkify(line string) string {
	var out strings.Builder
	last := 0
	for _, m := range mailLinkRe.FindAllStringIndex(line, -1) {
		url := strings.TrimRight(line[m[0]:m[1]], ".,;:)]")
		end := m[0] + len(url)
		out.WriteString(emailLinks(html.EscapeString(line[last:m[0]])))
		fmt.Fprintf(&out, `<a href="%s" style="color:#0b5cad;word-break:break-all">%s</a>`, html.EscapeString(url), html.EscapeString(url))
		last = end
	}
	out.WriteString(emailLinks(html.EscapeString(line[last:])))
	return out.String()
}

func emailLinks(escaped string) string {
	return mailEmailRe.ReplaceAllStringFunc(escaped, func(a string) string {
		return fmt.Sprintf(`<a href="mailto:%s" style="color:#0b5cad">%s</a>`, a, a)
	})
}
