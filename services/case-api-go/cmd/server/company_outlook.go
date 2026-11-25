package main

// company_outlook.go — a program's company mailbox (e.g. idre@upi.dev, where replies to case letters arrive), connected
// by a tenant admin through Microsoft Graph. It is read like any inbound mail (mailfiling.go): mail about a dispute is
// filed under it, the rest goes to the mail log's general inbound section. The mailbox may be the admin's own sign-in
// or a shared mailbox they can open; the connection lives under the reserved owner companySub, not under the admin.
//
//	GET    /company-outlook                  the connection's state
//	GET    /company-outlook/connect?mailbox=  Microsoft's sign-in address (mailbox: a shared mailbox to read, optional)
//	POST   /company-outlook/sync             bring new mail in now
//	DELETE /company-outlook                  forget the tokens; mail already filed stays filed

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

func companyMailAdmin(p principal) bool {
	return hasAnyRole(p, "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN")
}

func (s *server) companyOutlookGuard(w http.ResponseWriter, r *http.Request) (string, principal, bool) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !companyMailAdmin(p) {
		http.Error(w, `{"error":"forbidden: the company mailbox is managed by a tenant admin"}`, http.StatusForbidden)
		return "", p, false
	}
	return r.Context().Value(ctxTenant{}).(string), p, true
}

func (s *server) companyOutlookStatus(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.companyOutlookGuard(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outlook_available": s.outlookOn(), "outlook": s.outlookStatus(r.Context(), tenant, companySub),
		"outlook_admin_consent_url": s.outlookAdminConsentURL()})
}

func (s *server) companyOutlookConnect(w http.ResponseWriter, r *http.Request) {
	tenant, p, ok := s.companyOutlookGuard(w, r)
	if !ok {
		return
	}
	if !s.outlookOn() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Outlook isn't set up for this platform yet (OUTLOOK_CLIENT_ID)"})
		return
	}
	mailbox := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mailbox")))
	if mailbox != "" {
		if a, err := mail.ParseAddress(mailbox); err != nil || a.Address != mailbox || strings.Contains(mailbox, "|") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "give the mailbox as an address, e.g. idre@upi.dev"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": s.outlookAuthorizeURL(oauthState{tenant: tenant, sub: companySub, mailbox: mailbox, by: p.Subject})})
}

func (s *server) companyOutlookSync(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := s.companyOutlookGuard(w, r)
	if !ok {
		return
	}
	n, err := s.syncOutlook(r.Context(), tenant, companySub)
	if err != nil {
		writeJSON(w, http.StatusFailedDependency, map[string]any{"error": err.Error(), "filed": n})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"filed": n, "outlook": s.outlookStatus(r.Context(), tenant, companySub)})
}

func (s *server) companyOutlookDisconnect(w http.ResponseWriter, r *http.Request) {
	tenant, p, ok := s.companyOutlookGuard(w, r)
	if !ok {
		return
	}
	_, _ = s.db.Exec(r.Context(), `UPDATE public.outlook_connections SET status='DISCONNECTED', access_token_sealed=NULL, refresh_token_sealed=NULL,
		token_expires_at=NULL, updated_at=now() WHERE tenant=$1 AND user_sub=$2`, tenant, companySub)
	s.logAudit(r.Context(), tenant, "", "COMPANY_OUTLOOK_DISCONNECTED", map[string]any{"by": p.Username})
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// ---- attachments of inbound mail in the mail log ---------------------------------------------------------------------

// mailLogAttachments: GET /correspondence/{id}/attachments -- what came with an inbound mail (the company mailbox's, or
// a staff address's), for the staff who may read the mail log.
func (s *server) mailLogAttachments(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, mailJournalRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.queryRows(r, `SELECT DISTINCT ON (a.filename, a.size_bytes) a.id, a.filename, a.content_type, a.size_bytes AS size, a.status
		FROM public.mail_attachments a JOIN public.mail_messages mm ON mm.id = a.message_id
		JOIN public.correspondence_log c ON c.id = mm.correspondence_id
		WHERE c.id::text=$1 AND c.tenant=$2 ORDER BY a.filename, a.size_bytes, a.created_at`, chi.URLParam(r, "id"), tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachments": rows})
}

func (s *server) mailLogAttachment(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, mailJournalRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var key, name, ctype string
	err := s.db.QueryRow(r.Context(), `SELECT a.object_key, a.filename, a.content_type
		FROM public.mail_attachments a JOIN public.mail_messages mm ON mm.id = a.message_id
		JOIN public.correspondence_log c ON c.id = mm.correspondence_id
		WHERE a.id::text=$1 AND c.id::text=$2 AND c.tenant=$3 AND a.status='STORED'`, chi.URLParam(r, "aid"), chi.URLParam(r, "id"), tenant).Scan(&key, &name, &ctype)
	if err != nil || key == "" {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	s.serveMailAttachment(w, r, tenant, key, name, ctype)
}

// ---- sending the program's mail from its company Outlook -------------------------------------------------------------

type outAttachment struct {
	Name, ContentType string
	Content           []byte
}

// outlookSendFrom sends one message from a connected Outlook account (a person's, or the company mailbox) and returns
// its Message-ID: the message is created as a draft first, which gives the id, then sent; Outlook keeps it in Sent Items.
func (s *server) outlookSendFrom(ctx context.Context, tenant, sub string, to, cc []string, subject, text string, atts []outAttachment) (string, error) {
	token, err := s.outlookAccess(ctx, tenant, sub)
	if err != nil {
		return "", err
	}
	rcpt := func(l []string) []map[string]any {
		out := []map[string]any{}
		for _, a := range l {
			out = append(out, map[string]any{"emailAddress": map[string]string{"address": a}})
		}
		return out
	}
	msg := map[string]any{"subject": subject, "body": map[string]string{"contentType": "Text", "content": text}, "toRecipients": rcpt(to)}
	if len(cc) > 0 {
		msg["ccRecipients"] = rcpt(cc)
	}
	if len(atts) > 0 {
		list := []map[string]any{}
		for _, a := range atts {
			list = append(list, map[string]any{"@odata.type": "#microsoft.graph.fileAttachment", "name": a.Name,
				"contentType": a.ContentType, "contentBytes": base64.StdEncoding.EncodeToString(a.Content)})
		}
		msg["attachments"] = list
	}
	base := s.outlookBase(ctx, tenant, sub)
	var draft struct {
		ID                string `json:"id"`
		InternetMessageID string `json:"internetMessageId"`
	}
	if err := graphJSON(ctx, http.MethodPost, base+"/messages", token, msg, &draft); err != nil {
		return "", err
	}
	if err := graphJSON(ctx, http.MethodPost, base+"/messages/"+url.PathEscape(draft.ID)+"/send", token, nil, nil); err != nil {
		return "", err
	}
	return draft.InternetMessageID, nil
}

var outlookFallbackNoticed sync.Map // tenant -> time the admins were last told their Outlook couldn't send

// viaCompanyOutlook sends the program's mail from its company Outlook when the tenant chose that (program_mail_settings).
// sent=false means the caller sends it through the platform: Outlook isn't chosen, or it failed -- then the mail still
// goes out, and the tenant admins are told (at most hourly) that Outlook needs attention.
func (s *server) viaCompanyOutlook(tenant string, to, cc []string, subject, body string, atts []outAttachment) (id string, sent bool) {
	if !s.outlookOn() || s.systemSendVia(tenant) != "outlook" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id, err := s.outlookSendFrom(ctx, tenant, companySub, to, cc, subject, body, atts)
	if err == nil {
		return id, true
	}
	slog.Warn("program mail not sent from Outlook; sending through the platform", "tenant", tenant, "err", err)
	if v, ok := outlookFallbackNoticed.Load(tenant); !ok || time.Since(v.(time.Time)) > time.Hour {
		outlookFallbackNoticed.Store(tenant, time.Now())
		s.notifyCtx(ctx, tenant, "role:TENANT_ADMIN", "COMPANY_OUTLOOK_SEND_FAILED",
			"The company Outlook couldn't send the program's mail ("+truncate(err.Error(), 120)+"), so it went out from the platform's address instead. Reconnect it in the mail log.", "#/mail")
	}
	return "", false
}
