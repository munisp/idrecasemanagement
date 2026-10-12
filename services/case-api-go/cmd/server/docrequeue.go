// docrequeue.go — no uploaded document is left unread.
//
// An upload is stored first and then announced on the message bus for the
// document reader. That announcement was fire-and-forget: if it was lost
// (the bus briefly down, the topic renamed, a deploy mid-upload) nothing
// noticed. The document sat there marked "analysis queued" for ever, and a
// dispute could be decided without what it said. Three real uploads were
// found in that state.
//
// Two things close the hole. publish now checks that the bus accepted the
// event and tries again when it did not. And this sweep is the backstop for
// every other way an event can go missing: every few minutes it looks for
// documents that should have been read and have not been, announces them
// again, and — if a document still is not read after several tries — says so
// to people instead of staying silent.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

const (
	// docUnreadGrace is how long an upload may wait for its first reading
	// before it is announced again. Long enough that an ordinary queue
	// (a few slow scans ahead of it) never triggers a second announcement.
	docUnreadGrace = 30 * time.Minute
	// docRequeueGap is the least time between two announcements of one document.
	docRequeueGap = 30 * time.Minute
	// docBusyPatience: while the reader is visibly working (it finished
	// something in the last docReaderActiveWindow), a waiting document is
	// probably just behind others — a batch of fifty faxes takes hours. It is
	// left alone until it has waited this long.
	docBusyPatience       = 3 * time.Hour
	docReaderActiveWindow = 20 * time.Minute
	// docRequeueMax announcements, then people are told.
	docRequeueMax = 4
	// docRequeueWindow bounds the sweep to recent uploads: an old document
	// that was never read is a decision for a person, not a surprise re-read.
	docRequeueWindow = 30 * 24 * time.Hour
	docSweepEvery    = 5 * time.Minute
)

// requeueDecision says what to do with one unread document: "wait",
// "requeue" (announce it again) or "stall" (stop and tell people).
// Pure, so it is tested.
func requeueDecision(attempts int, unreadFor, sinceLastAttempt time.Duration, readerActive bool) string {
	if unreadFor < docUnreadGrace {
		return "wait"
	}
	if readerActive && unreadFor < docBusyPatience {
		return "wait" // behind other documents, not lost
	}
	if attempts > 0 && sinceLastAttempt < docRequeueGap {
		return "wait"
	}
	if attempts >= docRequeueMax {
		return "stall"
	}
	return "requeue"
}

type unreadDoc struct {
	id, caseID, caseNumber, objectKey, contentType, filename string
	attempts                                                 int
	unreadFor, sinceLast                                     time.Duration
}

// sweepUnreadDocuments re-announces documents the reader never read and
// flags the ones it still cannot read. One replica runs it at a time.
func (s *server) sweepUnreadDocuments() (requeued, stalled int) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return 0, 0
	}
	defer conn.Release()
	var got bool
	if conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(7411902)`).Scan(&got) != nil || !got {
		return 0, 0 // another replica is sweeping
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7411902)`) //nolint:errcheck

	var tenants []string
	rows, err := s.db.Query(ctx, `SELECT substr(schema_name, 8) FROM information_schema.schemata WHERE schema_name LIKE 'tenant\_%' ORDER BY 1`)
	if err != nil {
		return 0, 0
	}
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			tenants = append(tenants, t)
		}
	}
	rows.Close()

	for _, tenant := range tenants {
		tbl := sanitizeTenant(tenant)
		var readerActive bool
		_ = s.db.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM public.doc_analysis WHERE tenant=$1 AND status <> 'QUEUED' AND status <> 'STALLED'
			              AND coalesce(analyzed_at, created_at) > now() - make_interval(secs => $2))`,
			tenant, docReaderActiveWindow.Seconds()).Scan(&readerActive)
		// Unread: no analysis row at all, or only this sweep's own QUEUED
		// marker. Sealed and quarantined files, and files the system wrote
		// itself (generated letters and checklists), are never read.
		drows, err := s.db.Query(ctx, fmt.Sprintf(`
			SELECT d.id::text, d.case_id::text, coalesce(c.case_number,''), d.object_key, coalesce(d.content_type,''),
			       coalesce(d.filename,''), coalesce((a.result->>'requeue_attempts')::int, 0),
			       extract(epoch FROM now() - d.created_at),
			       extract(epoch FROM now() - coalesce((a.result->>'last_requeue')::timestamptz, d.created_at))
			FROM tenant_%s.documents d
			LEFT JOIN tenant_%s.cases c ON c.id = d.case_id
			LEFT JOIN public.doc_analysis a ON a.doc_id = d.id
			WHERE NOT d.sealed AND coalesce(d.scan_status,'') <> 'BLOCKED'
			  AND coalesce(d.analysis_status,'QUEUED') NOT IN ('NOT_APPLICABLE','BLOCKED')
			  AND coalesce(d.uploaded_by,'') NOT LIKE 'lettergen:%%' AND coalesce(d.uploaded_by,'') NOT LIKE 'system%%'
			  AND d.created_at > now() - make_interval(secs => $1)
			  AND (a.doc_id IS NULL OR a.status = 'QUEUED')
			ORDER BY d.created_at LIMIT 300`, tbl, tbl), docRequeueWindow.Seconds())
		if err != nil {
			continue
		}
		var docs []unreadDoc
		for drows.Next() {
			var d unreadDoc
			var unread, since float64
			if drows.Scan(&d.id, &d.caseID, &d.caseNumber, &d.objectKey, &d.contentType, &d.filename, &d.attempts, &unread, &since) == nil {
				d.unreadFor, d.sinceLast = time.Duration(unread)*time.Second, time.Duration(since)*time.Second
				docs = append(docs, d)
			}
		}
		drows.Close()
		for _, d := range docs {
			switch requeueDecision(d.attempts, d.unreadFor, d.sinceLast, readerActive) {
			case "requeue":
				if s.requeueDocument(ctx, tenant, d) {
					requeued++
				}
			case "stall":
				s.stallDocument(ctx, tenant, d)
				stalled++
			}
		}
	}
	return requeued, stalled
}

// requeueDocument announces a document again and records the attempt.
func (s *server) requeueDocument(ctx context.Context, tenant string, d unreadDoc) bool {
	err := s.publishChecked(ctx, tenant, "documents", map[string]any{
		"type": "doc.uploaded", "tenant": tenant, "case_id": d.caseID,
		"doc_id": d.id, "object_key": d.objectKey, "content_type": d.contentType,
		"sealed": false, "at": time.Now().UTC(), "requeue": d.attempts + 1,
	})
	note := map[string]any{"requeue_attempts": d.attempts + 1, "last_requeue": time.Now().UTC().Format(time.RFC3339)}
	if err != nil {
		note["last_error"] = truncate(err.Error(), 300)
	}
	nj, _ := json.Marshal(note)
	// The reader replaces this row when it finishes; until then it is the
	// record that the document is waiting and how often it has been announced.
	_, _ = s.db.Exec(ctx, `
		INSERT INTO public.doc_analysis (doc_id, tenant, case_id, status, doc_type, result)
		VALUES ($1::uuid, $2, $3::uuid, 'QUEUED', '', $4::jsonb)
		ON CONFLICT (doc_id) DO UPDATE SET result = public.doc_analysis.result || EXCLUDED.result
		WHERE public.doc_analysis.status = 'QUEUED'`, d.id, tenant, d.caseID, nj)
	if d.attempts == 0 {
		s.logActivity(ctx, tenant, d.caseID, "DOC_ANALYSIS_REQUEUED",
			fmt.Sprintf("%s had not been read %s after upload — sent to the document reader again", orWord(d.filename, "A document"), roundDur(d.unreadFor)))
	}
	slog.Warn("unread document re-announced", "tenant", tenant, "case", d.caseNumber, "doc", d.id, "attempt", d.attempts+1, "err", err)
	return err == nil
}

// stallDocument stops announcing a document the reader will not read, and
// tells the people who work the case.
func (s *server) stallDocument(ctx context.Context, tenant string, d unreadDoc) {
	res, err := s.db.Exec(ctx, `
		UPDATE public.doc_analysis SET status='STALLED',
		       result = result || jsonb_build_object('findings', jsonb_build_array(jsonb_build_object('field', null,
		         'issue', 'This document could not be read automatically after several attempts — read it by hand, or use Retry analysis')))
		WHERE doc_id=$1::uuid AND status='QUEUED'`, d.id)
	if err != nil || res.RowsAffected() == 0 {
		return
	}
	name := orWord(d.filename, "a document")
	s.logActivity(ctx, tenant, d.caseID, "DOC_ANALYSIS_STALLED",
		fmt.Sprintf("%s could not be read automatically after %d attempts. Nothing was taken from it — read it by hand, or use Retry analysis.", name, d.attempts))
	for _, role := range []string{"CASE_MANAGER", "PM"} {
		_, _ = s.db.Exec(ctx, `
			INSERT INTO public.notifications (tenant, user_sub, type, body, link) VALUES ($1,$2,'DOC_UNREAD',$3,$4)`,
			tenant, "role:"+role, fmt.Sprintf("Case %s: %s was not read by the document reader — it needs a person to look at it", orWord(d.caseNumber, d.caseID), name),
			"#/cases/"+d.caseID)
	}
	slog.Error("document could not be read after retries", "tenant", tenant, "case", d.caseNumber, "doc", d.id)
}

func roundDur(d time.Duration) string {
	if d >= 2*time.Hour {
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes()))
}

// publishChecked sends one event and reports whether the bus took it,
// trying three times. An event the bus refused is an error, not a log line.
func (s *server) publishChecked(ctx context.Context, tenant, domain string, event map[string]any) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 400 * time.Millisecond):
			}
		}
		if last = s.publishOnce(ctx, tenant, domain, event); last == nil {
			return nil
		}
	}
	return last
}
