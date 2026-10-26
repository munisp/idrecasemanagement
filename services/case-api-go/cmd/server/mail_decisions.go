package main

// mail_decisions.go — a party's answer by email does what the same answer in the portal does. When a mail is filed
// under a dispute (mailfiling.go) and it comes from the party the dispute is waiting on:
//   - its attachments become documents of the dispute (scanned, sealed, read by doc-intel), as an upload would;
//   - a permission to proceed, an extension agreed or declined, the documents an RFI asked for, or a withdrawal is
//     read from the words the sender wrote (not the letter they quote) and recorded through the same code the portal
//     and the Reviewer's step use -- the request closed, the workflow signalled, the right people told.
// It acts only when it can be sure: the sender is the party's registered address or one of its portal team, the mail
// passed its sender checks (DMARC, or SPF and DKIM), and the answer is unambiguous. Otherwise nothing is recorded and
// the dispute's holder is asked to read the mail and record it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// replyText: what the sender wrote, without the message they replied to (quoted lines, "On … wrote:", the forwarded
// or original header, a signature's "Sent from").
func replyText(body string) string {
	out := []string{}
	for _, ln := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, ">") {
			continue
		}
		if replyCut.MatchString(t) {
			break
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

var sentenceSplit = regexp.MustCompile(`(?:[.!]\s+|\n+)`)

var replyCut = regexp.MustCompile(`(?i)^(on .{3,200} wrote:?$|-{2,}\s*original message\s*-{2,}|_{8,}|from:\s.+|sent from my )`)

// The answers a request can take, with the words that say each one. Negations are tried first, so "we do not wish
// to proceed" is a decline, never a proceed.
var (
	declineProceed = regexp.MustCompile(`(?i)\b(do not|don't|dont|not)\s+(wish\s+to\s+|want\s+to\s+|give\s+(our\s+)?permission\s+to\s+)?(proceed|continue|go ahead)\b|\b(decline|declined|declining|refuse|refused|deny|denied|reject|rejected|cancel)\b|\bno\s+longer\s+(wish|want)\b|\bwithdraw`)
	grantProceed   = regexp.MustCompile(`(?i)\b(proceed|go ahead|approve|approved|consent|authori[sz]e|authori[sz]ed|permission\s+(is\s+)?(granted|given)|give\s+(our\s+|you\s+)?permission|we\s+agree|agreed|accept|accepted)\b`)
	declineMore    = regexp.MustCompile(`(?i)\b(do not|don't|dont|not)\s+(agree|accept|consent)\b|\b(decline|declined|disagree|refuse|refused|object|reject|rejected)\b`)
	agreeMore      = regexp.MustCompile(`(?i)\b(agree|agreed|accept|accepted|consent|approve|approved|fine|okay|ok|no objection|that works)\b`)
	withdrawWords  = regexp.MustCompile(`(?i)\b(withdraw|withdrawing|withdrawn)\b.{0,40}\b(dispute|claim|case|request|filing)\b|\b(dispute|claim|case|request|filing)\b.{0,40}\b(withdraw|withdrawn)\b`)
)

// readDecision: the answer a reply gives to a request of this kind ("PERMISSION": PROCEED | DECLINE,
// "EXTENSION": AGREE | DECLINE), and whether it is clear. Words for both answers -> not clear.
func readDecision(kind, text string) (string, bool) {
	var yes, no *regexp.Regexp
	var yesWord string
	switch kind {
	case "PERMISSION":
		yes, no, yesWord = grantProceed, declineProceed, "PROCEED"
	case "EXTENSION":
		yes, no, yesWord = agreeMore, declineMore, "AGREE"
	default:
		return "", false
	}
	// A question is not an answer: "should we proceed?" is for a person to read.
	for _, sentence := range sentenceSplit.Split(text, -1) {
		if strings.Contains(sentence, "?") && (yes.MatchString(sentence) || no.MatchString(sentence)) {
			return "", false
		}
	}
	n := no.MatchString(text)
	y := yes.MatchString(no.ReplaceAllString(text, " ")) // the words left once the declines are taken out
	switch {
	case n && !y:
		return "DECLINE", true
	case y && !n:
		return yesWord, true
	}
	return "", false
}

// mailPassedChecks: the receiving side's sender checks passed -- DMARC, or both SPF and DKIM.
func mailPassedChecks(auth map[string]string) bool {
	pass := func(k string) bool { return strings.EqualFold(auth[k], "pass") }
	return pass("dmarc") || (pass("spf") && pass("dkim"))
}

// authResults reads spf/dkim/dmarc out of a stored auth_results value (Resend's {"spf":"pass"} or {"spf":{"result":"pass"}}).
func authResults(raw []byte) map[string]string {
	out := map[string]string{}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return out
	}
	for _, k := range []string{"spf", "dkim", "dmarc"} {
		switch v := m[k].(type) {
		case string:
			out[k] = v
		case map[string]any:
			if r, _ := v["result"].(string); r != "" {
				out[k] = r
			} else if r, _ := v["status"].(string); r != "" {
				out[k] = r
			}
		}
	}
	return out
}

var authResultRE = regexp.MustCompile(`(?i)\b(spf|dkim|dmarc)=([a-z]+)`)

// authFromHeader reads the receiving server's Authentication-Results header (Outlook mail): spf=pass dkim=pass dmarc=pass.
func authFromHeader(h string) map[string]string {
	out := map[string]string{}
	for _, m := range authResultRE.FindAllStringSubmatch(h, -1) {
		k := strings.ToLower(m[1])
		if _, seen := out[k]; !seen {
			out[k] = strings.ToLower(m[2])
		}
	}
	return out
}

// partyAddresses: the addresses that may answer for the dispute's filer -- its registered contact, the requester
// who filed it, and the filer organization's portal team.
func (s *server) partyAddresses(ctx context.Context, tenant, caseID string) (side string, org string, name string, addrs map[string]bool) {
	addrs = map[string]bool{}
	side = "PROVIDER"
	var requester string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT CASE WHEN upper(coalesce(details->>'filing_party_type','')) = 'HEALTH_PLAN' THEN 'HEALTH_PLAN' ELSE 'PROVIDER' END,
		coalesce(details->>'requester_email','') FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&side, &requester)
	c := s.sideContact(ctx, tenant, caseID, side)
	org, name = c.org, c.name
	for _, a := range []string{c.email, requester} {
		if _, x := splitAddress(a); x != "" {
			addrs[x] = true
		}
	}
	if id, ok := strings.CutPrefix(org, "tpa:"); ok {
		rows, err := s.db.Query(ctx, `SELECT lower(email) FROM public.tpa_users WHERE tenant=$1 AND tpa_id::text=$2 AND coalesce(email,'') <> ''`, tenant, id)
		if err == nil {
			for rows.Next() {
				var e string
				if rows.Scan(&e) == nil {
					addrs[e] = true
				}
			}
			rows.Close()
		}
	}
	return side, org, name, addrs
}

// mailDecision is what one filed mail did on its dispute.
type mailDecision struct {
	Action string `json:"action"` // PERMISSION, EXTENSION, DOCUMENTS, WITHDRAWAL, or "" (nothing recorded)
	Answer string `json:"answer,omitempty"`
	Why    string `json:"why"`
}

// actOnFiledMail runs after a mail is filed under a dispute. mailRowID is one of its stored copies (for its
// attachments and sender checks), "" when it came through the email-to-case endpoint.
func (s *server) actOnFiledMail(ctx context.Context, in inboundMail, m caseMatch, mailRowID string) mailDecision {
	tenant, caseID := in.Tenant, m.CaseID
	r := (&http.Request{}).WithContext(ctx) // the shared steps take a request only for its context
	_, from := splitAddress(in.From)
	side, org, orgName, addrs := s.partyAddresses(ctx, tenant, caseID)
	fromParty := addrs[from]
	orgName = orgOr(orgName, from)

	// The checks the receiving side made on the sender.
	var authRaw []byte
	if mailRowID != "" {
		_ = s.db.QueryRow(ctx, `SELECT auth_results FROM public.mail_messages WHERE id::text=$1`, mailRowID).Scan(&authRaw)
	}
	trusted := mailPassedChecks(authResults(authRaw))

	// Attachments become the dispute's documents, whoever sent them: they are on the case's record either way.
	docs := s.attachmentsToCase(ctx, tenant, caseID, mailRowID, from)

	text := replyText(in.Body)
	if strings.TrimSpace(text) == "" {
		text = in.Subject
	}
	d := mailDecision{}
	ask := func(what string) mailDecision {
		d.Why = what
		holder := s.currentAssignee(ctx, tenant, caseID)
		if holder == "" {
			holder = "role:CASE_MANAGER"
		}
		s.notifyCtx(ctx, tenant, holder, "MAIL_DECISION_TO_CHECK",
			fmt.Sprintf("An email from %s on %s may answer %s — %s. Read it and record the answer.", from, m.CaseNumber, strings.ToLower(d.Action), what), "#/cases/"+caseID)
		s.logActivity(ctx, tenant, caseID, "MAIL_DECISION_TO_CHECK", fmt.Sprintf("Email from %s (%s): %s — not recorded automatically", from, orWord(in.Subject, "no subject"), what))
		d.Action = ""
		return d
	}
	recorded := func(answer, msg string) mailDecision {
		d.Answer = answer
		s.logActivity(ctx, tenant, caseID, "MAIL_DECISION_RECORDED", fmt.Sprintf("Recorded from an email from %s: %s", from, msg))
		s.logAudit(ctx, tenant, caseID, "MAIL_DECISION_RECORDED", map[string]any{"from": from, "action": d.Action, "answer": answer,
			"message_id": in.MessageID, "documents": len(docs), "subject": truncate(in.Subject, 200)})
		return d
	}

	// What the dispute is waiting on from its filer: an open portal request first, else the workflow's own step.
	type openReq struct{ id, reason string }
	var reqs []openReq
	if rows, err := s.db.Query(ctx, `SELECT id::text, reason FROM public.document_requests WHERE tenant=$1 AND case_id=$2 AND status='OPEN' ORDER BY created_at`, tenant, caseID); err == nil {
		for rows.Next() {
			var o openReq
			if rows.Scan(&o.id, &o.reason) == nil {
				reqs = append(reqs, o)
			}
		}
		rows.Close()
	}
	awaitingPermission := s.awaitingPermission(ctx, tenant, caseID)
	for _, q := range reqs {
		if q.reason == "PERMISSION" {
			awaitingPermission = true
		}
	}

	if withdrawWords.MatchString(text) {
		d.Action = "WITHDRAWAL"
		if !fromParty || !trusted {
			return ask(senderWhy(fromParty, trusted, side))
		}
		return s.withdrawByMail(ctx, r, tenant, caseID, m.CaseNumber, side, orgName, from, recorded)
	}

	// Permission to proceed with the full review (SOP step 4): the TPA's (or provider's) yes or no.
	if awaitingPermission {
		if ans, clear := readDecision("PERMISSION", text); clear || grantProceed.MatchString(text) || declineProceed.MatchString(text) {
			d.Action = "PERMISSION"
			if !clear {
				return ask("its wording gives both a yes and a no")
			}
			if !fromParty || !trusted {
				return ask(senderWhy(fromParty, trusted, side))
			}
			note := map[string]string{"PROCEED": "Permission given to proceed.", "DECLINE": "Declined to proceed."}[ans] + " (by email from " + from + ")"
			for _, q := range reqs {
				if q.reason == "PERMISSION" {
					if _, err := s.answerDocRequest(r, tenant, caseID, q.id, "email:"+from, orgName, org, ans, note, len(docs), "email"); err == nil {
						return recorded(ans, note)
					}
				}
			}
			// No portal request (a filer without a portal account answers the estimate email): the Reviewer's step.
			data := map[string]any{"granted": ans == "PROCEED", "date": time.Now().UTC().Format("2006-01-02"), "source": "email", "by": from}
			wfID := fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), m.CaseNumber)
			if _, err := s.signalCaseWorkflow(ctx, tenant, caseID, wfID, "ESTIMATE_PERMISSION", data); err != nil {
				return ask("the workflow could not be signalled (" + truncate(err.Error(), 80) + ")")
			}
			s.closePermissionRequests(ctx, tenant, caseID, "COMPLETE")
			detail, _ := json.Marshal(data)
			s.logActivity(ctx, tenant, caseID, "ESTIMATE_PERMISSION", fmt.Sprintf("Workflow signal ESTIMATE_PERMISSION delivered to %s — %s", wfID, detail))
			holder := orWord(s.currentAssignee(ctx, tenant, caseID), "role:CASE_MANAGER")
			s.notifyCtx(ctx, tenant, holder, "DOCUMENTS_RECEIVED", fmt.Sprintf("%s %s for %s by email", orgName,
				map[string]string{"PROCEED": "gave permission to proceed with the full review", "DECLINE": "declined to proceed with the full review"}[ans], m.CaseNumber), "#/cases/"+caseID)
			return recorded(ans, note)
		}
	}

	// More time asked for: agreed or declined.
	for _, q := range reqs {
		if q.reason != "EXTENSION" {
			continue
		}
		if ans, clear := readDecision("EXTENSION", text); clear || agreeMore.MatchString(text) || declineMore.MatchString(text) {
			d.Action = "EXTENSION"
			if !clear {
				return ask("its wording gives both a yes and a no")
			}
			if !fromParty || !trusted {
				return ask(senderWhy(fromParty, trusted, side))
			}
			note := map[string]string{"AGREE": "Agreed to the extra time.", "DECLINE": "Declined the extra time."}[ans] + " (by email from " + from + ")"
			if _, err := s.answerDocRequest(r, tenant, caseID, q.id, "email:"+from, orgName, org, ans, note, 0, "email"); err == nil {
				return recorded(ans, note)
			}
		}
	}

	// The documents asked for: the attachments answer the open request (an RFI's clock stops).
	if len(docs) > 0 {
		for _, q := range reqs {
			if q.reason == "PERMISSION" || q.reason == "EXTENSION" {
				continue
			}
			d.Action = "DOCUMENTS"
			if !fromParty || !trusted {
				return ask(senderWhy(fromParty, trusted, side))
			}
			for _, doc := range docs {
				_, _ = s.db.Exec(ctx, `INSERT INTO public.document_request_files (request_id, tenant, doc_id, filename, uploaded_by) VALUES ($1::uuid,$2,$3::uuid,$4,$5)`,
					q.id, tenant, doc[0], doc[1], "email:"+from)
			}
			note := fmt.Sprintf("Sent by email from %s (%d file%s)", from, len(docs), plural(len(docs)))
			if _, err := s.answerDocRequest(r, tenant, caseID, q.id, "email:"+from, orgName, org, "", note, len(docs), "email"); err == nil {
				return recorded("SUBMITTED", note)
			}
		}
	}
	if len(docs) > 0 {
		d.Why = fmt.Sprintf("%d attachment%s added to the dispute's documents", len(docs), plural(len(docs)))
	}
	return d
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func orgOr(name, addr string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return addr
}

// senderWhy: why a clear answer was not recorded on its own.
func senderWhy(fromParty, trusted bool, side string) string {
	switch {
	case !fromParty:
		return "it isn't from the " + strings.ToLower(strings.ReplaceAll(side, "_", " ")) + "'s registered address or portal team"
	case !trusted:
		return "its sender checks (DMARC / SPF and DKIM) did not pass"
	}
	return ""
}

// awaitingPermission: the Cost Estimate went out and no permission has been recorded yet.
func (s *server) awaitingPermission(ctx context.Context, tenant, caseID string) bool {
	var sent, recorded bool
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.correspondence_log WHERE tenant=$1 AND case_id=$2 AND direction='OUT'
		AND template='estimate_cost' AND delivery_status NOT IN ('PENDING','REJECTED'))`, tenant, caseID).Scan(&sent)
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.case_activities WHERE tenant=$1 AND case_id::text=$2 AND type='ESTIMATE_PERMISSION')`,
		tenant, caseID).Scan(&recorded)
	return sent && !recorded
}

// withdrawByMail: the filer asks to withdraw -- recorded as the Reviewer's step records it; the workflow then asks
// the office to confirm the withdrawal (it never closes a case on an email alone).
func (s *server) withdrawByMail(ctx context.Context, r *http.Request, tenant, caseID, caseNumber, side, orgName, from string,
	recorded func(string, string) mailDecision) mailDecision {
	var already bool
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.case_activities WHERE tenant=$1 AND case_id::text=$2 AND type='WITHDRAWAL_REQUESTED_BY_MAIL')`,
		tenant, caseID).Scan(&already)
	if already {
		return mailDecision{Action: "WITHDRAWAL", Why: "already recorded"}
	}
	by := "provider"
	if side == "HEALTH_PLAN" {
		by = "health_plan"
	}
	wfID := fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), caseNumber)
	if _, err := s.signalCaseWorkflow(ctx, tenant, caseID, wfID, "PROVIDER_WITHDRAW", map[string]any{"by": by, "source": "email", "from": from}); err != nil {
		return mailDecision{Action: "WITHDRAWAL", Why: "the workflow could not be signalled"}
	}
	day := time.Now().UTC().Format("2006-01-02")
	s.setProgramDateOn(ctx, tenant, caseID, "withdrawal_requested_at", day)
	s.setCaseDetail(ctx, tenant, caseID, "withdrawal_requested_by", by)
	s.logActivity(ctx, tenant, caseID, "WITHDRAWAL_REQUESTED_BY_MAIL", fmt.Sprintf("%s asked by email to withdraw the dispute — the office confirms it", orgName))
	holder := orWord(s.currentAssignee(ctx, tenant, caseID), "role:CASE_MANAGER")
	s.notifyCtx(ctx, tenant, holder, "WITHDRAWAL_REQUESTED", fmt.Sprintf("%s asked by email to withdraw %s — confirm the withdrawal", orgName, caseNumber), "#/cases/"+caseID)
	d := recorded("REQUESTED", "withdrawal requested — waiting for the office's confirmation")
	d.Action = "WITHDRAWAL"
	return d
}

// attachmentsToCase files a mail's stored attachments as documents of the dispute (once per mail). Returns
// [doc id, filename] per file stored.
func (s *server) attachmentsToCase(ctx context.Context, tenant, caseID, mailRowID, from string) [][2]string {
	out := [][2]string{}
	if mailRowID == "" {
		return out
	}
	rows, err := s.db.Query(ctx, `SELECT id::text, filename, content_type, object_key FROM public.mail_attachments
		WHERE message_id::text=$1 AND status='STORED' AND object_key <> '' AND coalesce(case_doc_id,'') = ''`, mailRowID)
	if err != nil {
		return out
	}
	type att struct{ id, name, ctype, key string }
	atts := []att{}
	for rows.Next() {
		var a att
		if rows.Scan(&a.id, &a.name, &a.ctype, &a.key) == nil {
			atts = append(atts, a)
		}
	}
	rows.Close()
	for _, a := range atts {
		obj, err := s.docs.mc.GetObject(ctx, docBucket, a.key, minio.GetObjectOptions{})
		if err != nil {
			continue
		}
		ct, err := io.ReadAll(obj)
		obj.Close()
		if err != nil {
			continue
		}
		raw, err := s.vaultOpenDoc(nil, tenant, a.key, ct)
		if err != nil {
			continue
		}
		docID, code, msg := s.storeCaseFile(ctx, tenant, caseID, "email:"+from, a.name, "Email — "+a.name, a.ctype, raw, "PARTY_UPLOADS")
		if code != 0 {
			slog.Info("mail attachment not added to the case", "case", caseID, "file", a.name, "why", msg)
			s.logActivity(ctx, tenant, caseID, "MAIL_ATTACHMENT_REFUSED", fmt.Sprintf("Attachment %q from %s was not added to the documents: %s", a.name, from, msg))
			continue
		}
		_, _ = s.db.Exec(ctx, `UPDATE public.mail_attachments SET case_doc_id=$2 WHERE id::text=$1`, a.id, docID)
		s.logActivity(ctx, tenant, caseID, "REQUESTED_DOCUMENT", fmt.Sprintf("%s sent a document by email: %s", from, a.name))
		out = append(out, [2]string{docID, "Email — " + a.name})
	}
	return out
}
