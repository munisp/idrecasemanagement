package main

// mail_tracker.go — every email of a dispute, tracked, on its case page.
//
// GET /cases/{caseId}/mail-tracker: each letter the case's mail journal holds (public.correspondence_log) -- what
// was sent, to whom, by whom, its delivery as the mail provider reported it (sent, delivered, bounced, failed) --
// with the drafts still at the QA gate, and the replies received. For each sent letter: the documents its links
// carried, and what the recipient did with each link (opened the download, uploaded files), from the link's own
// record and the journal's link entries.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
)

type trackedLink struct {
	Kind      string     `json:"kind"` // upload | download
	Uses      int        `json:"uses"`
	MaxUses   int        `json:"max_uses"`
	Expires   time.Time  `json:"expires_at"`
	Downloads int        `json:"downloads"`
	Uploads   int        `json:"uploads"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

func (s *server) caseMailTracker(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if outsideCaller(p) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	t := sanitizeTenant(tenant)

	// What each link was used for: downloads are journal entries by "sharebox:<token prefix>", uploads are documents
	// stored by it.
	downloads := map[string]int{}
	lastUse := map[string]time.Time{}
	if rows, err := s.db.Query(r.Context(), `SELECT substr(sent_by, 10), created_at FROM public.correspondence_log
		WHERE tenant=$1 AND case_id=$2 AND template='sharebox_download' AND sent_by LIKE 'sharebox:%'`, tenant, caseID); err == nil {
		for rows.Next() {
			var pre string
			var at time.Time
			if rows.Scan(&pre, &at) == nil {
				downloads[pre]++
				if at.After(lastUse[pre]) {
					lastUse[pre] = at
				}
			}
		}
		rows.Close()
	}
	uploads := map[string]int{}
	if rows, err := s.db.Query(r.Context(), fmt.Sprintf(`SELECT substr(uploaded_by, 10), max(created_at), count(*) FROM tenant_%s.documents
		WHERE case_id::text=$1 AND uploaded_by LIKE 'sharebox:%%' GROUP BY 1`, t), caseID); err == nil {
		for rows.Next() {
			var pre string
			var at time.Time
			var n int
			if rows.Scan(&pre, &at, &n) == nil {
				uploads[pre] = n
				if at.After(lastUse[pre]) {
					lastUse[pre] = at
				}
			}
		}
		rows.Close()
	}
	linkInfo := func(body string) []trackedLink {
		out := []trackedLink{}
		for _, m := range shareTokenRe.FindAllStringSubmatch(body, -1) {
			var l trackedLink
			if s.db.QueryRow(r.Context(), `SELECT kind, uses, max_uses, expires_at FROM public.share_links WHERE token=$1 AND tenant=$2 AND case_id=$3`,
				m[1], tenant, caseID).Scan(&l.Kind, &l.Uses, &l.MaxUses, &l.Expires) != nil {
				continue
			}
			pre := m[1][:8]
			l.Downloads, l.Uploads = downloads[pre], uploads[pre]
			if at, ok := lastUse[pre]; ok {
				l.LastUsed = &at
			}
			out = append(out, l)
		}
		return out
	}
	docNames := func(ids []string) []map[string]any {
		out := []map[string]any{}
		for _, id := range ids {
			var name string
			_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT coalesce(filename,'') FROM tenant_%s.documents WHERE id::text=$1`, t), id).Scan(&name)
			out = append(out, map[string]any{"doc_id": id, "filename": name})
		}
		return out
	}

	items := []map[string]any{}
	seenQA := map[string]bool{}
	rows, err := s.db.Query(r.Context(), `
		SELECT l.id, l.direction, coalesce(l.template,''), coalesce(l.subject,''), coalesce(l.body,''), coalesce(l.recipients::text,'{}'),
		       coalesce(l.sent_by,''), l.created_at, coalesce(l.delivery_status,''), coalesce(l.delivery_error,''), l.status_updated_at,
		       coalesce(l.qa_review_id,''), coalesce(q.drafted_by,''), coalesce(q.reviewed_by,''), q.reviewed_at, coalesce(q.status,'')
		FROM public.correspondence_log l
		LEFT JOIN public.qa_reviews q ON q.tenant=l.tenant AND q.id::text=l.qa_review_id
		WHERE l.tenant=$1 AND l.case_id=$2 AND coalesce(l.template,'') <> 'sharebox_download'
		ORDER BY l.created_at DESC LIMIT 500`, tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	type logRow struct {
		id                                                                                int64
		dir, tpl, subject, body, rcpts, by, status, derr, qa, drafter, reviewer, qaStatus string
		at                                                                                time.Time
		updated, reviewedAt                                                               *time.Time
	}
	var logs []logRow
	for rows.Next() {
		var x logRow
		if rows.Scan(&x.id, &x.dir, &x.tpl, &x.subject, &x.body, &x.rcpts, &x.by, &x.at, &x.status, &x.derr, &x.updated,
			&x.qa, &x.drafter, &x.reviewer, &x.reviewedAt, &x.qaStatus) == nil {
			logs = append(logs, x)
		}
	}
	rows.Close()
	for _, x := range logs {
		var rc struct{ To, CC []string }
		_ = json.Unmarshal([]byte(x.rcpts), &rc)
		status := x.status
		switch {
		case x.dir == "IN":
			status = "RECEIVED"
		case x.status == "PENDING" && x.qaStatus == "APPROVED":
			status = "APPROVED" // passed QA, back with the drafter to send
		case x.status == "PENDING" && x.qa != "":
			status = "IN_QA" // held at the gate (logCorrespondenceHeld), settled when approved
		case x.status == "REJECTED":
			status = "SENT_BACK"
		}
		if x.qa != "" {
			seenQA[x.qa] = true
		}
		item := map[string]any{
			"id": x.id, "direction": x.dir, "template": x.tpl, "subject": x.subject, "body": x.body,
			"to": nonNil(rc.To), "cc": nonNil(rc.CC), "by": x.by, "at": x.at, "status": status, "error": x.derr,
			"status_at": x.updated, "drafted_by": x.drafter, "approved_by": x.reviewer, "approved_at": x.reviewedAt,
		}
		if x.dir == "OUT" {
			item["links"] = linkInfo(x.body)
			item["documents"] = docNames(s.linkedDocs(r.Context(), tenant, caseID, x.body))
		}
		items = append(items, item)
	}
	// Drafts the journal doesn't hold yet (approved and back with the drafter, or sent back).
	qrows, err := s.db.Query(r.Context(), `
		SELECT id::text, coalesce(artifact,''), coalesce(subject,''), coalesce(body,''), coalesce(to_recipients::text,'[]'),
		       coalesce(cc_recipients::text,'[]'), coalesce(drafted_by,''), created_at, status, coalesce(reviewed_by,''), reviewed_at,
		       coalesce(review_note,'')
		FROM public.qa_reviews WHERE tenant=$1 AND case_id=$2 AND channel='email' AND status IN ('PENDING','APPROVED','REJECTED')`,
		tenant, caseID)
	if err == nil {
		for qrows.Next() {
			var id, tpl, subject, body, to, cc, drafter, st, reviewer, note string
			var at time.Time
			var reviewedAt *time.Time
			if qrows.Scan(&id, &tpl, &subject, &body, &to, &cc, &drafter, &at, &st, &reviewer, &reviewedAt, &note) != nil || seenQA[id] {
				continue
			}
			var toL, ccL []string
			_ = json.Unmarshal([]byte(to), &toL)
			_ = json.Unmarshal([]byte(cc), &ccL)
			items = append(items, map[string]any{
				"qa_id": id, "direction": "OUT", "template": tpl, "subject": subject, "body": body, "to": nonNil(toL), "cc": nonNil(ccL),
				"by": drafter, "at": at, "status": map[string]string{"PENDING": "IN_QA", "APPROVED": "APPROVED", "REJECTED": "SENT_BACK"}[st],
				"drafted_by": drafter, "approved_by": reviewer, "approved_at": reviewedAt, "error": note,
				"links": linkInfo(body), "documents": docNames(s.linkedDocs(r.Context(), tenant, caseID, body)),
			})
		}
		qrows.Close()
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i]["at"].(time.Time).After(items[j]["at"].(time.Time)) })
	counts := map[string]int{}
	for _, it := range items {
		counts[it["status"].(string)]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"mail": items, "counts": counts})
}
