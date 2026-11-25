// Author: Patrick Munis at newwave
package main

// Archiving a case's documentation (change log 2026-11-25 #24e: once the Final Order is released, the PM tells the
// case team and the case documentation is archived). Every clean document on the case -- filings, letters, the
// Final Order, uploads -- is bundled into one zip, sealed by the vault and kept on the case under Determination as
// "Case archive <case number>.zip". It runs when AHCA's Final Order is recorded, and the PM can run it again
// (POST /cases/{caseId}/archive) after anything is added later.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/go-chi/chi/v5"
)

const maxArchiveBytes = 512 << 20

// archiveCaseDocs builds and stores the archive; it returns the new document's id.
func (s *server) archiveCaseDocs(r *http.Request, tenant, caseID, by string) (string, int, error) {
	ctx := r.Context()
	t := sanitizeTenant(tenant)
	rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT id::text, object_key, coalesce(filename, id::text), coalesce(parts::text,''), coalesce(size_bytes,0)
		FROM tenant_%s.documents
		WHERE case_id::text=$1 AND scan_status='CLEAN' AND NOT sealed AND coalesce(filename,'') NOT LIKE 'Case archive %%'
		ORDER BY folder, created_at`, t), caseID)
	if err != nil {
		return "", 0, err
	}
	var docs []bundleDoc
	var total int64
	for rows.Next() {
		var d bundleDoc
		var parts string
		if rows.Scan(&d.id, &d.key, &d.name, &parts, &d.size) == nil {
			if parts != "" {
				d.parts = []byte(parts)
			}
			total += d.size
			docs = append(docs, d)
		}
	}
	rows.Close()
	if len(docs) == 0 {
		return "", 0, fmt.Errorf("no documents to archive")
	}
	if total > maxArchiveBytes {
		return "", 0, fmt.Errorf("the documents are over %d MB together: archive them by download instead", maxArchiveBytes>>20)
	}
	cn := s.caseNumberOf(ctx, tenant, caseID)
	rec := httptest.NewRecorder()
	s.writeDocsZip(rec, r, tenant, "archive.zip", docs) // the same zip the document downloads build
	raw := rec.Body.Bytes()
	if len(raw) == 0 {
		return "", 0, fmt.Errorf("the archive came out empty")
	}
	docID := newUUID()
	key := fmt.Sprintf("%s/cases/%s/%s.enc", tenant, caseID, docID)
	ct, err := s.vaultSealDoc(r, tenant, key, raw)
	if err != nil {
		return "", 0, fmt.Errorf("seal failed: %w", err)
	}
	if err := s.docs.api.PutObject(ctx, docBucket, key, bytes.NewReader(ct), int64(len(ct)),
		putOpts{UserMetadata: map[string]string{"tenant": tenant, "case": caseID}}); err != nil {
		return "", 0, fmt.Errorf("store failed: %w", err)
	}
	name := fmt.Sprintf("Case archive %s (%s).zip", cn, time.Now().UTC().Format("2006-01-02"))
	if _, err := s.db.Exec(ctx, fmt.Sprintf(`
		INSERT INTO tenant_%s.documents (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version, scan_status, filename, folder)
		VALUES ($1,$2::uuid,$3,$4,'application/zip',false,$5,1,'CLEAN',$6,'DETERMINATION')`, t),
		docID, caseID, key, len(raw), by, name); err != nil {
		return "", 0, err
	}
	_, _ = s.db.Exec(ctx, fmt.Sprintf(`UPDATE tenant_%s.cases SET details = coalesce(details,'{}'::jsonb) ||
		jsonb_build_object('archived_at', $2::text, 'archive_doc_id', $3::text) WHERE id::text=$1`, t), caseID, time.Now().UTC().Format(time.RFC3339), docID)
	s.logActivity(ctx, tenant, caseID, "CASE_ARCHIVED", fmt.Sprintf("Case documentation archived: %d document(s) in %s", len(docs), name))
	s.logAudit(ctx, tenant, caseID, "CASE_ARCHIVED", map[string]any{"by": by, "documents": len(docs), "bytes": len(raw), "doc_id": docID})
	return docID, len(docs), nil
}

// archiveCaseLater: the archive after AHCA's Final Order is recorded, off the request (it reads every document).
func (s *server) archiveCaseLater(r *http.Request, tenant, caseID string) {
	r2 := r.WithContext(context.WithoutCancel(r.Context()))
	go func() {
		ctx, cancel := context.WithTimeout(r2.Context(), 10*time.Minute)
		defer cancel()
		if _, n, err := s.archiveCaseDocs(r2.WithContext(ctx), tenant, caseID, "system:archive"); err != nil {
			slog.Warn("case archive", "case", caseID, "err", err)
			s.logActivity(ctx, tenant, caseID, "CASE_ARCHIVE_FAILED", "The case documentation could not be archived: "+err.Error()+" — the PM can run it again")
		} else {
			slog.Info("case archived", "case", caseID, "documents", n)
		}
	}()
}

// archiveCase: POST /cases/{caseId}/archive -- the PM (or an admin) archives the case documentation again.
func (s *server) archiveCase(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: the PM archives the case documentation"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	id, n, err := s.archiveCaseDocs(r, tenant, chi.URLParam(r, "caseId"), displayName(p))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"doc_id": id, "documents": n})
}
