package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Inbound mail goes where it belongs. Whatever carried it -- a staff member's own address, their Outlook, or the
// email-to-case endpoint -- a received mail that points at a dispute is filed under that dispute (its mail log and its
// timeline); one that points at none is kept in the mail log's general inbound section ("not matched to a case"),
// where staff can file it by hand. What points at a dispute, strongest first:
//   - it answers a message already filed under a dispute (In-Reply-To / References);
//   - one dispute's number is in the subject;
//   - one dispute's number is in the message.
// Several different disputes named and nothing stronger: no guess, it waits in the general section.

// A dispute number: the state, the year, a serial -- FL26-019334.
var caseNumberRE = regexp.MustCompile(`(?i)\b([A-Z]{2}\d{2}-\d{5,6})\b`)

// caseNumbersIn: the distinct dispute numbers in s, upper case, in order of appearance.
func caseNumbersIn(s string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range caseNumberRE.FindAllStringSubmatch(s, -1) {
		n := strings.ToUpper(m[1])
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// messageIDsIn: the <...> message ids in an In-Reply-To or References header.
func messageIDsIn(h string) []string {
	out := []string{}
	for _, f := range strings.Fields(h) {
		if strings.HasPrefix(f, "<") && strings.HasSuffix(f, ">") {
			out = append(out, f)
		}
	}
	return out
}

type caseMatch struct {
	CaseID     string   `json:"case_id"`
	CaseNumber string   `json:"case_number"`
	Reason     string   `json:"reason"`
	Candidates []string `json:"candidates,omitempty"` // several disputes named: the ones that exist
}

// existingCases: which of the numbers are disputes of this tenant, number -> id.
func (s *server) existingCases(ctx context.Context, tenant string, numbers []string) map[string]string {
	out := map[string]string{}
	if len(numbers) == 0 {
		return out
	}
	rows, err := s.db.Query(ctx, fmt.Sprintf(`SELECT case_number, id::text FROM tenant_%s.cases WHERE upper(case_number) = ANY($1)`, sanitizeTenant(tenant)), numbers)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n, id string
		if rows.Scan(&n, &id) == nil {
			out[strings.ToUpper(n)] = id
		}
	}
	return out
}

// matchCase decides which dispute an inbound mail belongs to, if any.
func (s *server) matchCase(ctx context.Context, tenant, hint, subject, body string, refs []string) caseMatch {
	if n := strings.ToUpper(strings.TrimSpace(hint)); n != "" {
		if id := s.existingCases(ctx, tenant, []string{n})[n]; id != "" {
			return caseMatch{CaseID: id, CaseNumber: n, Reason: "dispute number " + n + " given with the mail"}
		}
	}
	if len(refs) > 0 {
		var m caseMatch
		if s.db.QueryRow(ctx, `SELECT case_id FROM public.correspondence_log
			WHERE tenant=$1 AND case_id <> '' AND (internet_message_id = ANY($2) OR provider_internet_id = ANY($2)) ORDER BY created_at DESC LIMIT 1`, tenant, refs).Scan(&m.CaseID) == nil ||
			s.db.QueryRow(ctx, `SELECT case_id FROM public.mail_messages
			WHERE tenant=$1 AND case_id <> '' AND (internet_message_id = ANY($2) OR provider_internet_id = ANY($2)) ORDER BY occurred_at DESC LIMIT 1`, tenant, refs).Scan(&m.CaseID) == nil {
			_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), m.CaseID).Scan(&m.CaseNumber)
			m.Reason = "a reply to mail filed under " + m.CaseNumber
			return m
		}
	}
	var named []string
	for _, part := range []struct{ text, where string }{{subject, "the subject"}, {body, "the message"}} {
		found := s.existingCases(ctx, tenant, caseNumbersIn(part.text))
		switch {
		case len(found) == 1:
			for n, id := range found {
				return caseMatch{CaseID: id, CaseNumber: n, Reason: "dispute number " + n + " in " + part.where}
			}
		case len(found) > 1:
			for n := range found {
				named = append(named, n)
			}
			return caseMatch{Reason: fmt.Sprintf("names %d disputes in %s — file it by hand", len(found), part.where), Candidates: named}
		}
	}
	return caseMatch{Reason: "no dispute number or thread found"}
}

// inboundMail is one received mail, however it arrived.
type inboundMail struct {
	Tenant     string
	From       string
	To         []string
	Subject    string
	Body       string
	MessageID  string   // its own Message-ID
	Refs       []string // In-Reply-To and References
	CaseHint   string   // a dispute number the sender gave explicitly (the email-to-case endpoint)
	Owners     []string // whose inboxes it reached (they were told already)
	// Business mail -- to the program's own address, its company Outlook, email-to-case: when it matches no dispute
	// the PMs are told it waits to be filed. A staff member's personal mail is not announced to anyone else.
	NotifyUnmatched bool
	MailboxIDs []string // the mail_messages rows (one per staff member it reached), when it came to a personal address
	By         string   // what carried it: resend, outlook, email-inbound
}

// fileInbound records an inbound mail in the mail log -- under its dispute when one is found -- and adds it to that
// dispute's timeline. general=false (a person's Outlook) keeps unmatched mail private: only mail about a dispute leaves
// their own inbox. The same mail reaching several people, or arriving twice, is filed once.
func (s *server) fileInbound(ctx context.Context, in inboundMail, general bool) caseMatch {
	m := s.matchCase(ctx, in.Tenant, in.CaseHint, in.Subject, in.Body, in.Refs)
	if m.CaseID == "" && !general {
		s.linkMailboxRows(ctx, in.MailboxIDs, "", 0, "not filed: "+m.Reason)
		return m
	}
	var logID int64
	if in.MessageID != "" {
		_ = s.db.QueryRow(ctx, `SELECT id FROM public.correspondence_log WHERE tenant=$1 AND direction='IN' AND internet_message_id=$2 LIMIT 1`,
			in.Tenant, in.MessageID).Scan(&logID)
	}
	if logID == 0 {
		rcpts, _ := json.Marshal(map[string]any{"from": in.From, "to": in.To})
		if err := s.db.QueryRow(ctx, `INSERT INTO public.correspondence_log
			(tenant, case_id, direction, template, subject, body, recipients, sent_by, internet_message_id, match_reason)
			VALUES ($1,$2,'IN','inbound',$3,$4,$5,$6,$7,$8) RETURNING id`,
			in.Tenant, m.CaseID, truncate(in.Subject, 500), truncate(in.Body, 20000), rcpts, in.From, in.MessageID, m.Reason).Scan(&logID); err != nil {
			return m
		}
		if m.CaseID != "" {
			s.addInboundToTimeline(ctx, in.Tenant, m.CaseID, in.From, in.Subject, in.Body, m.Reason)
		}
		s.announceInbound(ctx, in, m)
	}
	note := "in the mail log's inbound section: " + m.Reason
	if m.CaseID != "" {
		note = "filed under " + m.CaseNumber + " (" + m.Reason + ")"
	}
	s.linkMailboxRows(ctx, in.MailboxIDs, m.CaseID, logID, note)
	return m
}

// announceInbound tells the people who should know about a newly filed mail: the dispute's holder (or the PMs when no
// one holds it), or -- for business mail that matched none -- the PMs who file it by hand.
func (s *server) announceInbound(ctx context.Context, in inboundMail, m caseMatch) {
	who := strings.TrimSpace(in.From)
	subject := orWord(strings.TrimSpace(in.Subject), "(no subject)")
	if m.CaseID != "" {
		target := s.currentAssignee(ctx, in.Tenant, m.CaseID)
		if target == "" {
			target = "role:PM"
		}
		if contains(in.Owners, target) {
			return // it reached their own inbox: they were told there
		}
		s.notifyCtx(ctx, in.Tenant, target, "MAIL_FILED_TO_CASE",
			fmt.Sprintf("Email from %s on %s: %s", who, m.CaseNumber, subject), "#/cases/"+m.CaseID)
		return
	}
	if in.NotifyUnmatched {
		s.notifyCtx(ctx, in.Tenant, "role:PM", "MAIL_NOT_MATCHED",
			fmt.Sprintf("Email from %s waits to be filed under a dispute: %s", who, subject), "#/mail?unmatched=1")
	}
}

func (s *server) addInboundToTimeline(ctx context.Context, tenant, caseID, from, subject, body, reason string) {
	_, _ = s.db.Exec(ctx, `INSERT INTO public.case_activities (tenant, case_id, type, body) VALUES ($1,$2,'EMAIL',$3)`, tenant, caseID,
		fmt.Sprintf("Email from %s — %s\n%s\n(filed automatically: %s)", from, subject, truncate(body, 400), reason))
}

func (s *server) linkMailboxRows(ctx context.Context, ids []string, caseID string, logID int64, note string) {
	for _, id := range ids {
		_, _ = s.db.Exec(ctx, `UPDATE public.mail_messages SET case_id=$2, correspondence_id=nullif($3,0), updated_at=now() WHERE id=$1`, id, caseID, logID)
		event := "NOT_MATCHED"
		if caseID != "" {
			event = "FILED_TO_CASE"
		}
		s.mailEvent(ctx, id, event, note, "system")
	}
}

// fileMailToCase: POST /correspondence/{id}/file {case_number} -- staff file an unmatched inbound mail by hand.
func (s *server) fileMailToCase(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "TENANT_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		CaseNumber string `json:"case_number"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	n := strings.ToUpper(strings.TrimSpace(in.CaseNumber))
	caseID := s.existingCases(r.Context(), tenant, []string{n})[n]
	if caseID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no dispute " + orWord(n, "(none given)") + " in this program"})
		return
	}
	var from, subject, body, current string
	id := chi.URLParam(r, "id")
	if err := s.db.QueryRow(r.Context(), `SELECT sent_by, subject, body, case_id FROM public.correspondence_log WHERE id::text=$1 AND tenant=$2 AND direction='IN'`,
		id, tenant).Scan(&from, &subject, &body, &current); err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if current != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this mail is already filed under a dispute"})
		return
	}
	reason := "filed by " + p.Username
	_, _ = s.db.Exec(r.Context(), `UPDATE public.correspondence_log SET case_id=$3, match_reason=$4 WHERE id::text=$1 AND tenant=$2`, id, tenant, caseID, reason)
	_, _ = s.db.Exec(r.Context(), `UPDATE public.mail_messages SET case_id=$2, updated_at=now() WHERE correspondence_id::text=$1`, id, caseID)
	s.addInboundToTimeline(r.Context(), tenant, caseID, from, subject, body, reason)
	s.logAudit(r.Context(), tenant, caseID, "INBOUND_MAIL_FILED", map[string]any{"by": p.Username, "mail": id, "case_number": n})
	writeJSON(w, http.StatusOK, map[string]string{"status": "filed", "case_id": caseID, "case_number": n})
}
