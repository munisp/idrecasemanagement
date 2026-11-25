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

// writeAlternative writes a multipart/alternative entity (its own Content-Type header included) for body; brand
// heads the HTML part ("" for none).
func writeAlternative(b *strings.Builder, body, brand string) {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	boundary := "alt-" + randHex(12)
	fmt.Fprintf(b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n%s\r\n", boundary, qp(body))
	fmt.Fprintf(b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n%s\r\n", boundary, qp(mailHTML(body, brand)))
	fmt.Fprintf(b, "--%s--\r\n", boundary)
}

var (
	mailLinkRe  = regexp.MustCompile(`https?://[^\s<>"]+`)
	mailEmailRe = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
)

// The HTML part is laid out for reading (change log 2026-11-25 #11, "refine the templates for greater
// presentation"); the words are the template's own, untouched:
//   - the program's name heads the message, on a card centred on a light background;
//   - "Label: https://…" on a line of its own becomes a button reading Label (a note in brackets after the label
//     goes under the button), and a link alone on its line becomes an "Open the link" button -- no raw payment or
//     upload URLs filling the message (the plain-text part keeps them, for mail programs that show text only);
//   - a link inside a sentence shows its site, not its full address;
//   - a block of short "Name: value" lines (Case Number, Amount due, …) becomes a two-column summary;
//   - lines starting "- " or "• " become a list.
//
// Nothing in the text is treated as markup.
const (
	mailInk    = "#1f2328"
	mailMuted  = "#5b6570"
	mailAccent = "#1e4d6b"
	mailLink   = "#0b5cad"
)

var (
	mailBlockRe  = regexp.MustCompile(`\n[ \t]*\n+`)
	mailBulletRe = regexp.MustCompile(`^\s*(?:[-•*]|\d+[.)])\s+(.+)$`)
	mailFactRe   = regexp.MustCompile(`^([A-Z][A-Za-z0-9 /()&'’.-]{1,40}):\s+(\S.{0,90})$`)
	mailButtonRe = regexp.MustCompile(`^(.{2,160}?):?\s*(https?://\S+?)[.,;)]?$`)
	mailNoteRe   = regexp.MustCompile(`^(.*?)\s*\((.+)\)$`)
)

// mailHTML: the text as HTML, headed by brand (the program's name; "" for no heading).
func mailHTML(text, brand string) string {
	var blocks []string
	for _, block := range mailBlockRe.Split(strings.TrimSpace(text), -1) {
		blocks = append(blocks, mailBlock(strings.Split(block, "\n")))
	}
	head, foot := "", ""
	if brand = strings.TrimSpace(brand); brand != "" {
		head = fmt.Sprintf(`<tr><td style="background:%s;padding:16px 28px;font-size:17px;font-weight:bold;color:#ffffff;letter-spacing:.2px">%s</td></tr>`,
			mailAccent, html.EscapeString(brand))
		foot = fmt.Sprintf(`<tr><td style="padding:14px 28px;border-top:1px solid #e3e6ea;font-size:12px;color:%s">%s</td></tr>`, mailMuted, html.EscapeString(brand))
	}
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>` +
		`<body style="margin:0;padding:0;background:#f3f5f7">` +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f3f5f7"><tr><td align="center" style="padding:24px 12px">` +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:640px;background:#ffffff;border:1px solid #e3e6ea;border-radius:8px;overflow:hidden;` +
		`font-family:Arial,Helvetica,sans-serif;font-size:15px;line-height:1.55;color:` + mailInk + `">` + head +
		`<tr><td style="padding:24px 28px 10px">` + strings.Join(blocks, "\n") + `</td></tr>` + foot +
		`</table></td></tr></table></body></html>`
}

// mailBlock lays out one blank-line-separated block.
func mailBlock(lines []string) string {
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	// A list.
	if mailAll(lines, func(l string) bool { return mailBulletRe.MatchString(l) }) {
		var items []string
		for _, l := range lines {
			items = append(items, `<li style="margin:0 0 6px">`+linkify(mailBulletRe.FindStringSubmatch(l)[1])+`</li>`)
		}
		return `<ul style="margin:0 0 16px;padding-left:22px">` + strings.Join(items, "") + `</ul>`
	}
	// A summary of short "Name: value" lines.
	if len(lines) <= 8 && mailAll(lines, func(l string) bool { return mailFactRe.MatchString(l) && !mailLinkRe.MatchString(l) }) {
		var rows []string
		for _, l := range lines {
			m := mailFactRe.FindStringSubmatch(l)
			rows = append(rows, fmt.Sprintf(`<tr><td style="padding:4px 16px 4px 0;color:%s;font-size:13px;white-space:nowrap;vertical-align:top">%s</td>`+
				`<td style="padding:4px 0;font-weight:bold;vertical-align:top">%s</td></tr>`, mailMuted, html.EscapeString(m[1]), emailLinks(html.EscapeString(m[2]))))
		}
		return `<table role="presentation" cellpadding="0" cellspacing="0" style="margin:0 0 18px;border-left:3px solid ` + mailAccent +
			`;padding-left:12px">` + strings.Join(rows, "") + `</table>`
	}
	// Paragraph text, with links on their own line turned into buttons.
	var out, para []string
	flush := func() {
		if len(para) > 0 {
			out = append(out, `<p style="margin:0 0 14px">`+strings.Join(para, "<br>\n")+"</p>")
			para = nil
		}
	}
	prev := ""
	for _, l := range lines {
		if b := mailButton(l, prev); b != "" {
			flush()
			out = append(out, b)
			continue
		}
		para = append(para, linkify(l))
		prev = l
	}
	flush()
	return strings.Join(out, "\n")
}

// mailButton: the button for a line that is "Label: link" or just a link ("" when the line is something else). A
// link alone is named from the sentence before it ("…the letter linked below." -> "Open the letter").
func mailButton(line, prev string) string {
	t := strings.TrimSpace(line)
	var label, note, url string
	if mailLinkRe.MatchString(t) && mailLinkRe.FindString(t) == t {
		label, url = linkLabel(prev), strings.TrimRight(t, ".,;:)]")
	} else if m := mailButtonRe.FindStringSubmatch(t); m != nil && strings.HasSuffix(strings.TrimSpace(t[:strings.Index(t, m[2])]), ":") &&
		strings.TrimRight(mailLinkRe.FindString(t), ".,;:)]") == m[2] && len(mailLinkRe.FindAllString(t, -1)) == 1 {
		label, url = strings.TrimSpace(m[1]), m[2]
		if n := mailNoteRe.FindStringSubmatch(label); n != nil && n[1] != "" {
			label, note = n[1], n[2]
		}
	} else {
		return ""
	}
	b := fmt.Sprintf(`<table role="presentation" cellpadding="0" cellspacing="0" style="margin:4px 0 %s"><tr><td style="background:%s;border-radius:6px">`+
		`<a href="%s" style="display:inline-block;padding:11px 20px;color:#ffffff;font-weight:bold;text-decoration:none;font-size:15px">%s</a></td></tr></table>`,
		map[bool]string{true: "4px", false: "14px"}[note != ""], mailAccent, html.EscapeString(url), html.EscapeString(label))
	if note != "" {
		b += fmt.Sprintf(`<p style="margin:0 0 14px;font-size:13px;color:%s">%s</p>`, mailMuted, html.EscapeString(note))
	}
	return b
}

func mailAll(xs []string, f func(string) bool) bool {
	for _, x := range xs {
		if !f(x) {
			return false
		}
	}
	return len(xs) > 0
}

// shortLink: what a link inside a sentence shows -- the site, and the start of the path when the address is long.
func shortLink(url string) string {
	if len(url) <= 48 {
		return url
	}
	rest := url[strings.Index(url, "//")+2:]
	host, path := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host, path = rest[:i], rest[i:]
	}
	if len(path) > 18 {
		path = path[:18] + "…"
	}
	return host + path
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
		fmt.Fprintf(&out, `<a href="%s" style="color:#0b5cad;word-break:break-all">%s</a>`, html.EscapeString(url), html.EscapeString(shortLink(url)))
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

func linkLabel(prev string) string {
	p := strings.ToLower(prev)
	switch {
	case strings.Contains(p, "invoice"):
		return "View the invoice"
	case strings.Contains(p, "upload"):
		return "Upload documents"
	case strings.Contains(p, "letter"), strings.Contains(p, "final order"), strings.Contains(p, "rationale"):
		return "Open the letter"
	case strings.Contains(p, "document"), strings.Contains(p, "file"):
		return "Open the documents"
	}
	return "Open the link"
}
