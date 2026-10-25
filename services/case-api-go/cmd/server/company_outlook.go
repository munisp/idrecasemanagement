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
	"net/http"
	"net/mail"
	"strings"

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
