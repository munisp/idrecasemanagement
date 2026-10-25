package main

// party_documents.go — the documents a letter's download link carries, and the parties' copy of them.
//
// A download link in a letter used to point at one file: the case's newest document at the time it was drafted.
// The Final Order package (cover letter, rationale and invoices) and the rationale copy to the parties each need
// several, so their links carry a named set (letterBundles), resolved to the latest of each document when the
// link is used and downloaded as one zip.
//
// When a letter goes out, every document its links carry is also recorded for the organizations on the platform
// it went to (public.party_documents, scripts/party_documents_20261023.sql): a TPA or a health plan finds them
// under the dispute in its portal, not only in its mailbox.

import (
	"archive/zip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// caseSetPrefix marks a download link that carries a named set of a case's documents (letterBundles).
const caseSetPrefix = "zip:set:"

// bundlePart: one document of a set -- the latest generated letter of that template (its object key carries
// "/<template>-"), or the latest document whose name starts with prefix (prefix takes the case number).
type bundlePart struct{ tpl, prefix string }

// letterBundles: the templates whose download link carries a set, and what is in it.
var letterBundles = map[string]struct {
	label string
	parts []bundlePart
}{
	"final_order": {"Full Review determination documents %s", []bundlePart{
		{"cover_letter_final_notice", "Cover Letter %s"},
		{"final_order_rationale", "Review Rationale and Letter %s"},
		{"invoice_health_plan", ""},
		{"invoice_provider", ""},
	}},
	"final_order_rationale_copy": {"Final Order Rationale %s", []bundlePart{
		{"final_order_rationale", "Review Rationale and Letter %s"},
		{"", "Final Order %s"}, // AHCA's own order, uploaded with the step that records it
	}},
}

type bundleDoc struct {
	id, key, name string
	parts         []byte
	size          int64
}

// bundleDocs: the documents of a set on a case, the latest of each part.
func (s *server) bundleDocs(ctx context.Context, tenant, caseID, set string) []bundleDoc {
	b, ok := letterBundles[set]
	if !ok {
		return nil
	}
	t := sanitizeTenant(tenant)
	var caseNumber string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, t), caseID).Scan(&caseNumber)
	var out []bundleDoc
	seen := map[string]bool{}
	for _, p := range b.parts {
		prefix := ""
		if p.prefix != "" {
			prefix = fmt.Sprintf(p.prefix, caseNumber)
		}
		var d bundleDoc
		// An office letter goes only once it has passed QA: its newest QA item approved or sent, or none (a
		// template the program doesn't gate). One waiting or sent back stays out -- bundleWaiting says so first.
		if s.db.QueryRow(ctx, fmt.Sprintf(`
			SELECT id::text, object_key, coalesce(filename, id::text), coalesce(parts::text,''), coalesce(size_bytes,0)
			FROM tenant_%s.documents
			WHERE case_id::text=$1 AND scan_status='CLEAN' AND NOT sealed AND NOT (content_type = 'text/html' AND left(coalesce(filename,''), 22) = 'Eligibility Checklist ')
			  AND (($2 <> '' AND object_key LIKE '%%/' || $2 || '-%%') OR ($3 <> '' AND filename ILIKE $3 || '%%'))
			  AND ($2 = '' OR coalesce((SELECT q.status FROM public.qa_reviews q WHERE q.tenant=$4 AND q.case_id::text=$1
			        AND q.channel='letter' AND q.artifact=$2 ORDER BY q.created_at DESC LIMIT 1), 'APPROVED') IN ('APPROVED','SENT'))
			ORDER BY created_at DESC LIMIT 1`, t), caseID, p.tpl, prefix, tenant).Scan(&d.id, &d.key, &d.name, &d.parts, &d.size) == nil && !seen[d.id] {
			seen[d.id] = true
			out = append(out, d)
		}
	}
	return out
}

// letterReleased: whether an office letter whose newest QA item has this status may leave the office ("" = the
// program doesn't gate the template).
func letterReleased(status string) bool {
	return status == "" || status == "APPROVED" || status == "SENT"
}

// bundleWaiting: the letters of a set that are on the case but haven't passed QA -- still waiting, or sent back
// -- by subject. While any is, the set's letter is not drafted or sent: the package would go without them.
func (s *server) bundleWaiting(ctx context.Context, tenant, caseID, set string) []string {
	var out []string
	for _, p := range letterBundles[set].parts {
		if p.tpl == "" {
			continue
		}
		var status, subject string
		if s.db.QueryRow(ctx, `SELECT status, coalesce(subject,'') FROM public.qa_reviews
			WHERE tenant=$1 AND case_id::text=$2 AND channel='letter' AND artifact=$3 ORDER BY created_at DESC LIMIT 1`,
			tenant, caseID, p.tpl).Scan(&status, &subject) == nil && !letterReleased(status) {
			what := "waiting for QA"
			if status == "REJECTED" {
				what = "sent back in QA"
			}
			out = append(out, fmt.Sprintf("%s (%s)", subject, what))
		}
	}
	return out
}

// bundleRefusal: the error a set's letter gets while some of its letters haven't passed QA; "" when none.
func bundleRefusal(waiting []string) string {
	if len(waiting) == 0 {
		return ""
	}
	return "This letter carries documents that haven't passed QA yet: " + strings.Join(waiting, "; ") +
		". Approve them in the QA gate first, so the package goes complete."
}

// bundleName: the set's zip, by what the letter calls it.
func (s *server) bundleName(ctx context.Context, tenant, caseID, set string) string {
	var caseNumber string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber)
	return fmt.Sprintf(letterBundles[set].label, caseNumber) + ".zip"
}

// writeDocsZip streams the given documents as one zip.
func (s *server) writeDocsZip(w http.ResponseWriter, r *http.Request, tenant, zipName string, docs []bundleDoc) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", zipName))
	zw := zip.NewWriter(w)
	used := map[string]int{}
	for _, d := range docs {
		name := d.name
		if n := used[name]; n > 0 {
			name = fmt.Sprintf("%s-%d", d.name, n)
		}
		used[d.name]++
		entry, err := zw.Create(name)
		if err != nil {
			continue
		}
		if err := s.writeDocPlaintext(r, tenant, d.key, d.parts, entry); err != nil {
			return // the zip is cut short; the client sees a broken download, not a wrong one
		}
	}
	zw.Close()
}

// shareTokenRe finds the secure links in a letter's text (portal /s/<token> or the API's /api/share/<token>).
var shareTokenRe = regexp.MustCompile(`(?:/s/|/api/share/)([0-9a-f]{48})\b`)

// letterSides: which sides of the dispute a letter went to -- by its template's recipients, and by the
// addresses it was actually sent to.
func (s *server) letterSides(r *http.Request, tenant, caseID, template, rfiTo string, to, cc []string) map[string]bool {
	var filer, billed string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT upper(coalesce(details->>'filing_party_type','PROVIDER')), coalesce(details->>'party_billed','')
		FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&filer, &billed)
	if filer != "HEALTH_PLAN" {
		filer = "PROVIDER"
	}
	sides := map[string]bool{}
	var roles []string
	for _, t := range s.corrTemplates(r, tenant) {
		if t.Key == template {
			roles = append(append(roles, t.To...), t.CC...)
		}
	}
	if template == "rfi" {
		roles = []string{map[bool]string{true: "health_plan", false: "provider"}[rfiTo == "plan"]}
	}
	for _, role := range roles {
		switch role {
		case "provider":
			sides["PROVIDER"] = true
		case "health_plan", "plan":
			sides["HEALTH_PLAN"] = true
		case "filing_party", "requester":
			sides[filer] = true
		case "all_parties":
			sides["PROVIDER"], sides["HEALTH_PLAN"] = true, true
		case "billed_party":
			if billed == "Provider" || billed == "Both" {
				sides["PROVIDER"] = true
			}
			if billed == "Health Plan" || billed == "Both" {
				sides["HEALTH_PLAN"] = true
			}
		}
	}
	sent := map[string]bool{}
	for _, e := range append(append([]string{}, to...), cc...) {
		sent[strings.ToLower(strings.TrimSpace(e))] = true
	}
	for _, side := range []string{"PROVIDER", "HEALTH_PLAN"} {
		if e := strings.ToLower(s.sideContact(r.Context(), tenant, caseID, side).email); e != "" && sent[e] {
			sides[side] = true
		}
	}
	return sides
}

// linkedDocs: the documents a letter's links carry.
func (s *server) linkedDocs(ctx context.Context, tenant, caseID, body string) []string {
	t := sanitizeTenant(tenant)
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, m := range shareTokenRe.FindAllStringSubmatch(body, -1) {
		var kind, key string
		if s.db.QueryRow(ctx, `SELECT kind, coalesce(object_key,'') FROM public.share_links WHERE token=$1 AND tenant=$2 AND case_id=$3`,
			m[1], tenant, caseID).Scan(&kind, &key) != nil || kind != "download" || key == "" {
			continue
		}
		switch {
		case key == caseZipKey: // the filing party's documentation, as writeCaseZip sends it
			rows, err := s.db.Query(ctx, fmt.Sprintf(`SELECT d.id::text FROM tenant_%s.documents d
				WHERE d.case_id::text=$1 AND d.scan_status='CLEAN' AND NOT d.sealed
				  AND coalesce(d.folder,'') NOT IN ('CORRESPONDENCE','DETERMINATION') AND NOT `+internalDocSQL+`
				ORDER BY d.created_at`, t), caseID)
			if err != nil {
				continue
			}
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					add(id)
				}
			}
			rows.Close()
		case strings.HasPrefix(key, caseSetPrefix):
			for _, d := range s.bundleDocs(ctx, tenant, caseID, strings.TrimPrefix(key, caseSetPrefix)) {
				add(d.id)
			}
		default:
			var id string
			_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT d.id::text FROM tenant_%s.documents d WHERE d.object_key=$1 AND d.case_id::text=$2 AND NOT d.sealed AND NOT `+internalDocSQL, t),
				key, caseID).Scan(&id)
			add(id)
		}
	}
	return ids
}

// recordPartyDocs: a letter went out -- it, and the documents its links carry, are kept for each organization
// on the platform it went to. sentAt zero: now. Returns how many documents were added.
func (s *server) recordPartyDocs(r *http.Request, tenant, caseID, template, rfiTo, subject, body, qaID string, to, cc []string, sentAt time.Time) int {
	docs := s.linkedDocs(r.Context(), tenant, caseID, body)
	if sentAt.IsZero() {
		sentAt = time.Now()
	}
	n := 0
	for side := range s.letterSides(r, tenant, caseID, template, rfiTo, to, cc) {
		org := s.sideContact(r.Context(), tenant, caseID, side).org
		if org == "" {
			continue
		}
		{
			// One row per letter, whichever path recorded it first (the send and the mail journal both do).
			sum := sha1.Sum([]byte(caseID + "\x00" + subject + "\x00" + body))
			mailKey := "m:" + hex.EncodeToString(sum[:])
			_, _ = s.db.Exec(r.Context(), `
				INSERT INTO public.party_mail (tenant, case_id, org, side, qa_id, template, subject, body, sent_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (tenant, org, qa_id) DO NOTHING`,
				tenant, caseID, org, side, mailKey, template, truncate(subject, 300), body, sentAt)
		}
		for _, id := range docs {
			tag, err := s.db.Exec(r.Context(), `
				INSERT INTO public.party_documents (tenant, case_id, org, side, doc_id, template, subject, qa_id, sent_at)
				VALUES ($1,$2,$3,$4,$5::uuid,$6,$7,nullif($8,''),$9) ON CONFLICT (tenant, org, doc_id) DO NOTHING`,
				tenant, caseID, org, side, id, template, truncate(subject, 300), qaID, sentAt)
			if err == nil {
				n += int(tag.RowsAffected())
			}
		}
	}
	return n
}

// orgSentDocs: GET /tpa/sent-documents and /hp/sent-documents[?case_id=] -- the letters sent to the caller's
// organization and the documents they carried, newest first.
func (s *server) orgSentDocs(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"not linked to an organization"}`, http.StatusNotFound)
			return
		}
		t := sanitizeTenant(tenant)
		where, args := "p.tenant=$1 AND p.org=$2", []any{tenant, py.org()}
		if c := r.URL.Query().Get("case_id"); c != "" {
			args = append(args, c)
			where += " AND p.case_id=$3"
		}
		rows, err := s.queryRows(r, fmt.Sprintf(`
			SELECT p.case_id, coalesce(c.case_number,'') AS case_number, d.id::text AS doc_id, coalesce(d.filename,'') AS filename,
			       coalesce(d.folder,'') AS folder, d.content_type, p.template, coalesce(p.subject,'') AS subject, p.sent_at
			FROM public.party_documents p
			JOIN tenant_%s.documents d ON d.id = p.doc_id AND NOT d.sealed AND NOT `+internalDocSQL+`
			LEFT JOIN tenant_%s.cases c ON c.id::text = p.case_id
			WHERE %s ORDER BY p.sent_at DESC, d.filename LIMIT 5000`, t, t, where), args...)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		mail, err := s.queryRows(r, fmt.Sprintf(`
			SELECT p.case_id, coalesce(c.case_number,'') AS case_number, p.qa_id, p.template, coalesce(p.subject,'') AS subject,
			       coalesce(p.body,'') AS body, p.sent_at
			FROM public.party_mail p LEFT JOIN tenant_%s.cases c ON c.id::text = p.case_id
			WHERE %s ORDER BY p.sent_at DESC LIMIT 2000`, t, where), args...)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"documents": rows, "mail": mail})
	}
}

// orgSentDocDownload: GET /tpa/sent-documents/{docId} and /hp/... -- one document sent to the caller's
// organization.
func (s *server) orgSentDocDownload(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"not linked to an organization"}`, http.StatusNotFound)
			return
		}
		var caseID, key, partsRaw, name string
		var size int64
		if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
			SELECT p.case_id, d.object_key, coalesce(d.parts::text,''), coalesce(d.size_bytes,0), coalesce(d.filename, d.id::text)
			FROM public.party_documents p JOIN tenant_%s.documents d ON d.id = p.doc_id
			WHERE p.tenant=$1 AND p.org=$2 AND d.id::text=$3 AND NOT d.sealed AND NOT `+internalDocSQL, sanitizeTenant(tenant)),
			tenant, py.org(), chi.URLParam(r, "docId")).Scan(&caseID, &key, &partsRaw, &size, &name); err != nil {
			http.Error(w, `{"error":"no such document was sent to your organization"}`, http.StatusNotFound)
			return
		}
		var parts []partMeta
		_ = json.Unmarshal([]byte(partsRaw), &parts)
		s.logActivity(r.Context(), tenant, caseID, "PARTY_DOC_DOWNLOAD", fmt.Sprintf("%s downloaded %q from its portal", py.name, name))
		s.streamDocRange(w, r, tenant, key, parts, size, name)
	}
}

// backfillPartyDocs: POST /party-documents/backfill -- records the letters sent before the record existed, and
// what they carried. Safe to repeat.
func (s *server) backfillPartyDocs(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	letters, added, err := s.backfillTenantPartyDocs(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"letters": letters, "added": added})
}

// backfillPartyDocsAll runs the backfill for every tenant once at startup (one replica at a time); a letter
// already recorded is skipped, so a restart adds only what is missing.
func (s *server) backfillPartyDocsAll() {
	ctx := context.Background()
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	var got bool
	if conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('idre:party-docs-backfill'))`).Scan(&got) != nil || !got {
		return
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('idre:party-docs-backfill'))`)
	rows, err := s.db.Query(ctx, `SELECT DISTINCT tenant FROM public.correspondence_log WHERE direction='OUT' AND coalesce(case_id,'') <> ''`)
	if err != nil {
		return
	}
	var tenants []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			tenants = append(tenants, t)
		}
	}
	rows.Close()
	for _, t := range tenants {
		c := context.WithValue(context.WithValue(ctx, ctxTenant{}, t), ctxPrincipal{}, principal{Subject: "system", Roles: []string{serviceRole}})
		r, _ := http.NewRequestWithContext(c, http.MethodGet, "/", nil)
		if letters, added, err := s.backfillTenantPartyDocs(r, t); err != nil {
			slog.Error("party documents backfill", "tenant", t, "err", err)
		} else if added > 0 {
			slog.Info("party documents backfill", "tenant", t, "letters", letters, "documents_added", added)
		}
	}
}

// backfillTenantPartyDocs records one tenant's sent letters: how many letters were read, documents added.
func (s *server) backfillTenantPartyDocs(r *http.Request, tenant string) (int, int, error) {
	// The mail journal holds every letter that went out (QA-approved ones included); the QA record adds an RFI's
	// side.
	rows, err := s.db.Query(r.Context(), `
		SELECT l.case_id, coalesce(l.template,''), coalesce(q.rfi_to,''), coalesce(l.subject,''), coalesce(l.body,''),
		       coalesce(l.recipients::text,'{}'), l.created_at
		FROM public.correspondence_log l
		LEFT JOIN LATERAL (SELECT rfi_to FROM public.qa_reviews q WHERE q.tenant=l.tenant AND q.case_id=l.case_id
		                   AND q.artifact=l.template AND q.subject=l.subject LIMIT 1) q ON true
		WHERE l.tenant=$1 AND l.direction='OUT' AND l.delivery_status='SENT' AND coalesce(l.case_id,'') <> ''
		  AND coalesce(l.template,'') <> 'sharebox_download'
		ORDER BY l.created_at`, tenant)
	if err != nil {
		return 0, 0, err
	}
	type sent struct {
		caseID, tpl, rfiTo, subject, body, rcpts string
		at                                       time.Time
	}
	var all []sent
	for rows.Next() {
		var x sent
		if rows.Scan(&x.caseID, &x.tpl, &x.rfiTo, &x.subject, &x.body, &x.rcpts, &x.at) == nil {
			all = append(all, x)
		}
	}
	rows.Close()
	added := 0
	for _, x := range all {
		var rc struct{ To, CC []string }
		_ = json.Unmarshal([]byte(x.rcpts), &rc)
		added += s.recordPartyDocs(r, tenant, x.caseID, x.tpl, x.rfiTo, x.subject, x.body, "", rc.To, rc.CC, x.at)
	}
	return len(all), added, nil
}
