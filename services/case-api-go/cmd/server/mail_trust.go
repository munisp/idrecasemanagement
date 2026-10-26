package main

// mail_trust.go — who may put mail on a dispute. A dispute number in a subject is easy to guess (they run in order),
// so it decides only WHICH dispute a mail is about; whether the mail goes on the dispute (its timeline, its
// documents) depends on who sent it. A mail is filed onto a dispute only when it passed its sender checks (DMARC, or
// SPF and DKIM) and:
//   - its sender is authorized for the dispute: the parties' registered addresses and portal teams, the addresses an
//     organization added itself (Authorized email addresses), the address that filed it, the agency's contacts, the
//     platform's own addresses, or a sender authorized on this dispute before (by its reply code, or a person filing
//     their mail by hand); or
//   - it is a reply to a mail the platform sent that same address about the dispute (it quotes that mail's
//     Message-ID, which only its recipients have); or
//   - it carries the dispute's reply code (RC-XXXX-XXXX, printed on the platform's emails about it). A code is good
//     once: its sender is then authorized for the dispute, and a new code replaces it.
// Anything else is held, unseen by the dispute, until a person files or rejects it. Recording a party's answer
// (mail_decisions.go) asks more: the filer's own addresses -- a reply code never answers for a party.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
)

// The code's letters: no 0/O, 1/I/L, so a code read aloud or copied by hand stays right.
const replyCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

var replyCodeRE = regexp.MustCompile(`(?i)\bRC-([A-Z2-9]{4})-([A-Z2-9]{4})\b`)

func newReplyCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	out := []byte("RC-")
	for i, x := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, replyCodeAlphabet[int(x)%len(replyCodeAlphabet)])
	}
	return string(out)
}

// caseReplyCode: the dispute's current reply code, made when it has none.
func (s *server) caseReplyCode(ctx context.Context, tenant, caseID string) string {
	var code string
	if s.db.QueryRow(ctx, `SELECT code FROM public.case_reply_codes WHERE tenant=$1 AND case_id=$2 AND used_at IS NULL ORDER BY created_at DESC LIMIT 1`,
		tenant, caseID).Scan(&code) == nil {
		return code
	}
	for i := 0; i < 5; i++ {
		code = newReplyCode()
		if _, err := s.db.Exec(ctx, `INSERT INTO public.case_reply_codes (tenant, case_id, code) VALUES ($1,$2,$3)`, tenant, caseID, code); err == nil {
			return code
		}
	}
	return ""
}

// withReplyCode: the closing line of an email about a dispute -- how to answer it, and its reply code.
func (s *server) withReplyCode(ctx context.Context, tenant, caseID, body string) string {
	if caseID == "" {
		return body
	}
	code := s.caseReplyCode(ctx, tenant, caseID)
	if code == "" {
		return body
	}
	return strings.TrimRight(body, "\n") + "\n\n—\nTo answer by email, reply to this message. Writing from another address? Include this dispute's reply code: " +
		code + " (it can be used once)."
}

// replyCodeCase: the dispute a code in the text belongs to, and the code ("" when none is current).
func (s *server) replyCodeCase(ctx context.Context, tenant, text string) (caseID, code string) {
	for _, m := range replyCodeRE.FindAllStringSubmatch(text, -1) {
		c := "RC-" + strings.ToUpper(m[1]) + "-" + strings.ToUpper(m[2])
		if s.db.QueryRow(ctx, `SELECT case_id FROM public.case_reply_codes WHERE tenant=$1 AND code=$2 AND used_at IS NULL`, tenant, c).Scan(&caseID) == nil {
			return caseID, c
		}
	}
	return "", ""
}

// useReplyCode: the code is spent, its sender authorized for the dispute, and a new code made.
func (s *server) useReplyCode(ctx context.Context, tenant, caseID, code, from string) {
	tag, _ := s.db.Exec(ctx, `UPDATE public.case_reply_codes SET used_at=now(), used_by=$4 WHERE tenant=$1 AND case_id=$2 AND code=$3 AND used_at IS NULL`,
		tenant, caseID, code, from)
	if tag.RowsAffected() == 0 {
		return
	}
	s.authorizeSender(ctx, tenant, "", from, caseID, "reply code "+code, "CODE")
	s.caseReplyCode(ctx, tenant, caseID) // the next one
	s.logActivity(ctx, tenant, caseID, "REPLY_CODE_USED", fmt.Sprintf("%s used the reply code %s; their mail is accepted on this dispute from now on", from, code))
}

// authorizeSender records an address that may put mail on disputes: for an organization (org-wide) or one dispute.
func (s *server) authorizeSender(ctx context.Context, tenant, org, email, caseID, by, via string) {
	_, _ = s.db.Exec(ctx, `INSERT INTO public.mail_authorized_senders (tenant, org, email, case_id, added_by, via) VALUES ($1,$2,lower($3),$4,$5,$6)
		ON CONFLICT (tenant, org, email, case_id) DO NOTHING`, tenant, org, email, caseID, by, via)
}

// orgSenders: the addresses an organization authorized itself.
func (s *server) orgSenders(ctx context.Context, tenant, org string) []string {
	out := []string{}
	if org == "" {
		return out
	}
	rows, err := s.db.Query(ctx, `SELECT email FROM public.mail_authorized_senders WHERE tenant=$1 AND org=$2 AND case_id=''`, tenant, org)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var e string
		if rows.Scan(&e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// senderAuthorized: why this address may put mail on the dispute, or "".
func (s *server) senderAuthorized(ctx context.Context, tenant, caseID, from string) string {
	if from == "" {
		return ""
	}
	if s.mailboxesOn() && strings.HasSuffix(from, "@"+s.cfg.MailboxDomain) {
		return "an address of this platform"
	}
	_, _, _, filer := s.partyAddresses(ctx, tenant, caseID) // the filer's contact, requester, portal team, added addresses
	if filer[from] {
		return "the filing party's address"
	}
	other := s.sideContact(ctx, tenant, caseID, otherSide(s.filerSide(ctx, tenant, caseID)))
	if _, a := splitAddress(other.email); a == from {
		return "the other party's address"
	}
	for _, e := range s.orgSenders(ctx, tenant, other.org) {
		if e == from {
			return "an address the other party authorized"
		}
	}
	var n int
	_ = s.db.QueryRow(ctx, `SELECT count(*) FROM public.mail_authorized_senders WHERE tenant=$1 AND case_id=$2 AND email=$3`, tenant, caseID, from).Scan(&n)
	if n > 0 {
		return "a sender authorized on this dispute"
	}
	var agency []string
	var raw []byte
	if s.db.QueryRow(ctx, `SELECT coalesce(config->'correspondence'->'agency_recipients','[]'::jsonb) FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&raw) == nil {
		_ = json.Unmarshal(raw, &agency)
		for _, a := range agency {
			if strings.EqualFold(strings.TrimSpace(a), from) {
				return "the agency's address"
			}
		}
	}
	return ""
}

func (s *server) filerSide(ctx context.Context, tenant, caseID string) string {
	side := "PROVIDER"
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT CASE WHEN upper(coalesce(details->>'filing_party_type','')) = 'HEALTH_PLAN' THEN 'HEALTH_PLAN' ELSE 'PROVIDER' END
		FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&side)
	return side
}

// genuineReply: the mail answers one the platform sent this address about the dispute.
func (s *server) genuineReply(ctx context.Context, tenant, caseID, from string, refs []string) bool {
	if len(refs) == 0 || from == "" {
		return false
	}
	var ok bool
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.correspondence_log c WHERE c.tenant=$1 AND c.case_id=$2 AND c.direction='OUT'
		AND (c.internet_message_id = ANY($3) OR c.provider_internet_id = ANY($3))
		AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(coalesce(c.recipients->'to','[]'::jsonb) || coalesce(c.recipients->'cc','[]'::jsonb)) a WHERE lower(a) = $4))`,
		tenant, caseID, refs, from).Scan(&ok)
	return ok
}

// mailTrust: whether a mail about this dispute may go on it, and why (or why not).
func (s *server) mailTrust(ctx context.Context, in inboundMail, caseID string) (bool, string) {
	_, from := splitAddress(in.From)
	var authRaw []byte
	if len(in.MailboxIDs) > 0 {
		_ = s.db.QueryRow(ctx, `SELECT auth_results FROM public.mail_messages WHERE id::text=$1`, in.MailboxIDs[0]).Scan(&authRaw)
	}
	if !mailPassedChecks(authResults(authRaw)) {
		return false, "its sender checks (DMARC / SPF and DKIM) did not pass"
	}
	if why := s.senderAuthorized(ctx, in.Tenant, caseID, from); why != "" {
		return true, why
	}
	if s.genuineReply(ctx, in.Tenant, caseID, from, in.Refs) {
		return true, "a reply to the platform's mail to this address"
	}
	if cid, code := s.replyCodeCase(ctx, in.Tenant, in.Subject+"\n"+replyText(in.Body)); cid == caseID && code != "" {
		s.useReplyCode(ctx, in.Tenant, caseID, code, from)
		return true, "it carries the dispute's reply code"
	}
	return false, from + " is not an authorized sender for this dispute"
}

// ---- an organization's authorized email addresses (TPA and health plan portals) -----------------------------------

// orgSendersHTTP: GET/POST/DELETE /tpa|hp/authorized-senders -- the addresses that may email and answer for the
// organization, beside its registered address and its portal team. A TPA's admins manage them; any plan user.
func (s *server) orgSendersHTTP(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"not linked"}`, http.StatusNotFound)
			return
		}
		org := py.org()
		p := r.Context().Value(ctxPrincipal{}).(principal)
		mayEdit := kind == "hp" || s.tpaTeamAdmin(r, tenant, py.id)
		switch r.Method {
		case http.MethodPost, http.MethodDelete:
			if !mayEdit {
				http.Error(w, `{"error":"only your organization's admins manage its authorized addresses"}`, http.StatusForbidden)
				return
			}
			var in struct {
				Email string `json:"email"`
			}
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in)
			a, perr := mail.ParseAddress(strings.TrimSpace(in.Email))
			if perr != nil || !strings.Contains(a.Address, ".") {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "give an email address, e.g. claims@yourcompany.com"})
				return
			}
			addr := strings.ToLower(a.Address)
			if r.Method == http.MethodPost {
				s.authorizeSender(r.Context(), tenant, org, addr, "", p.Username, strings.ToUpper(py.kind))
				s.logAudit(r.Context(), tenant, "", "MAIL_SENDER_AUTHORIZED", map[string]any{"org": org, "email": addr, "by": p.Username})
			} else {
				_, _ = s.db.Exec(r.Context(), `DELETE FROM public.mail_authorized_senders WHERE tenant=$1 AND org=$2 AND email=$3 AND case_id=''`, tenant, org, addr)
				s.logAudit(r.Context(), tenant, "", "MAIL_SENDER_REMOVED", map[string]any{"org": org, "email": addr, "by": p.Username})
			}
		}
		rows, err := s.queryRows(r, `SELECT email, added_by, created_at FROM public.mail_authorized_senders WHERE tenant=$1 AND org=$2 AND case_id='' ORDER BY email`, tenant, org)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"registered": py.email, "senders": rows, "may_edit": mayEdit})
	}
}

// caseReplyCodeHTTP: GET /tpa|hp/disputes/{caseId}/reply-code -- the code, for a party writing from another address.
func (s *server) caseReplyCodeHTTP(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"not linked"}`, http.StatusNotFound)
			return
		}
		caseID := chi.URLParam(r, "caseId")
		var mine bool
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM public.case_origin WHERE tenant=$1 AND case_id=$2 AND %s::text=$3)
			OR EXISTS(SELECT 1 FROM public.case_respondents WHERE tenant=$1 AND case_id=$2 AND %s::text=$3)`, py.originCol(), py.originCol()),
			tenant, caseID, py.id).Scan(&mine)
		if !mine {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"code": s.caseReplyCode(r.Context(), tenant, caseID)})
	}
}
