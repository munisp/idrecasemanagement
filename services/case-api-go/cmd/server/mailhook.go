// Author: Patrick Munis at newwave
// mailhook.go — delivery reports from the mail provider, and the helpers
// that keep one mail-log row per email across its whole life.
//
// A row in correspondence_log moves through:
//
//	PENDING    held for QA approval; nothing has been sent
//	SENT       the mail server accepted it
//	DELIVERED  the provider confirmed delivery to the recipient's server
//	BOUNCED    accepted, then returned (bad address, full mailbox, blocked)
//	COMPLAINED the recipient marked it as spam
//	FAILED     the mail server refused it at send time
//	REJECTED   QA rejected the draft; it was never sent
//
// SENT/FAILED are known at send time. BOUNCED/DELIVERED/COMPLAINED only
// arrive later, from the provider's webhook (POST /api/webhooks/mail,
// Svix-signed as Resend sends them). With MAIL_WEBHOOK_SECRET unset the
// endpoint is closed: an unsigned report is never accepted.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// logCorrespondenceHeld records a QA-held draft in the mail log as PENDING,
// tied to its QA review so the same row is updated when it is decided.
func (s *server) logCorrespondenceHeld(r *http.Request, tenant, caseID, template, subject, body string, to, cc []string, actor, qaID string) {
	rcpts, _ := json.Marshal(map[string]any{"to": to, "cc": cc})
	_, _ = s.db.Exec(r.Context(), `
		INSERT INTO public.correspondence_log (tenant, case_id, direction, template, subject, body, recipients, sent_by, delivery_status, qa_review_id, status_updated_at)
		VALUES ($1,$2,'OUT',$3,$4,$5,$6,$7,'PENDING',$8, now())`,
		tenant, caseID, template, subject, body, rcpts, actor, qaID)
}

// settleHeldMail moves a QA-held row to its outcome (SENT, FAILED or
// REJECTED). Returns false when the review has no held row -- drafts created
// before held rows existed, or QA items that are not email -- so the caller
// can fall back to writing a fresh row.
func (s *server) settleHeldMail(r *http.Request, tenant, qaID, status, detail, body string, to, cc []string) bool {
	if qaID == "" {
		return false
	}
	rcpts, _ := json.Marshal(map[string]any{"to": to, "cc": cc})
	tag, err := s.db.Exec(r.Context(), `
		UPDATE public.correspondence_log
		   SET delivery_status=$3, delivery_error=nullif($4,''), body=$5, recipients=$6, status_updated_at=now()
		 WHERE tenant=$1 AND qa_review_id=$2 AND delivery_status IN ('PENDING','FAILED')`,
		tenant, qaID, status, truncate(detail, 500), body, rcpts)
	return err == nil && tag.RowsAffected() > 0
}

// verifySvix checks a Svix-style webhook signature (the scheme Resend uses):
// HMAC-SHA256 over "<id>.<timestamp>.<body>" with the base64 secret that
// follows the "whsec_" prefix. Timestamps older than five minutes are
// refused so a captured report cannot be replayed later.
func verifySvix(secret, id, ts, sigHeader string, body []byte, now time.Time) bool {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) == 0 || id == "" || ts == "" {
		return false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, part := range strings.Fields(sigHeader) {
		v, sig, ok := strings.Cut(part, ",")
		if !ok || v != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(sig)
		if err == nil && hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

// mailEventStatus maps a provider event to a mail-log status ("" = ignore).
func mailEventStatus(eventType string) string {
	switch eventType {
	case "email.bounced":
		return "BOUNCED"
	case "email.delivered":
		return "DELIVERED"
	case "email.complained":
		return "COMPLAINED"
	case "email.failed":
		return "FAILED"
	}
	return ""
}

// mailStatusMayReplace: a late or out-of-order report must not hide a worse
// outcome -- once BOUNCED or COMPLAINED, a DELIVERED report changes nothing.
func mailStatusMayReplace(current, next string) bool {
	if current == next {
		return false
	}
	if next == "DELIVERED" && (current == "BOUNCED" || current == "COMPLAINED" || current == "FAILED") {
		return false
	}
	return current == "SENT" || current == "DELIVERED" || current == "BOUNCED" || current == "FAILED" || current == "COMPLAINED"
}

type mailEvent struct {
	Type string `json:"type"`
	Data struct {
		EmailID   string   `json:"email_id"`
		To        []string `json:"to"`
		Subject   string   `json:"subject"`
		CreatedAt string   `json:"created_at"`
		Bounce    struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			SubType string `json:"subType"`
		} `json:"bounce"`
	} `json:"data"`
}

func parseProviderTime(v string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999-07", "2006-01-02 15:04:05.999999-07:00", "2006-01-02 15:04:05.999999+00"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// mailWebhook: POST /api/webhooks/mail — delivery reports from the provider.
func (s *server) mailWebhook(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MailWebhookSecret == "" {
		http.Error(w, `{"error":"mail delivery reports are not configured"}`, http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	if !verifySvix(s.cfg.MailWebhookSecret, r.Header.Get("svix-id"), r.Header.Get("svix-timestamp"),
		r.Header.Get("svix-signature"), body, time.Now()) {
		http.Error(w, `{"error":"invalid signature"}`, http.StatusUnauthorized)
		return
	}
	var ev mailEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	status := mailEventStatus(ev.Type)
	if status == "" || len(ev.Data.To) == 0 {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "type": ev.Type})
		return
	}
	detail := ""
	if status == "BOUNCED" {
		detail = strings.TrimSpace(fmt.Sprintf("%s %s: %s", ev.Data.Bounce.Type, ev.Data.Bounce.SubType, ev.Data.Bounce.Message))
		detail = strings.Trim(detail, " :")
		if detail == "" {
			detail = "returned by the recipient's mail server"
		}
	}
	updated := 0
	for _, addr := range ev.Data.To {
		// Find the mail-log row this report is about: by the provider's id
		// once a previous report has stamped it, otherwise the outbound row
		// with this subject and recipient closest in time to when the
		// provider accepted the message.
		args := []any{ev.Data.EmailID, ev.Data.Subject, strings.ToLower(addr)}
		q := `SELECT id, tenant, case_id, delivery_status, sent_by FROM public.correspondence_log
		       WHERE direction='OUT' AND delivery_status NOT IN ('PENDING','REJECTED')
		         AND ( ($1 <> '' AND provider_message_id = $1)
		            OR (provider_message_id IS NULL AND subject = $2
		                AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(
		                      coalesce(recipients->'to','[]'::jsonb) || coalesce(recipients->'cc','[]'::jsonb)) a
		                    WHERE lower(a) = $3)`
		if at, ok := parseProviderTime(ev.Data.CreatedAt); ok {
			args = append(args, at)
			q += ` AND created_at BETWEEN $4::timestamptz - interval '30 minutes' AND $4::timestamptz + interval '30 minutes') )
			       ORDER BY (provider_message_id IS NULL), abs(extract(epoch FROM created_at - $4::timestamptz)) LIMIT 1`
		} else {
			q += ` AND created_at > now() - interval '7 days') ) ORDER BY (provider_message_id IS NULL), created_at DESC LIMIT 1`
		}
		var id int64
		var tenant, caseID, current, sentBy string
		if err := s.db.QueryRow(r.Context(), q, args...).Scan(&id, &tenant, &caseID, &current, &sentBy); err != nil {
			slog.Warn("mail delivery report matched no mail-log row", "type", ev.Type, "to", addr, "subject", ev.Data.Subject, "email_id", ev.Data.EmailID)
			continue
		}
		if !mailStatusMayReplace(current, status) {
			_, _ = s.db.Exec(r.Context(), `UPDATE public.correspondence_log SET provider_message_id=coalesce(provider_message_id, nullif($2,'')) WHERE id=$1`, id, ev.Data.EmailID)
			continue
		}
		note := detail
		if note != "" && len(ev.Data.To) > 0 {
			note = addr + " — " + note
		}
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.correspondence_log
			   SET delivery_status=$2, delivery_error=nullif($3,''), provider_message_id=coalesce(provider_message_id, nullif($4,'')), status_updated_at=now()
			 WHERE id=$1`, id, status, truncate(note, 500), ev.Data.EmailID)
		updated++
		if status == "BOUNCED" || status == "COMPLAINED" {
			what := "bounced"
			if status == "COMPLAINED" {
				what = "was reported as spam by the recipient"
			}
			msg := fmt.Sprintf("Email %q to %s %s%s", ev.Data.Subject, addr, what, orDash(" — "+detail))
			if caseID != "" {
				s.logActivity(r.Context(), tenant, caseID, "EMAIL_"+status, msg)
			}
			if sentBy != "" && !strings.HasPrefix(sentBy, "system:") {
				s.notify(r, tenant, sentBy, "EMAIL_"+status, msg, "#/mail")
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "recorded", "delivery_status": status, "updated": updated})
}
