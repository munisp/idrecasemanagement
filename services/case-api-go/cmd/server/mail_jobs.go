package main

// mail_jobs.go — every received mail is processed by a durable workflow, one per message and program:
// InboundMailWorkflow (idre-workflows-py, task queue idre-mail) calls these steps in order, each retried with backoff:
//
//	receive     the message from its provider (Resend, or the Outlook copy the sync stored) into each recipient's inbox;
//	            its attachments listed, none stored yet
//	attachment  one attachment fetched and malware-scanned before it is kept: clean -> sealed and stored; infected ->
//	            quarantined (never stored), its recipients told; the scanner down -> the step fails and is retried
//	file        filed under its dispute or in the mail log's "not matched" section (mailfiling.go); attachments become
//	            the dispute's documents and an answer the dispute waits on is recorded (mail_decisions.go)
//	deliver     the recipients told it arrived
//	failed      run once when a step gave up (after 3 days): the PMs are told the mail could not be processed
//
// Each step can run again and does nothing twice. If the workflow cannot be started (Temporal down), the steps run
// here, at once, so no mail waits on the workflow engine.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	enumspb "go.temporal.io/api/enums/v1"
	temporalclient "go.temporal.io/sdk/client"
)

const mailJobQueue = "idre-mail"

// errStepRetry: the step could not finish now (a provider or the scanner unavailable); the workflow retries it.
var errStepRetry = errors.New("not now")

type mailJob struct {
	ID, Tenant, Source, ProviderID, OwnerSub, Status string
	Event                                            json.RawMessage
	MailboxIDs                                       []string
	Refs                                             []string
	Filed, Delivered                                 bool
}

func (s *server) loadMailJob(ctx context.Context, tenant, id string) (mailJob, error) {
	var j mailJob
	var rows, refs []byte
	err := s.db.QueryRow(ctx, `SELECT id::text, tenant, source, provider_id, owner_sub, status, event, mailbox_ids, refs, filed, delivered
		FROM public.inbound_mail_jobs WHERE tenant=$1 AND id::text=$2`, tenant, id).
		Scan(&j.ID, &j.Tenant, &j.Source, &j.ProviderID, &j.OwnerSub, &j.Status, &j.Event, &rows, &refs, &j.Filed, &j.Delivered)
	_ = json.Unmarshal(rows, &j.MailboxIDs)
	_ = json.Unmarshal(refs, &j.Refs)
	return j, err
}

func (s *server) mailJobStep(ctx context.Context, j mailJob, step, errText string) {
	_, _ = s.db.Exec(ctx, `UPDATE public.inbound_mail_jobs SET step=$2, last_error=$3, updated_at=now() WHERE id::text=$1`, j.ID, step, truncate(errText, 500))
}

// newMailJob records a job (once per message and program) and starts its workflow; false = it already existed.
// A sender over its hourly limit (mailFlood) gets its job recorded THROTTLED: nothing is fetched or processed.
func (s *server) newMailJob(ctx context.Context, tenant, source, providerID, ownerSub, from string, event []byte, mailboxIDs []string) (string, bool) {
	if event == nil {
		event = []byte(`{}`)
	}
	_, from = splitAddress(from)
	status, why := "NEW", ""
	if source == "RESEND" {
		if why = s.mailFlood(ctx, tenant, from); why != "" {
			status = "THROTTLED"
		}
	}
	ids, _ := json.Marshal(orList(mailboxIDs, []string{}))
	var id string
	if s.db.QueryRow(ctx, `INSERT INTO public.inbound_mail_jobs (tenant, source, provider_id, owner_sub, event, mailbox_ids, from_addr, status, last_error)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (tenant, source, owner_sub, provider_id) DO NOTHING RETURNING id::text`,
		tenant, source, providerID, ownerSub, event, ids, from, status, why).Scan(&id) != nil {
		return "", false
	}
	if status == "THROTTLED" {
		return id, true
	}
	s.startMailJob(tenant, id)
	return id, true
}

// Hourly limits on received mail a program processes: per sender (lower for one it doesn't know), and in all. Mail
// past them is recorded but not fetched, scanned or filed -- a flood costs a row each, not the platform's work.
var (
	mailSenderHourly      = envInt("MAIL_SENDER_HOURLY_LIMIT", 20)
	mailKnownSenderHourly = envInt("MAIL_KNOWN_SENDER_HOURLY_LIMIT", 100)
	mailTenantHourly      = envInt("MAIL_TENANT_HOURLY_LIMIT", 1000)
)

// mailFlood: why this sender's mail is not processed now, or "".
func (s *server) mailFlood(ctx context.Context, tenant, from string) string {
	var bySender, all int
	_ = s.db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE from_addr=$2), count(*) FROM public.inbound_mail_jobs
		WHERE tenant=$1 AND source='RESEND' AND created_at > now() - interval '1 hour'`, tenant, from).Scan(&bySender, &all)
	limit := mailSenderHourly
	if s.knownSender(ctx, tenant, from) {
		limit = mailKnownSenderHourly
	}
	why := ""
	switch {
	case all >= mailTenantHourly:
		why = fmt.Sprintf("more than %d emails reached the program in the last hour", mailTenantHourly)
	case bySender >= limit:
		why = fmt.Sprintf("%s sent more than %d emails in the last hour", orWord(from, "a sender"), limit)
	default:
		return ""
	}
	var told bool
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.notifications WHERE tenant=$1 AND type='MAIL_FLOOD' AND body LIKE '%'||$2||'%'
		AND created_at > now() - interval '1 hour')`, tenant, orWord(from, "a sender")).Scan(&told)
	if !told {
		s.notifyCtx(ctx, tenant, "role:TENANT_ADMIN", "MAIL_FLOOD", "Email is arriving too fast: "+why+". Further mail from them is recorded but not processed for now.", "#/mail")
		s.logAudit(ctx, tenant, "", "MAIL_FLOOD", map[string]any{"from": from, "why": why})
	}
	return why
}

// knownSender: an address the program already deals with -- its own, a party's contact or portal team, or one an
// organization or a person authorized.
func (s *server) knownSender(ctx context.Context, tenant, from string) bool {
	if from == "" {
		return false
	}
	if s.mailboxesOn() && strings.HasSuffix(from, "@"+s.cfg.MailboxDomain) {
		return true
	}
	var ok bool
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.tpa_users WHERE tenant=$1 AND lower(email)=$2)
		OR EXISTS(SELECT 1 FROM public.tpas WHERE tenant=$1 AND lower(contact_email)=$2)
		OR EXISTS(SELECT 1 FROM public.health_plans WHERE tenant=$1 AND lower(contact_email)=$2)
		OR EXISTS(SELECT 1 FROM public.mail_authorized_senders WHERE tenant=$1 AND email=$2)`, tenant, from).Scan(&ok)
	return ok
}

// startMailJob hands the job to its workflow, or -- the engine unavailable -- runs its steps here.
func (s *server) startMailJob(tenant, id string) {
	if s.tc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := s.tc.ExecuteWorkflow(ctx, temporalclient.StartWorkflowOptions{ID: "MAIL-" + id, TaskQueue: mailJobQueue,
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE},
			"InboundMailWorkflow", map[string]any{"tenant": tenant, "job_id": id})
		cancel()
		if err == nil {
			return
		}
		slog.Warn("inbound mail workflow not started; processing the mail here", "job", id, "err", err)
	}
	go s.runMailJobHere(tenant, id)
}

// runMailJobHere: the workflow's steps, in order, in this process (a step that fails is tried a few times).
func (s *server) runMailJobHere(tenant, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	try := func(f func() error) error {
		var err error
		for i := 0; i < 4; i++ {
			if err = f(); err == nil || !errors.Is(err, errStepRetry) {
				return err
			}
			time.Sleep(time.Duration(5*(i+1)) * time.Second)
		}
		return err
	}
	var atts []string
	if err := try(func() (e error) { atts, e = s.mailReceive(ctx, tenant, id); return }); err != nil {
		s.mailFailed(ctx, tenant, id, "receive: "+err.Error())
		return
	}
	for _, a := range atts {
		if err := try(func() error { return s.mailAttachment(ctx, tenant, id, a) }); err != nil {
			s.mailFailed(ctx, tenant, id, "attachment: "+err.Error())
			return
		}
	}
	if err := try(func() error { return s.mailFile(ctx, tenant, id) }); err != nil {
		s.mailFailed(ctx, tenant, id, "file: "+err.Error())
		return
	}
	_ = s.mailDeliver(ctx, tenant, id)
}

// ---- receiving: the webhook records a job per program -----------------------------------------------------------------

// enqueueReceivedEmail: Resend's email.received -> one job per program whose addresses it reached. Returns how many
// jobs were started (0 for a retry of one already recorded, or mail for no address of ours).
func (s *server) enqueueReceivedEmail(ctx context.Context, body []byte) (int, error) {
	var ev receivedEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.Data.EmailID == "" || !s.mailboxesOn() {
		return 0, nil
	}
	tenants := map[string][]string{}
	for _, a := range recipientsOnDomain(s.cfg.MailboxDomain, ev.Data.To, ev.Data.CC, ev.Data.BCC, ev.Data.ReceivedFor) {
		var tenant, id string
		if s.db.QueryRow(ctx, `SELECT tenant, id::text FROM public.user_mailboxes WHERE address=$1 AND status='ACTIVE'`, a).Scan(&tenant, &id) == nil {
			tenants[tenant] = append(tenants[tenant], id)
		}
	}
	if len(tenants) == 0 {
		slog.Info("received email for no mailbox", "email_id", ev.Data.EmailID)
		return 0, nil
	}
	started := 0
	for tenant := range tenants {
		if _, ok := s.newMailJob(ctx, tenant, "RESEND", ev.Data.EmailID, "", ev.Data.From, body, nil); ok {
			started++
		}
	}
	return started, nil
}

// mailReceive: the message into each recipient's inbox (once), its attachments listed as PENDING. Returns the
// attachment ids still to fetch.
func (s *server) mailReceive(ctx context.Context, tenant, id string) ([]string, error) {
	j, err := s.loadMailJob(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	s.mailJobStep(ctx, j, "receive", "")
	if j.Source == "OUTLOOK" {
		return s.outlookReceive(ctx, j)
	}
	var ev receivedEvent
	_ = json.Unmarshal(j.Event, &ev)
	var em receivedEmail
	if err := s.resendGet(ctx, "/emails/receiving/"+ev.Data.EmailID, &em); err != nil {
		if strings.Contains(err.Error(), "HTTP 404") { // Resend has no such message: retrying will not make one
			return nil, fmt.Errorf("Resend has no message %s", ev.Data.EmailID)
		}
		return nil, fmt.Errorf("%w: %v", errStepRetry, err)
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
	addrs := recipientsOnDomain(s.cfg.MailboxDomain, ev.Data.To, ev.Data.CC, ev.Data.BCC, ev.Data.ReceivedFor)
	rowIDs := []string{}
	for _, a := range addrs {
		var boxID, sub string
		if s.db.QueryRow(ctx, `SELECT id::text, user_sub FROM public.user_mailboxes WHERE address=$1 AND tenant=$2 AND status='ACTIVE'`, a, tenant).Scan(&boxID, &sub) != nil {
			continue
		}
		var row string
		err := s.db.QueryRow(ctx, `INSERT INTO public.mail_messages
			(tenant, user_sub, mailbox_id, source, direction, folder, provider_message_id, internet_message_id, conversation_id,
			 from_addr, from_name, to_addrs, cc_addrs, subject, body_text, body_html, has_attachments, status, auth_results, occurred_at)
			VALUES ($1,$2,$3,'RESEND','IN','inbox',$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'RECEIVED',$15,$16)
			ON CONFLICT (user_sub, source, provider_message_id) WHERE provider_message_id <> '' DO NOTHING RETURNING id::text`,
			tenant, sub, boxID, em.ID, orWord(em.MessageID, ev.Data.MessageID), conv, fromAddr, fromName, toJ, ccJ,
			orWord(em.Subject, ev.Data.Subject), text, html, len(em.Attachments) > 0, []byte(auth), at).Scan(&row)
		if err != nil { // stored by an earlier try of this step
			if s.db.QueryRow(ctx, `SELECT id::text FROM public.mail_messages WHERE user_sub=$1 AND source='RESEND' AND provider_message_id=$2`, sub, em.ID).Scan(&row) != nil {
				continue
			}
		} else {
			s.mailEvent(ctx, row, "RECEIVED", "from "+fromAddr+" to "+strings.Join(addrs, ", ")+authSummary(auth), "resend")
		}
		rowIDs = append(rowIDs, row)
		for _, a := range em.Attachments {
			_, _ = s.db.Exec(ctx, `INSERT INTO public.mail_attachments (message_id, filename, content_type, size_bytes, status, provider_ref)
				VALUES ($1,$2,$3,$4,'PENDING',$5) ON CONFLICT (message_id, provider_ref) WHERE provider_ref <> '' DO NOTHING`,
				row, orWord(a.Filename, "attachment"), orWord(a.ContentType, "application/octet-stream"), a.Size, em.ID+"/"+a.ID)
		}
	}
	refs := messageIDsIn(em.Headers["in-reply-to"] + " " + em.Headers["references"])
	idsJ, _ := json.Marshal(rowIDs)
	refsJ, _ := json.Marshal(refs)
	_, _ = s.db.Exec(ctx, `UPDATE public.inbound_mail_jobs SET mailbox_ids=$2, refs=$3, status='RECEIVED', updated_at=now() WHERE id::text=$1`, id, idsJ, refsJ)
	return s.pendingAttachments(ctx, rowIDs)
}

func (s *server) pendingAttachments(ctx context.Context, rowIDs []string) ([]string, error) {
	out := []string{}
	rows, err := s.db.Query(ctx, `SELECT id::text FROM public.mail_attachments WHERE message_id::text = ANY($1) AND status='PENDING' ORDER BY created_at`, rowIDs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errStepRetry, err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if rows.Scan(&a) == nil {
			out = append(out, a)
		}
	}
	return out, nil
}

// outlookReceive: the sync stored the message; its attachments are listed from Graph.
func (s *server) outlookReceive(ctx context.Context, j mailJob) ([]string, error) {
	if len(j.MailboxIDs) == 0 {
		return nil, nil
	}
	var hasAtt bool
	_ = s.db.QueryRow(ctx, `SELECT has_attachments FROM public.mail_messages WHERE id::text=$1`, j.MailboxIDs[0]).Scan(&hasAtt)
	if hasAtt {
		token, err := s.outlookAccess(ctx, j.Tenant, j.OwnerSub)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errStepRetry, err)
		}
		var res struct {
			Value []struct {
				Type        string `json:"@odata.type"`
				ID          string `json:"id"`
				Name        string `json:"name"`
				ContentType string `json:"contentType"`
				Size        int64  `json:"size"`
			} `json:"value"`
		}
		if err := graphJSON(ctx, http.MethodGet, s.outlookBase(ctx, j.Tenant, j.OwnerSub)+"/messages/"+url.PathEscape(j.ProviderID)+"/attachments?$select=id,name,contentType,size", token, nil, &res); err != nil {
			return nil, fmt.Errorf("%w: %v", errStepRetry, err)
		}
		for _, a := range res.Value {
			status := "PENDING"
			if !strings.HasSuffix(a.Type, "fileAttachment") {
				status = "FAILED" // an attached item or a link, not a file
			}
			_, _ = s.db.Exec(ctx, `INSERT INTO public.mail_attachments (message_id, filename, content_type, size_bytes, status, provider_ref)
				VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (message_id, provider_ref) WHERE provider_ref <> '' DO NOTHING`,
				j.MailboxIDs[0], orWord(a.Name, "attachment"), orWord(a.ContentType, "application/octet-stream"), a.Size, status, a.ID)
		}
	}
	_, _ = s.db.Exec(ctx, `UPDATE public.inbound_mail_jobs SET status='RECEIVED', updated_at=now() WHERE id::text=$1`, j.ID)
	return s.pendingAttachments(ctx, j.MailboxIDs)
}

// ---- one attachment: fetched, scanned, then kept or quarantined -------------------------------------------------------

func (s *server) mailAttachment(ctx context.Context, tenant, id, attID string) error {
	j, err := s.loadMailJob(ctx, tenant, id)
	if err != nil {
		return err
	}
	var row, name, ref, status, sub string
	var size int64
	if err := s.db.QueryRow(ctx, `SELECT a.message_id::text, a.filename, a.provider_ref, a.status, a.size_bytes, m.user_sub
		FROM public.mail_attachments a JOIN public.mail_messages m ON m.id=a.message_id WHERE a.id::text=$1 AND m.tenant=$2`, attID, tenant).
		Scan(&row, &name, &ref, &status, &size, &sub); err != nil {
		return nil // gone: nothing to do
	}
	if status != "PENDING" {
		return nil // done by an earlier try
	}
	s.mailJobStep(ctx, j, "attachment", "")
	set := func(st, key string, n int64) {
		_, _ = s.db.Exec(ctx, `UPDATE public.mail_attachments SET status=$2, object_key=$3, size_bytes=$4 WHERE id::text=$1`, attID, st, key, n)
	}
	if size > maxMailAttachment {
		set("TOO_LARGE", "", size)
		return nil
	}
	data, err := s.fetchMailAttachment(ctx, j, ref)
	if err != nil {
		return err
	}
	if int64(len(data)) > maxMailAttachment {
		set("TOO_LARGE", "", int64(len(data)))
		return nil
	}
	sig, err := s.clamScan(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%w: malware scanner unavailable", errStepRetry) // never kept unscanned: wait and try again
	}
	if sig != "" {
		set("QUARANTINED", "", int64(len(data)))
		s.mailEvent(ctx, row, "ATTACHMENT_QUARANTINED", name+" — malware detected ("+sig+"); it was not kept", "scanner")
		s.logAudit(ctx, tenant, "", "MAIL_ATTACHMENT_QUARANTINED", map[string]any{"message": row, "file": name, "signature": sig})
		if sub != systemSub {
			s.notifyCtx(ctx, tenant, sub, "MAIL_ATTACHMENT_QUARANTINED", fmt.Sprintf("An attachment (%s) was removed from an email you received: it carried malware (%s)", name, sig), "#/inbox?open="+row)
		} else {
			s.notifyCtx(ctx, tenant, "role:TENANT_ADMIN", "MAIL_ATTACHMENT_QUARANTINED", fmt.Sprintf("An attachment (%s) sent to the program's address carried malware (%s) and was not kept", name, sig), "#/mail")
		}
		return nil
	}
	key, err := s.sealMailAttachment(ctx, tenant, sub, row, attID, data)
	if err != nil {
		return fmt.Errorf("%w: %v", errStepRetry, err)
	}
	set("STORED", key, int64(len(data)))
	return nil
}

func (s *server) fetchMailAttachment(ctx context.Context, j mailJob, ref string) ([]byte, error) {
	if j.Source == "OUTLOOK" {
		token, err := s.outlookAccess(ctx, j.Tenant, j.OwnerSub)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errStepRetry, err)
		}
		var a struct {
			Content string `json:"contentBytes"`
		}
		u := s.outlookBase(ctx, j.Tenant, j.OwnerSub) + "/messages/" + url.PathEscape(j.ProviderID) + "/attachments/" + url.PathEscape(ref)
		if err := graphJSON(ctx, http.MethodGet, u, token, nil, &a); err != nil {
			return nil, fmt.Errorf("%w: %v", errStepRetry, err)
		}
		data, err := base64.StdEncoding.DecodeString(a.Content)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	emailID, attID, _ := strings.Cut(ref, "/")
	var meta struct {
		DownloadURL string `json:"download_url"`
	}
	if err := s.resendGet(ctx, fmt.Sprintf("/emails/receiving/%s/attachments/%s", emailID, attID), &meta); err != nil || meta.DownloadURL == "" {
		return nil, fmt.Errorf("%w: attachment not available from Resend (%v)", errStepRetry, err)
	}
	data, err := fetchLimited(ctx, meta.DownloadURL, maxMailAttachment+1)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errStepRetry, err)
	}
	return data, nil
}

// ---- filing and delivery --------------------------------------------------------------------------------------------

func (s *server) mailFile(ctx context.Context, tenant, id string) error {
	j, err := s.loadMailJob(ctx, tenant, id)
	if err != nil || j.Filed || len(j.MailboxIDs) == 0 {
		return err
	}
	s.mailJobStep(ctx, j, "file", "")
	var from, subject, body, msgID, direction string
	var toJ []byte
	if err := s.db.QueryRow(ctx, `SELECT from_addr, subject, body_text, internet_message_id, to_addrs, direction FROM public.mail_messages WHERE id::text=$1`,
		j.MailboxIDs[0]).Scan(&from, &subject, &body, &msgID, &toJ, &direction); err != nil {
		return fmt.Errorf("%w: %v", errStepRetry, err)
	}
	if direction != "IN" { // a person's own sent mail (Outlook's Sent Items): scanned and kept, not filed as received
		_, _ = s.db.Exec(ctx, `UPDATE public.inbound_mail_jobs SET filed=true, status='FILED', updated_at=now() WHERE id::text=$1`, id)
		return nil
	}
	var to []string
	_ = json.Unmarshal(toJ, &to)
	in := inboundMail{Tenant: tenant, From: from, To: to, Subject: subject, Body: body, MessageID: msgID, Refs: j.Refs, MailboxIDs: j.MailboxIDs}
	general := true
	if j.Source == "OUTLOOK" {
		in.By, in.Owners = "outlook", []string{j.OwnerSub}
		in.NotifyUnmatched = j.OwnerSub == companySub
		general = j.OwnerSub == companySub // a person's Outlook stays theirs unless it names a dispute
	} else {
		in.By = "resend"
		rows, _ := s.db.Query(ctx, `SELECT user_sub FROM public.mail_messages WHERE id::text = ANY($1)`, j.MailboxIDs)
		for rows != nil && rows.Next() {
			var sub string
			if rows.Scan(&sub) == nil {
				if sub == systemSub {
					in.NotifyUnmatched = true
				} else {
					in.Owners = append(in.Owners, sub)
				}
			}
		}
		if rows != nil {
			rows.Close()
		}
	}
	s.fileInbound(ctx, in, general)
	_, _ = s.db.Exec(ctx, `UPDATE public.inbound_mail_jobs SET filed=true, status='FILED', updated_at=now() WHERE id::text=$1`, id)
	return nil
}

func (s *server) mailDeliver(ctx context.Context, tenant, id string) error {
	j, err := s.loadMailJob(ctx, tenant, id)
	if err != nil || j.Delivered {
		return err
	}
	if j.Source != "OUTLOOK" { // Outlook mail is announced once per sync, not per message (outlook.go)
		for _, row := range j.MailboxIDs {
			var sub, from, fromName, subject string
			if s.db.QueryRow(ctx, `SELECT user_sub, from_addr, from_name, subject FROM public.mail_messages WHERE id::text=$1`, row).
				Scan(&sub, &from, &fromName, &subject) == nil && sub != systemSub {
				s.notifyCtx(ctx, tenant, sub, "MAIL_RECEIVED", "New email from "+orWord(fromName, from)+": "+orWord(subject, "(no subject)"), "#/inbox?open="+row)
			}
		}
	}
	_, _ = s.db.Exec(ctx, `UPDATE public.inbound_mail_jobs SET delivered=true, status='DONE', step='done', last_error='', finished_at=now(), updated_at=now() WHERE id::text=$1`, id)
	return nil
}

// mailFailed: a step gave up. The mail stays where it got to; the PMs are told it needs a person.
func (s *server) mailFailed(ctx context.Context, tenant, id, why string) {
	j, err := s.loadMailJob(ctx, tenant, id)
	if err != nil || j.Status == "FAILED" {
		return
	}
	_, _ = s.db.Exec(ctx, `UPDATE public.inbound_mail_jobs SET status='FAILED', last_error=$2, finished_at=now(), updated_at=now() WHERE id::text=$1`, id, truncate(why, 500))
	s.logAudit(ctx, tenant, "", "MAIL_PROCESSING_FAILED", map[string]any{"job": id, "source": j.Source, "why": truncate(why, 300)})
	var recent bool // one notice per 10 minutes: a burst of failures is one thing to look at
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.notifications WHERE tenant=$1 AND type='MAIL_PROCESSING_FAILED' AND created_at > now() - interval '10 minutes')`, tenant).Scan(&recent)
	if !recent {
		s.notifyCtx(ctx, tenant, "role:PM", "MAIL_PROCESSING_FAILED", "A received email could not be fully processed ("+truncate(why, 120)+"). Check the mail log.", "#/mail")
	}
}

// mailJobHTTP: POST /internal/mail/jobs/{jobId}/{step}[/{attId}] -- the workflow's activities (worker token only).
// 503 = try again later; 422 = this step will never succeed (the workflow stops retrying it).
func (s *server) mailJobHTTP(w http.ResponseWriter, r *http.Request) {
	if p := r.Context().Value(ctxPrincipal{}).(principal); !hasAnyRole(p, serviceRole) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id, step := chi.URLParam(r, "jobId"), chi.URLParam(r, "step")
	var out map[string]any
	var err error
	switch step {
	case "receive":
		var atts []string
		atts, err = s.mailReceive(r.Context(), tenant, id)
		out = map[string]any{"attachments": orList(atts, []string{})}
	case "attachment":
		err = s.mailAttachment(r.Context(), tenant, id, chi.URLParam(r, "attId"))
	case "file":
		err = s.mailFile(r.Context(), tenant, id)
	case "deliver":
		err = s.mailDeliver(r.Context(), tenant, id)
	case "failed":
		var in struct {
			Why string `json:"why"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in)
		s.mailFailed(r.Context(), tenant, id, orWord(in.Why, "the workflow gave up"))
	default:
		http.Error(w, `{"error":"no such step"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		if j, e := s.loadMailJob(r.Context(), tenant, id); e == nil {
			s.mailJobStep(r.Context(), j, step, err.Error())
		}
		code := http.StatusUnprocessableEntity
		if errors.Is(err, errStepRetry) {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		out = map[string]any{}
	}
	out["status"] = "ok"
	writeJSON(w, http.StatusOK, out)
}

// envInt: a positive whole number from the environment, or def.
func envInt(name string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && n > 0 {
		return n
	}
	return def
}
