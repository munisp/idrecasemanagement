package main

// system_mailbox.go — a program's own address on the mailbox domain (e.g. cases@mail.upi.dev), set by a tenant admin.
// The platform's mail -- letters, notices, links -- is sent from it, so a reply comes back to it, and what it receives
// is filed like any inbound mail (mailfiling.go): under its dispute, or in the mail log's "not matched" section. It is
// a row in user_mailboxes owned by systemSub, so it can never take an address a staff member has, or the reverse.
// Unset, mail is sent from SMTP_FROM as before.
//
//	GET /program/system-mail   the address (staff: who mail comes from)
//	PUT /program/system-mail   {local_part, display_name} -- admin; {local_part: ""} turns it off

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const systemSub = "system"

type systemFromEntry struct {
	header, envelope, via string
	at                    time.Time
}

var systemFromCache sync.Map // tenant -> systemFromEntry; mail is sent often, the address rarely changes

// systemSendVia: how the program's mail goes out -- "platform" (its address here, through Resend) or "outlook" (its
// company Outlook mailbox).
func (s *server) systemSendVia(tenant string) string {
	s.systemFrom(tenant)
	if v, ok := systemFromCache.Load(tenant); ok {
		return v.(systemFromEntry).via
	}
	return "platform"
}

// systemFrom: the From header and envelope address the program's mail goes out with through the platform.
func (s *server) systemFrom(tenant string) (header, envelope string) {
	if v, ok := systemFromCache.Load(tenant); ok && time.Since(v.(systemFromEntry).at) < time.Minute {
		return v.(systemFromEntry).header, v.(systemFromEntry).envelope
	}
	via := "platform"
	_, envelope = splitAddress(s.cfg.SMTPFrom)
	header = s.cfg.SMTPFrom
	if s.db != nil && tenant != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var addr, name string
		if s.db.QueryRow(ctx, `SELECT address, display_name FROM public.user_mailboxes WHERE tenant=$1 AND user_sub=$2 AND status='ACTIVE'`,
			tenant, systemSub).Scan(&addr, &name) == nil && addr != "" {
			envelope = addr
			header = addr
			if name != "" {
				header = headerText(name) + " <" + addr + ">"
			}
		}
		_ = s.db.QueryRow(ctx, `SELECT send_via FROM public.program_mail_settings WHERE tenant=$1`, tenant).Scan(&via)
	}
	systemFromCache.Store(tenant, systemFromEntry{header: header, envelope: envelope, via: via, at: time.Now()})
	return header, envelope
}

func (s *server) getSystemMail(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	var addr, name, status string
	_ = s.db.QueryRow(r.Context(), `SELECT address, display_name, status FROM public.user_mailboxes WHERE tenant=$1 AND user_sub=$2`,
		tenant, systemSub).Scan(&addr, &name, &status)
	header, _ := s.systemFrom(tenant)
	o := s.outlookStatus(r.Context(), tenant, companySub)
	writeJSON(w, http.StatusOK, map[string]any{"domain": s.cfg.MailboxDomain, "address": addr, "display_name": name,
		"active": status == "ACTIVE", "sends_as": header, "default_from": s.cfg.SMTPFrom, "send_via": s.systemSendVia(tenant),
		"outlook_available": s.outlookOn(), "company_outlook": o})
}

func (s *server) setSystemMail(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN") {
		http.Error(w, `{"error":"forbidden: the program's address is set by an admin"}`, http.StatusForbidden)
		return
	}
	if !s.mailboxesOn() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no mailbox domain is set up for this platform (MAILBOX_DOMAIN)"})
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		LocalPart   string `json:"local_part"`
		DisplayName string `json:"display_name"`
		SendVia     string `json:"send_via"` // platform | outlook (the company Outlook mailbox)
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	defer systemFromCache.Delete(tenant)
	switch in.SendVia {
	case "outlook":
		if !s.outlookStatus(r.Context(), tenant, companySub).Connected {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "connect the company Outlook mailbox first (below)"})
			return
		}
		fallthrough
	case "platform":
		_, _ = s.db.Exec(r.Context(), `INSERT INTO public.program_mail_settings (tenant, send_via, updated_by) VALUES ($1,$2,$3)
			ON CONFLICT (tenant) DO UPDATE SET send_via=EXCLUDED.send_via, updated_by=EXCLUDED.updated_by, updated_at=now()`, tenant, in.SendVia, p.Username)
		s.logAudit(r.Context(), tenant, "", "SYSTEM_MAIL_SEND_VIA", map[string]any{"by": p.Username, "send_via": in.SendVia})
		if in.SendVia == "outlook" { // the platform address is optional then: replies go to the Outlook mailbox
			writeJSON(w, http.StatusOK, map[string]any{"send_via": "outlook"})
			return
		}
	}
	raw := strings.TrimSpace(strings.SplitN(in.LocalPart, "@", 2)[0])
	if raw == "" { // off: mail goes out from the platform's default address again
		_, _ = s.db.Exec(r.Context(), `UPDATE public.user_mailboxes SET status='DISABLED' WHERE tenant=$1 AND user_sub=$2`, tenant, systemSub)
		s.logAudit(r.Context(), tenant, "", "SYSTEM_MAIL_OFF", map[string]any{"by": p.Username})
		writeJSON(w, http.StatusOK, map[string]any{"active": false})
		return
	}
	local := mailboxLocalPart(raw)
	if local != strings.ToLower(raw) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "use letters, digits, dots, dashes or underscores — for example cases or fl.disputes"})
		return
	}
	addr := local + "@" + s.cfg.MailboxDomain
	name := strings.TrimSpace(in.DisplayName)
	if len(name) > 80 || strings.ContainsAny(name, "<>\"\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a display name of up to 80 characters, without < > or quotes"})
		return
	}
	var taken string
	if s.db.QueryRow(r.Context(), `SELECT username FROM public.user_mailboxes WHERE address=$1 AND NOT (tenant=$2 AND user_sub=$3)`,
		addr, tenant, systemSub).Scan(&taken) == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": addr + " already belongs to " + orWord(taken, "someone") + " — choose another"})
		return
	}
	if _, err := s.db.Exec(r.Context(), `
		INSERT INTO public.user_mailboxes (tenant, user_sub, username, address, display_name, created_by) VALUES ($1,$2,'system',$3,$4,$5)
		ON CONFLICT (tenant, user_sub) DO UPDATE SET address=EXCLUDED.address, display_name=EXCLUDED.display_name, status='ACTIVE'`,
		tenant, systemSub, addr, name, p.Username); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": addr + " is already taken — choose another"})
		return
	}
	s.logAudit(r.Context(), tenant, "", "SYSTEM_MAIL_SET", map[string]any{"by": p.Username, "address": addr, "display_name": name})
	header, _ := s.systemFrom(tenant)
	writeJSON(w, http.StatusOK, map[string]any{"active": true, "address": addr, "display_name": name, "sends_as": header})
}

// notifyCtx is notify for code without a request (inbound mail, syncs, delivery reports).
func (s *server) notifyCtx(ctx context.Context, tenant, target, typ, body, link string) {
	_, _ = s.db.Exec(ctx, `INSERT INTO public.notifications (tenant, user_sub, type, body, link) VALUES ($1,$2,$3,$4,$5)`,
		tenant, target, typ, truncate(body, 300), link)
}

// noteMailMessageID records the Message-ID a just-logged mail went out with, so a reply to it (In-Reply-To) is filed
// under its case even when its subject names none.
func (s *server) noteMailMessageID(ctx context.Context, tenant, caseID, subject, msgID string) {
	if msgID == "" {
		return
	}
	_, _ = s.db.Exec(ctx, `UPDATE public.correspondence_log SET internet_message_id=$4 WHERE id = (
		SELECT id FROM public.correspondence_log WHERE tenant=$1 AND case_id=$2 AND direction='OUT' AND subject=$3 AND internet_message_id=''
		  AND created_at > now() - interval '5 minutes' ORDER BY id DESC LIMIT 1)`, tenant, caseID, subject, msgID)
}
