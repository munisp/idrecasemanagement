// mailboxes.go — every staff member's own email address (Resend receiving on MAILBOX_DOMAIN), the central history of
// their mail, and sending from it.
//
//	address   provisioned when a staff account is created (createTenantStaff), or on the person's first visit
//	inbound   Resend's email.received webhook (on the same Svix-signed /api/webhooks/mail endpoint) names the
//	          message; its content and attachments are fetched from Resend's receiving API and filed under the
//	          mailbox it was addressed to
//	outbound  POST /me/mail/send through Resend's API, from the person's own address; its delivery reports update
//	          the message as they arrive
//	Outlook   outlook.go: the person's connected account, synchronised into the same history
//
// Every read is scoped to the signed-in person (user_sub = the JWT sub): nobody, an administrator included, can open
// another person's mail through these routes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

const resendAPI = "https://api.resend.com"

// maxMailAttachment: an attachment larger than this is listed but not stored.
const maxMailAttachment = 25 << 20

var mailboxLocalUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// mailboxLocalPart: the address's local part from a username: lower case, letters, digits, dot, dash and underscore;
// anything else becomes a dot. "" when nothing usable is left.
func mailboxLocalPart(username string) string {
	l := mailboxLocalUnsafe.ReplaceAllString(strings.ToLower(strings.TrimSpace(username)), ".")
	l = strings.Trim(regexp.MustCompile(`\.{2,}`).ReplaceAllString(l, "."), ".-_")
	if len(l) > 60 {
		l = strings.TrimRight(l[:60], ".-_")
	}
	return l
}

// resendKey: the key for Resend's API. RESEND_API_KEY, or the SMTP password when the SMTP host is Resend's (Resend's
// SMTP password is an API key).
func (s *server) resendKey() string {
	if s.cfg.ResendAPIKey != "" {
		return s.cfg.ResendAPIKey
	}
	if strings.Contains(strings.ToLower(s.cfg.SMTPHost), "resend.com") {
		return s.cfg.SMTPPass
	}
	return ""
}

func (s *server) mailboxesOn() bool { return s.cfg.MailboxDomain != "" }

// isStaff: an account that works cases, not a TPA, health plan or party.
func isStaff(p principal) bool {
	return !isExternalOnly(p) && !hasAnyRole(p, "PARTY") && p.Subject != "" && p.Username != ""
}

type mailbox struct {
	ID       string `json:"id"`
	Tenant   string `json:"tenant"`
	UserSub  string `json:"-"`
	Username string `json:"username"`
	Address  string `json:"address"`
	Status   string `json:"status"`
}

// provisionMailbox gives a person their address on the inbound domain, once. A taken local part gets -2, -3 ...
func (s *server) provisionMailbox(ctx context.Context, tenant, userSub, username, by string) (mailbox, error) {
	var m mailbox
	if !s.mailboxesOn() {
		return m, errors.New("mailboxes are not configured (MAILBOX_DOMAIN)")
	}
	if err := s.db.QueryRow(ctx, `SELECT id, tenant, user_sub, username, address, status FROM public.user_mailboxes WHERE tenant=$1 AND user_sub=$2`,
		tenant, userSub).Scan(&m.ID, &m.Tenant, &m.UserSub, &m.Username, &m.Address, &m.Status); err == nil {
		return m, nil
	}
	local := mailboxLocalPart(username)
	if local == "" {
		local = "staff." + strings.ReplaceAll(userSub, "-", "")[:8]
	}
	for n := 1; n <= 50; n++ {
		addr := local
		if n > 1 {
			addr = fmt.Sprintf("%s-%d", local, n)
		}
		addr += "@" + s.cfg.MailboxDomain
		err := s.db.QueryRow(ctx, `
			INSERT INTO public.user_mailboxes (tenant, user_sub, username, address, created_by) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT DO NOTHING RETURNING id, tenant, user_sub, username, address, status`,
			tenant, userSub, username, addr, by).Scan(&m.ID, &m.Tenant, &m.UserSub, &m.Username, &m.Address, &m.Status)
		if err == nil {
			s.logAudit(ctx, tenant, "", "MAILBOX_PROVISIONED", map[string]any{"by": by, "user": username, "address": addr})
			return m, nil
		}
		// a concurrent request may have just provisioned this person
		if e2 := s.db.QueryRow(ctx, `SELECT id, tenant, user_sub, username, address, status FROM public.user_mailboxes WHERE tenant=$1 AND user_sub=$2`,
			tenant, userSub).Scan(&m.ID, &m.Tenant, &m.UserSub, &m.Username, &m.Address, &m.Status); e2 == nil {
			return m, nil
		}
	}
	return m, errors.New("no free address for " + local)
}

// myMailbox: the signed-in staff member's mailbox, provisioned on first use.
func (s *server) myMailbox(r *http.Request) (mailbox, principal, error) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !isStaff(p) {
		return mailbox{}, p, errForbidden
	}
	m, err := s.provisionMailbox(r.Context(), tenant, p.Subject, p.Username, p.Username)
	return m, p, err
}

var errForbidden = errors.New("forbidden")

// setMailboxActive follows a staff account being suspended, reactivated or deleted: a suspended person's address stops
// taking mail and their Outlook is disconnected (its tokens dropped, so nothing is read on their behalf any more).
// Reactivating opens the address again; Outlook stays disconnected until they connect it themselves.
func (s *server) setMailboxActive(ctx context.Context, userSub string, active bool, by string) {
	status := "DISABLED"
	if active {
		status = "ACTIVE"
	}
	tag, _ := s.db.Exec(ctx, `UPDATE public.user_mailboxes SET status=$2 WHERE user_sub=$1 AND status<>$2`, userSub, status)
	if !active {
		_, _ = s.db.Exec(ctx, `UPDATE public.outlook_connections SET status='DISCONNECTED', access_token_sealed=NULL, refresh_token_sealed=NULL,
			last_error='account suspended', updated_at=now() WHERE user_sub=$1 AND status<>'DISCONNECTED'`, userSub)
	}
	if tag.RowsAffected() > 0 {
		s.logAudit(ctx, "", "", "MAILBOX_"+status, map[string]any{"by": by, "user_sub": userSub})
	}
}

func mailErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errForbidden) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: personal email is for staff accounts"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
}

func (s *server) mailEvent(ctx context.Context, messageID, event, detail, actor string) {
	_, _ = s.db.Exec(ctx, `INSERT INTO public.mail_message_events (message_id, event, detail, actor) VALUES ($1,$2,$3,$4)`,
		messageID, event, truncate(detail, 1000), actor)
}

// ---- the person's own routes ----------------------------------------------------------------------------------------

// getMyMailbox: GET /me/mailbox -- the address, the Outlook connection and the unread count.
func (s *server) getMyMailbox(w http.ResponseWriter, r *http.Request) {
	m, p, err := s.myMailbox(r)
	if err != nil {
		mailErr(w, err)
		return
	}
	var unread int
	_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM public.mail_messages WHERE tenant=$1 AND user_sub=$2 AND folder='inbox' AND NOT is_read`,
		m.Tenant, p.Subject).Scan(&unread)
	out := map[string]any{"mailbox": m, "unread": unread, "can_send": s.cfg.SMTPHost != "",
		"outlook_available": s.outlookOn(), "outlook": s.outlookStatus(r.Context(), m.Tenant, p.Subject),
		"outlook_admin_consent_url": s.outlookAdminConsentURL()}
	writeJSON(w, http.StatusOK, out)
}

type mailListItem struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Direction   string    `json:"direction"`
	Folder      string    `json:"folder"`
	From        string    `json:"from"`
	FromName    string    `json:"from_name"`
	To          []string  `json:"to"`
	Subject     string    `json:"subject"`
	Preview     string    `json:"preview"`
	Status      string    `json:"status"`
	IsRead      bool      `json:"is_read"`
	Attachments bool      `json:"has_attachments"`
	At          time.Time `json:"at"`
}

// listMyMail: GET /me/mail?folder=inbox|sent|all&source=RESEND|OUTLOOK&q=&before=<RFC3339>&limit=
func (s *server) listMyMail(w http.ResponseWriter, r *http.Request) {
	m, p, err := s.myMailbox(r)
	if err != nil {
		mailErr(w, err)
		return
	}
	q := r.URL.Query()
	args := []any{m.Tenant, p.Subject}
	where := `tenant=$1 AND user_sub=$2`
	if f := q.Get("folder"); f == "inbox" || f == "sent" {
		args = append(args, f)
		where += fmt.Sprintf(" AND folder=$%d", len(args))
	}
	if src := strings.ToUpper(q.Get("source")); src == "RESEND" || src == "OUTLOOK" {
		args = append(args, src)
		where += fmt.Sprintf(" AND source=$%d", len(args))
	}
	if like := searchLike(q.Get("q")); like != "" {
		args = append(args, like)
		n := len(args)
		where += fmt.Sprintf(" AND (subject ILIKE $%d OR from_addr ILIKE $%d OR from_name ILIKE $%d OR to_addrs::text ILIKE $%d OR body_text ILIKE $%d)", n, n, n, n, n)
	}
	if b, err := time.Parse(time.RFC3339Nano, q.Get("before")); err == nil {
		args = append(args, b)
		where += fmt.Sprintf(" AND occurred_at < $%d", len(args))
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(r.Context(), `SELECT id, source, direction, folder, from_addr, from_name, to_addrs, subject,
		left(regexp_replace(body_text, '\s+', ' ', 'g'), 160), status, is_read, has_attachments, occurred_at
		FROM public.mail_messages WHERE `+where+fmt.Sprintf(` ORDER BY occurred_at DESC LIMIT %d`, limit), args...)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []mailListItem{}
	for rows.Next() {
		var it mailListItem
		var to []byte
		if rows.Scan(&it.ID, &it.Source, &it.Direction, &it.Folder, &it.From, &it.FromName, &to, &it.Subject, &it.Preview,
			&it.Status, &it.IsRead, &it.Attachments, &it.At) == nil {
			_ = json.Unmarshal(to, &it.To)
			out = append(out, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out, "address": m.Address})
}

// getMyMail: GET /me/mail/{id} -- the message, its attachments and its history. Opening it marks it read.
func (s *server) getMyMail(w http.ResponseWriter, r *http.Request) {
	m, p, err := s.myMailbox(r)
	if err != nil {
		mailErr(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	var msg struct {
		mailListItem
		CC         []string        `json:"cc"`
		BodyText   string          `json:"body_text"`
		BodyHTML   string          `json:"body_html"`
		Detail     string          `json:"status_detail"`
		Auth       json.RawMessage `json:"auth_results"`
		InternetID string          `json:"internet_message_id"`
		CaseID     string          `json:"case_id"`
		CaseNumber string          `json:"case_number"`
	}
	var to, cc, auth []byte
	err = s.db.QueryRow(r.Context(), `SELECT id, source, direction, folder, from_addr, from_name, to_addrs, cc_addrs, subject, body_text, body_html,
		status, status_detail, auth_results, is_read, has_attachments, occurred_at, internet_message_id, case_id
		FROM public.mail_messages WHERE id::text=$1 AND tenant=$2 AND user_sub=$3`, id, m.Tenant, p.Subject).
		Scan(&msg.ID, &msg.Source, &msg.Direction, &msg.Folder, &msg.From, &msg.FromName, &to, &cc, &msg.Subject, &msg.BodyText, &msg.BodyHTML,
			&msg.Status, &msg.Detail, &auth, &msg.IsRead, &msg.Attachments, &msg.At, &msg.InternetID, &msg.CaseID)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound) // also another person's message: indistinguishable on purpose
		return
	}
	_ = json.Unmarshal(to, &msg.To)
	_ = json.Unmarshal(cc, &msg.CC)
	msg.Auth = auth
	if msg.CaseID != "" {
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(m.Tenant)), msg.CaseID).Scan(&msg.CaseNumber)
	}
	if !msg.IsRead {
		_, _ = s.db.Exec(r.Context(), `UPDATE public.mail_messages SET is_read=true, updated_at=now() WHERE id=$1`, msg.ID)
		s.mailEvent(r.Context(), msg.ID, "READ", "", p.Username)
	}
	type att struct {
		ID          string `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
		Status      string `json:"status"`
	}
	atts := []att{}
	if rows, err := s.db.Query(r.Context(), `SELECT id, filename, content_type, size_bytes, status FROM public.mail_attachments WHERE message_id=$1 ORDER BY created_at`, msg.ID); err == nil {
		for rows.Next() {
			var a att
			if rows.Scan(&a.ID, &a.Filename, &a.ContentType, &a.Size, &a.Status) == nil {
				atts = append(atts, a)
			}
		}
		rows.Close()
	}
	type ev struct {
		Event  string    `json:"event"`
		Detail string    `json:"detail"`
		Actor  string    `json:"actor"`
		At     time.Time `json:"at"`
	}
	evs := []ev{}
	if rows, err := s.db.Query(r.Context(), `SELECT event, detail, actor, at FROM public.mail_message_events WHERE message_id=$1 ORDER BY at, id`, msg.ID); err == nil {
		for rows.Next() {
			var e ev
			if rows.Scan(&e.Event, &e.Detail, &e.Actor, &e.At) == nil {
				evs = append(evs, e)
			}
		}
		rows.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": msg, "attachments": atts, "events": evs})
}

// getMyMailAttachment: GET /me/mail/{id}/attachments/{aid} -- the file, unsealed.
func (s *server) getMyMailAttachment(w http.ResponseWriter, r *http.Request) {
	m, p, err := s.myMailbox(r)
	if err != nil {
		mailErr(w, err)
		return
	}
	var key, name, ctype string
	err = s.db.QueryRow(r.Context(), `SELECT a.object_key, a.filename, a.content_type FROM public.mail_attachments a
		JOIN public.mail_messages mm ON mm.id = a.message_id
		WHERE a.id::text=$1 AND mm.id::text=$2 AND mm.tenant=$3 AND mm.user_sub=$4 AND a.status='STORED'`,
		chi.URLParam(r, "aid"), chi.URLParam(r, "id"), m.Tenant, p.Subject).Scan(&key, &name, &ctype)
	if err != nil || key == "" {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	s.serveMailAttachment(w, r, m.Tenant, key, name, ctype)
}

// serveMailAttachment streams one stored attachment, unsealed.
func (s *server) serveMailAttachment(w http.ResponseWriter, r *http.Request, tenant, key, name, ctype string) {
	obj, err := s.docs.mc.GetObject(r.Context(), docBucket, key, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, `{"error":"store"}`, http.StatusBadGateway)
		return
	}
	defer obj.Close()
	ct, err := io.ReadAll(obj)
	if err != nil {
		http.Error(w, `{"error":"store"}`, http.StatusBadGateway)
		return
	}
	pt, err := s.vaultOpenDoc(r, tenant, key, ct)
	if err != nil {
		http.Error(w, `{"error":"unseal failed"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, strings.ReplaceAll(name, `"`, "")))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(pt)
}

type sendMailRequest struct {
	Via       string   `json:"via"` // platform (the person's own address, through Resend) | outlook
	To        []string `json:"to"`
	CC        []string `json:"cc"`
	Subject   string   `json:"subject"`
	Text      string   `json:"text"`
	InReplyTo string   `json:"in_reply_to"` // a message id from this person's history, when replying
}

// sendMyMail: POST /me/mail/send -- from the person's own address (Resend) or their connected Outlook.
func (s *server) sendMyMail(w http.ResponseWriter, r *http.Request) {
	m, p, err := s.myMailbox(r)
	if err != nil {
		mailErr(w, err)
		return
	}
	var in sendMailRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	to, err1 := cleanAddrs(in.To)
	cc, err2 := cleanAddrs(in.CC)
	if err1 != nil || err2 != nil || len(to) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "give at least one valid recipient address"})
		return
	}
	in.Subject = strings.TrimSpace(in.Subject)
	if in.Subject == "" || strings.TrimSpace(in.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a subject and a message are required"})
		return
	}
	var inReplyTo, conv string
	if in.InReplyTo != "" {
		_ = s.db.QueryRow(r.Context(), `SELECT internet_message_id, conversation_id FROM public.mail_messages WHERE id::text=$1 AND tenant=$2 AND user_sub=$3`,
			in.InReplyTo, m.Tenant, p.Subject).Scan(&inReplyTo, &conv)
	}
	source := "RESEND"
	if strings.EqualFold(in.Via, "outlook") {
		source = "OUTLOOK"
	}
	var id string
	from := m.Address
	if source == "OUTLOOK" {
		from = s.outlookStatus(r.Context(), m.Tenant, p.Subject).Email
		if from == "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "connect your Outlook account first"})
			return
		}
	}
	toJ, _ := json.Marshal(to)
	ccJ, _ := json.Marshal(cc)
	if err := s.db.QueryRow(r.Context(), `INSERT INTO public.mail_messages
		(tenant, user_sub, mailbox_id, source, direction, folder, from_addr, from_name, to_addrs, cc_addrs, subject, body_text, status, is_read, occurred_at, conversation_id)
		VALUES ($1,$2,$3,$4,'OUT','sent',$5,$6,$7,$8,$9,$10,'SENDING',true,now(),$11) RETURNING id`,
		m.Tenant, p.Subject, m.ID, source, from, p.Username, toJ, ccJ, in.Subject, in.Text, conv).Scan(&id); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.mailEvent(r.Context(), id, "SENDING", "via "+strings.ToLower(source), p.Username)
	var msgID string
	if source == "RESEND" {
		msgID, err = s.sendFromMailbox(p.Username, m.Address, to, cc, in.Subject, in.Text, inReplyTo)
		_, _ = s.db.Exec(r.Context(), `UPDATE public.mail_messages SET internet_message_id=$2 WHERE id=$1`, id, msgID)
	} else {
		err = s.outlookSend(r.Context(), m.Tenant, p.Subject, to, cc, in.Subject, in.Text)
	}
	if errors.Is(err, errMailQueued) {
		// The provider turned it away for now (its rate or daily limit): the platform's mail queue sends it again.
		_, _ = s.db.Exec(r.Context(), `UPDATE public.mail_messages SET status='QUEUED', status_detail=$2, updated_at=now() WHERE id=$1`, id, truncate(err.Error(), 500))
		s.mailEvent(r.Context(), id, "QUEUED", err.Error(), p.Username)
		s.logAudit(r.Context(), m.Tenant, "", "MAIL_QUEUED", map[string]any{"by": p.Username, "via": source, "to": to, "subject": truncate(in.Subject, 200), "message": id})
		writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "status": "QUEUED", "detail": err.Error()})
		return
	}
	if err != nil {
		_, _ = s.db.Exec(r.Context(), `UPDATE public.mail_messages SET status='FAILED', status_detail=$2, updated_at=now() WHERE id=$1`, id, truncate(err.Error(), 500))
		s.mailEvent(r.Context(), id, "FAILED", err.Error(), p.Username)
		s.logAudit(r.Context(), m.Tenant, "", "MAIL_SEND_FAILED", map[string]any{"by": p.Username, "via": source, "to": to, "subject": truncate(in.Subject, 200), "message": id, "error": truncate(err.Error(), 300)})
		writeJSON(w, http.StatusFailedDependency, map[string]string{"error": "not sent: " + err.Error(), "id": id})
		return
	}
	_, _ = s.db.Exec(r.Context(), `UPDATE public.mail_messages SET status='SENT', updated_at=now() WHERE id=$1`, id)
	s.mailEvent(r.Context(), id, "SENT", "via "+strings.ToLower(source)+" to "+strings.Join(append(append([]string{}, to...), cc...), ", "), p.Username)
	s.logAudit(r.Context(), m.Tenant, "", "MAIL_SENT", map[string]any{"by": p.Username, "via": source, "to": to, "cc": cc, "subject": truncate(in.Subject, 200), "message": id})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "SENT"})
}

// sendFromMailbox sends from a person's own address through the platform's mail relay -- paced, and queued when the
// provider turns it away for now, like every other platform mail (mailqueue.go). Returns the Message-ID it carries,
// so a reply can be threaded back to it.
func (s *server) sendFromMailbox(name, addr string, to, cc []string, subject, text, inReplyTo string) (string, error) {
	if s.cfg.SMTPHost == "" {
		return "", errors.New("sending is not configured (SMTP_HOST)")
	}
	from := fmt.Sprintf("%s <%s>", headerText(name), addr)
	msgID := messageID(addr)
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	if len(cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", strings.Join(cc, ", "))
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", headerText(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: %s\r\n", msgID)
	if inReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\nReferences: %s\r\n", inReplyTo, inReplyTo)
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	writeAlternative(&b, text)
	return msgID, s.deliver(addr, to, cc, []byte(b.String()))
}

// resendGet: a GET on Resend's API into out.
func (s *server) resendGet(ctx context.Context, path string, out any) error {
	key := s.resendKey()
	if key == "" {
		return errors.New("Resend API key not configured")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, resendAPI+path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("Resend %s: HTTP %d %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

// ---- inbound: Resend's email.received ---------------------------------------------------------------------------------

type receivedEvent struct {
	Type string `json:"type"`
	Data struct {
		EmailID     string   `json:"email_id"`
		From        string   `json:"from"`
		To          []string `json:"to"`
		CC          []string `json:"cc"`
		BCC         []string `json:"bcc"`
		ReceivedFor []string `json:"received_for"`
		Subject     string   `json:"subject"`
		MessageID   string   `json:"message_id"`
		CreatedAt   string   `json:"created_at"`
	} `json:"data"`
}

type receivedEmail struct {
	ID          string            `json:"id"`
	From        string            `json:"from"`
	To          []string          `json:"to"`
	CC          []string          `json:"cc"`
	ReplyTo     []string          `json:"reply_to"`
	Subject     string            `json:"subject"`
	HTML        *string           `json:"html"`
	Text        *string           `json:"text"`
	CreatedAt   string            `json:"created_at"`
	MessageID   string            `json:"message_id"`
	Headers     map[string]string `json:"headers"`
	Auth        json.RawMessage   `json:"authentication"`
	Attachments []struct {
		ID          string `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
	} `json:"attachments"`
}

// splitAddress: "Jane Doe <jane@x.com>" -> ("Jane Doe", "jane@x.com"); a bare address -> ("", address).
func splitAddress(v string) (name, addr string) {
	if a, err := mail.ParseAddress(strings.TrimSpace(v)); err == nil {
		return a.Name, strings.ToLower(a.Address)
	}
	return "", strings.ToLower(strings.Trim(strings.TrimSpace(v), "<>"))
}

// recipientsOnDomain: the distinct lower-case addresses among lists that are on the inbound domain.
func recipientsOnDomain(domain string, lists ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range lists {
		for _, v := range l {
			_, a := splitAddress(v)
			if strings.HasSuffix(a, "@"+domain) && !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// handleReceivedEmail files a received email under each mailbox it was addressed to. An error means "try again":
// the webhook answers non-2xx and Resend retries; a retry files nothing twice.
func (s *server) handleReceivedEmail(ctx context.Context, body []byte) (filed int, err error) {
	var ev receivedEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.Data.EmailID == "" {
		return 0, nil // not one we can use; nothing to retry
	}
	if !s.mailboxesOn() {
		return 0, nil
	}
	addrs := recipientsOnDomain(s.cfg.MailboxDomain, ev.Data.To, ev.Data.CC, ev.Data.BCC, ev.Data.ReceivedFor)
	type box struct{ id, tenant, sub, username string }
	boxes := []box{}
	for _, a := range addrs {
		var b box
		if s.db.QueryRow(ctx, `SELECT id, tenant, user_sub, username FROM public.user_mailboxes WHERE address=$1 AND status='ACTIVE'`, a).
			Scan(&b.id, &b.tenant, &b.sub, &b.username) == nil {
			boxes = append(boxes, b)
		}
	}
	if len(boxes) == 0 {
		slog.Info("received email for no mailbox", "email_id", ev.Data.EmailID, "to", addrs)
		return 0, nil
	}
	var em receivedEmail
	if err := s.resendGet(ctx, "/emails/receiving/"+ev.Data.EmailID, &em); err != nil {
		return 0, err
	}
	fromName, fromAddr := splitAddress(orWord(em.Headers["from"], orWord(em.From, ev.Data.From)))
	at, ok := parseProviderTime(orWord(em.CreatedAt, ev.Data.CreatedAt))
	if !ok {
		at = time.Now()
	}
	text, html := "", ""
	if em.Text != nil {
		text = *em.Text
	}
	if em.HTML != nil {
		html = *em.HTML
	}
	if text == "" && html != "" {
		text = htmlToText(html)
	}
	toJ, _ := json.Marshal(lowerAddrs(orList(em.To, ev.Data.To)))
	ccJ, _ := json.Marshal(lowerAddrs(orList(em.CC, ev.Data.CC)))
	auth := em.Auth
	if len(auth) == 0 {
		auth = json.RawMessage(`{}`)
	}
	conv := strings.TrimSpace(orWord(em.Headers["references"], em.Headers["in-reply-to"]))
	if i := strings.IndexByte(conv, ' '); i > 0 {
		conv = conv[:i] // the thread's first message
	}
	filedFor := map[string][]string{} // tenant -> the new rows, so the mail is filed once per program
	for _, b := range boxes {
		var id string
		err := s.db.QueryRow(ctx, `INSERT INTO public.mail_messages
			(tenant, user_sub, mailbox_id, source, direction, folder, provider_message_id, internet_message_id, conversation_id,
			 from_addr, from_name, to_addrs, cc_addrs, subject, body_text, body_html, has_attachments, status, auth_results, occurred_at)
			VALUES ($1,$2,$3,'RESEND','IN','inbox',$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'RECEIVED',$15,$16)
			ON CONFLICT (user_sub, source, provider_message_id) WHERE provider_message_id <> '' DO NOTHING RETURNING id`,
			b.tenant, b.sub, b.id, em.ID, orWord(em.MessageID, ev.Data.MessageID), conv, fromAddr, fromName, toJ, ccJ,
			orWord(em.Subject, ev.Data.Subject), text, html, len(em.Attachments) > 0, []byte(auth), at).Scan(&id)
		if err != nil {
			continue // already filed for this person
		}
		filed++
		filedFor[b.tenant] = append(filedFor[b.tenant], id)
		s.mailEvent(ctx, id, "RECEIVED", "from "+fromAddr+" to "+strings.Join(addrs, ", ")+authSummary(auth), "resend")
		for _, a := range em.Attachments {
			s.storeResendAttachment(ctx, b.tenant, b.sub, id, em.ID, a.ID, a.Filename, a.ContentType, a.Size)
		}
		_, _ = s.db.Exec(ctx, `INSERT INTO public.notifications (tenant, user_sub, type, body, link) VALUES ($1,$2,'MAIL_RECEIVED',$3,'#/inbox')`,
			b.tenant, b.sub, truncate("New email from "+orWord(fromName, fromAddr)+": "+orWord(em.Subject, "(no subject)"), 300))
	}
	refs := messageIDsIn(em.Headers["in-reply-to"] + " " + em.Headers["references"])
	for tenant, ids := range filedFor {
		s.fileInbound(ctx, inboundMail{Tenant: tenant, From: fromAddr, To: lowerAddrs(orList(em.To, ev.Data.To)), Subject: orWord(em.Subject, ev.Data.Subject),
			Body: text, MessageID: orWord(em.MessageID, ev.Data.MessageID), Refs: refs, MailboxIDs: ids, By: "resend"}, true)
	}
	return filed, nil
}

// storeResendAttachment fetches one received attachment from Resend, seals it and stores it.
func (s *server) storeResendAttachment(ctx context.Context, tenant, sub, messageID, emailID, attID, filename, ctype string, size int64) {
	status, key := "STORED", ""
	defer func() {
		_, _ = s.db.Exec(ctx, `INSERT INTO public.mail_attachments (message_id, filename, content_type, size_bytes, object_key, status) VALUES ($1,$2,$3,$4,$5,$6)`,
			messageID, orWord(filename, "attachment"), orWord(ctype, "application/octet-stream"), size, key, status)
	}()
	if size > maxMailAttachment {
		status = "TOO_LARGE"
		return
	}
	var meta struct {
		DownloadURL string `json:"download_url"`
		Size        int64  `json:"size"`
	}
	if err := s.resendGet(ctx, fmt.Sprintf("/emails/receiving/%s/attachments/%s", emailID, attID), &meta); err != nil || meta.DownloadURL == "" {
		slog.Warn("received attachment not fetched", "email_id", emailID, "attachment", attID, "err", err)
		status = "FAILED"
		return
	}
	data, err := fetchLimited(ctx, meta.DownloadURL, maxMailAttachment)
	if err != nil {
		status = "FAILED"
		return
	}
	size = int64(len(data))
	if k, err := s.sealMailAttachment(ctx, tenant, sub, messageID, attID, data); err == nil {
		key = k
	} else {
		status = "FAILED"
	}
}

// sealMailAttachment seals bytes with the vault and stores them in the documents bucket; returns the object key.
func (s *server) sealMailAttachment(ctx context.Context, tenant, sub, messageID, attID string, data []byte) (string, error) {
	key := fmt.Sprintf("%s/mailbox/%s/%s/%s.enc", tenant, sub, messageID, mailboxLocalPart(attID))
	ct, err := s.vaultSealDoc(nil, tenant, key, data)
	if err != nil {
		return "", err
	}
	_, err = s.docs.mc.PutObject(ctx, docBucket, key, bytes.NewReader(ct), int64(len(ct)), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return key, err
}

func fetchLimited(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("too large")
	}
	return b, err
}

// applyMailboxDelivery: a Resend delivery report about a message sent from a personal address.
// Mail sent over SMTP carries no Resend id at first: the first report is matched on sender, recipient and subject
// (recent, newest first), and the id is kept so later reports match it directly.
func (s *server) applyMailboxDelivery(ctx context.Context, emailID, eventType, detail, from string, to []string, subject string) bool {
	status := mailEventStatus(eventType)
	if emailID == "" || status == "" {
		return false
	}
	if _, addr := splitAddress(from); s.mailboxesOn() && strings.HasSuffix(addr, "@"+s.cfg.MailboxDomain) && len(to) > 0 {
		toJ, _ := json.Marshal(lowerAddrs(to)[:1])
		_, _ = s.db.Exec(ctx, `UPDATE public.mail_messages SET provider_message_id=$1 WHERE id = (
			SELECT id FROM public.mail_messages WHERE source='RESEND' AND direction='OUT' AND provider_message_id='' AND from_addr=$2
			   AND subject=$3 AND (to_addrs @> $4::jsonb OR cc_addrs @> $4::jsonb) AND occurred_at > now() - interval '4 days'
			 ORDER BY occurred_at DESC LIMIT 1)
			AND NOT EXISTS (SELECT 1 FROM public.mail_messages WHERE source='RESEND' AND provider_message_id=$1)`, emailID, addr, subject, string(toJ))
	}
	rows, err := s.db.Query(ctx, `SELECT id, status FROM public.mail_messages WHERE source='RESEND' AND direction='OUT' AND provider_message_id=$1`, emailID)
	if err != nil {
		return false
	}
	type hit struct{ id, cur string }
	hits := []hit{}
	for rows.Next() {
		var h hit
		if rows.Scan(&h.id, &h.cur) == nil {
			hits = append(hits, h)
		}
	}
	rows.Close()
	for _, h := range hits {
		s.mailEvent(ctx, h.id, status, detail, "resend")
		if mailStatusMayReplace(h.cur, status) {
			_, _ = s.db.Exec(ctx, `UPDATE public.mail_messages SET status=$2, status_detail=$3, updated_at=now() WHERE id=$1`, h.id, status, detail)
		}
	}
	return len(hits) > 0
}

// ---- small helpers --------------------------------------------------------------------------------------------------

var (
	htmlTag   = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]+>`)
	htmlSpace = regexp.MustCompile(`[ \t]+`)
	htmlLines = regexp.MustCompile(`\n{3,}`)
)

// htmlToText: a readable plain-text version of an HTML body, for previews and search.
func htmlToText(h string) string {
	h = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</tr>|</li>|</h[1-6]>`).ReplaceAllString(h, "\n")
	t := htmlTag.ReplaceAllString(h, "")
	for k, v := range map[string]string{"&nbsp;": " ", "&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&#39;": "'"} {
		t = strings.ReplaceAll(t, k, v)
	}
	t = htmlSpace.ReplaceAllString(t, " ")
	return strings.TrimSpace(htmlLines.ReplaceAllString(t, "\n\n"))
}

func lowerAddrs(l []string) []string {
	out := make([]string, 0, len(l))
	for _, v := range l {
		if _, a := splitAddress(v); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func orList(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

func authSummary(raw json.RawMessage) string {
	var a map[string]string
	if json.Unmarshal(raw, &a) != nil || len(a) == 0 {
		return ""
	}
	return fmt.Sprintf(" (spf %s, dkim %s, dmarc %s)", orWord(a["spf"], "-"), orWord(a["dkim"], "-"), orWord(a["dmarc"], "-"))
}
