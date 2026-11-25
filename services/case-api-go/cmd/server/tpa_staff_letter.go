// tpa_staff_letter.go — a TPA's staff authorization letter (2026-11-25; scripts/tpa_staff_letters.sql): the TPA's
// officer naming the staff who may act for it on Florida claim disputes (HaloMD's is the model). Kept on the TPA's
// profile like a client's letter of authorization (tpa_client_letter.go), and attached to every dispute the TPA files,
// where document analysis reads the people it names (doc-intel aor_docs.py) for the Reviewer's AOR check.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// tpaStaffLetterOrg: the caller's TPA; changing the letter is for its administrators.
func (s *server) tpaStaffLetterOrg(w http.ResponseWriter, r *http.Request, change bool) (*tpaOrg, string, bool) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
		return nil, "", false
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if change && org.TeamRole != "ADMIN" && !hasAnyRole(p, "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: your organization's administrators keep the staff authorization letter"}`, http.StatusForbidden)
		return nil, "", false
	}
	return org, tenant, true
}

// tpaStaffLetterUpload: POST /tpa/staff-letter (multipart "file", a PDF). Replaces the current letter.
func (s *server) tpaStaffLetterUpload(w http.ResponseWriter, r *http.Request) {
	org, tenant, ok := s.tpaStaffLetterOrg(w, r, true)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDocBytes)
	if err := r.ParseMultipartForm(maxDocBytes); err != nil {
		http.Error(w, `{"error":"file too large or malformed"}`, http.StatusRequestEntityTooLarge)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file field required"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}
	if !bytes.HasPrefix(raw, []byte("%PDF")) {
		http.Error(w, `{"error":"the staff authorization letter must be a PDF (on your letterhead, signed)"}`, http.StatusUnsupportedMediaType)
		return
	}
	if err := docPolicy(hdr.Filename, raw); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnsupportedMediaType)
		return
	}
	sig, err := s.clamScan(bytes.NewReader(raw))
	if err != nil {
		http.Error(w, `{"error":"malware scanner unavailable — not stored"}`, http.StatusServiceUnavailable)
		return
	}
	if sig != "" {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "malware scan rejected: "+sig), http.StatusUnsupportedMediaType)
		return
	}
	id := newUUID()
	key := fmt.Sprintf("%s/tpas/%s/staff-letters/%s.enc", tenant, org.ID, id)
	ct, err := s.vaultSealDoc(r, tenant, key, raw)
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}
	if err := s.docs.api.PutObject(r.Context(), docBucket, key, bytes.NewReader(ct), int64(len(ct)),
		putOpts{UserMetadata: map[string]string{"tenant": tenant, "tpa": org.ID}}); err != nil {
		http.Error(w, `{"error":"store failed"}`, http.StatusBadGateway)
		return
	}
	by := uploaderName(r)
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `UPDATE public.tpa_staff_letters SET ended_at=now(), ended_by=$3 WHERE tenant=$1 AND tpa_id::text=$2 AND ended_at IS NULL`,
		tenant, org.ID, by); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO public.tpa_staff_letters (id, tenant, tpa_id, object_key, filename, content_type, size_bytes, uploaded_by)
		VALUES ($1,$2,$3::uuid,$4,$5,'application/pdf',$6,$7)`, id, tenant, org.ID, key, hdr.Filename, len(raw), by); err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TPA_STAFF_LETTER_ADDED", map[string]any{"org": "tpa:" + org.ID, "filename": hdr.Filename, "by": by})
	writeJSON(w, http.StatusCreated, map[string]any{"letter_id": id, "filename": hdr.Filename})
}

// tpaStaffLetterGet: GET /tpa/staff-letter -- the current letter's details ({"letter": null} when none), or with
// ?file=1 the PDF itself.
func (s *server) tpaStaffLetterGet(w http.ResponseWriter, r *http.Request) {
	org, tenant, ok := s.tpaStaffLetterOrg(w, r, false)
	if !ok {
		return
	}
	var key, name, by string
	var at any
	if s.db.QueryRow(r.Context(), `SELECT object_key, filename, uploaded_by, uploaded_at FROM public.tpa_staff_letters WHERE tenant=$1 AND tpa_id::text=$2 AND ended_at IS NULL`,
		tenant, org.ID).Scan(&key, &name, &by, &at) != nil {
		writeJSON(w, http.StatusOK, map[string]any{"letter": nil})
		return
	}
	if r.URL.Query().Get("file") != "1" {
		writeJSON(w, http.StatusOK, map[string]any{"letter": map[string]any{"filename": name, "uploaded_by": by, "uploaded_at": at}})
		return
	}
	rc, err := s.docs.api.GetObject(r.Context(), docBucket, key)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	defer rc.Close()
	ct, _ := io.ReadAll(rc)
	pt, err := s.vaultOpenDoc(r, tenant, key, ct)
	if err != nil {
		http.Error(w, `{"error":"open failed"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename=%q`, name))
	_, _ = w.Write(pt)
}

// tpaStaffLetterRemove: DELETE /tpa/staff-letter -- the letter no longer applies (kept as history).
func (s *server) tpaStaffLetterRemove(w http.ResponseWriter, r *http.Request) {
	org, tenant, ok := s.tpaStaffLetterOrg(w, r, true)
	if !ok {
		return
	}
	by := uploaderName(r)
	_, _ = s.db.Exec(r.Context(), `UPDATE public.tpa_staff_letters SET ended_at=now(), ended_by=$3 WHERE tenant=$1 AND tpa_id::text=$2 AND ended_at IS NULL`, tenant, org.ID, by)
	s.logAudit(r.Context(), tenant, "", "TPA_STAFF_LETTER_REMOVED", map[string]any{"org": "tpa:" + org.ID, "by": by})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// attachStaffLetter: the TPA's current staff authorization letter onto a dispute it filed (same stored object; the
// case's documents row is what doc-intel reads it from).
func (s *server) attachStaffLetter(r *http.Request, tenant, tpaID, caseID, uploadedBy string) {
	var key, name string
	var size int64
	if s.db.QueryRow(r.Context(), `SELECT object_key, filename, size_bytes FROM public.tpa_staff_letters WHERE tenant=$1 AND tpa_id::text=$2 AND ended_at IS NULL`,
		tenant, tpaID).Scan(&key, &name, &size) != nil {
		return
	}
	docID := newUUID()
	if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
		INSERT INTO tenant_%s.documents (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version, scan_status, filename, folder)
		VALUES ($1,$2,$3,$4,'application/pdf',false,$5,1,'CLEAN',$6,'INTAKE')`, sanitizeTenant(tenant)),
		docID, caseID, key, size, uploadedBy, "Staff authorization letter — "+name); err != nil {
		return
	}
	s.publish(r.Context(), tenant, "documents", map[string]any{"type": "doc.uploaded", "tenant": tenant, "case_id": caseID,
		"doc_id": docID, "object_key": key, "content_type": "application/pdf", "sealed": false})
	s.logActivity(r.Context(), tenant, caseID, "DOCUMENTS_ATTACHED", "The TPA's staff authorization letter was attached from its profile")
}

// aorCheck: GET /cases/{caseId}/aor-check -- what the case's AOR / staff authorization letters say (doc-intel's
// reading: who authorizes, the people named) beside who filed the dispute, and whether that person is named. For
// the Reviewer's AOR check; it decides nothing.
func (s *server) aorCheck(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "ATTORNEY", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var filedBy, filedOrg string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT coalesce(details->>'filed_by_person',''), coalesce(details->>'filed_by_org','') FROM tenant_%s.cases WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&filedBy, &filedOrg)
	rows, _ := s.queryRows(r, fmt.Sprintf(`
		SELECT d.id::text AS doc_id, coalesce(d.filename,'') AS filename, coalesce(a.result->'extracted_fields'->'aor', '{}'::jsonb)::text AS aor
		FROM tenant_%s.documents d JOIN public.doc_analysis a ON a.doc_id = d.id
		WHERE d.case_id::text=$1 AND a.doc_type='aor_letter' ORDER BY d.created_at DESC LIMIT 10`, sanitizeTenant(tenant)), caseID)
	letters := []map[string]any{}
	named := false
	norm := func(v string) string { return strings.Join(strings.Fields(strings.ToLower(v)), " ") }
	for _, row := range rows {
		var aor map[string]any
		_ = json.Unmarshal([]byte(fmt.Sprint(row["aor"])), &aor)
		reps, _ := aor["representatives"].([]any)
		for _, x := range reps {
			if n, _ := x.(string); filedBy != "" && norm(n) == norm(filedBy) {
				named = true
			}
		}
		if who, _ := aor["authorized_by"].(string); filedBy != "" && norm(who) == norm(filedBy) {
			named = true // the officer who signed it may act too
		}
		letters = append(letters, map[string]any{"doc_id": row["doc_id"], "filename": row["filename"], "aor": aor})
	}
	writeJSON(w, http.StatusOK, map[string]any{"filed_by": filedBy, "filed_by_org": filedOrg, "letters": letters,
		"filer_named": named, "checked": filedBy != "" && len(letters) > 0})
}
