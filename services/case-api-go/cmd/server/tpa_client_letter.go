// tpa_client_letter.go — a client's letter of authorization, kept on its profile: the client's permission for the TPA
// to act for it. Stored as every document is (file-type check, malware scan, sealed by the vault). The current letter
// goes onto each dispute filed for the client afterwards, so the reviewer has it without asking. It never stops a filing.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	chi "github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// tpaLetterClient: the caller's TPA, and the client in the path if it is one of that TPA's. The route rules already
// hold a client-limited login to its own client (tpa_client_scope.go).
func (s *server) tpaLetterClient(w http.ResponseWriter, r *http.Request) (*tpaOrg, string, string, bool) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+jsonSafe(err.Error())+`"}`, http.StatusNotFound)
		return nil, "", "", false
	}
	clientID := chi.URLParam(r, "clientId")
	if !s.tpaHasClient(r, tenant, org.ID, clientID) {
		http.Error(w, `{"error":"client not found"}`, http.StatusNotFound)
		return nil, "", "", false
	}
	return org, tenant, clientID, true
}

// tpaLetterUpload: POST /tpa/clients/{clientId}/authorization-letter (multipart "file"). Replaces the current letter.
func (s *server) tpaLetterUpload(w http.ResponseWriter, r *http.Request) {
	org, tenant, clientID, ok := s.tpaLetterClient(w, r)
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
	key := fmt.Sprintf("%s/tpa-clients/%s/letters/%s.enc", tenant, clientID, id)
	ct, err := s.vaultSealDoc(r, tenant, key, raw)
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}
	if _, err := s.docs.mc.PutObject(r.Context(), docBucket, key, bytes.NewReader(ct), int64(len(ct)),
		minio.PutObjectOptions{UserMetadata: map[string]string{"tenant": tenant, "client": clientID}}); err != nil {
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
	if _, err := tx.Exec(r.Context(), `UPDATE public.tpa_client_letters SET ended_at=now(), ended_by=$3 WHERE tenant=$1 AND client_id::text=$2 AND ended_at IS NULL`,
		tenant, clientID, by); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO public.tpa_client_letters (id, tenant, client_id, object_key, filename, content_type, size_bytes, uploaded_by)
		VALUES ($1,$2,$3::uuid,$4,$5,$6,$7,$8)`, id, tenant, clientID, key, hdr.Filename, hdr.Header.Get("Content-Type"), len(raw), by); err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TPA_CLIENT_LETTER_ADDED", map[string]any{"org": "tpa:" + org.ID, "client_id": clientID, "filename": hdr.Filename, "by": by})
	writeJSON(w, http.StatusCreated, map[string]any{"letter_id": id, "filename": hdr.Filename})
}

// tpaLetterGet: GET /tpa/clients/{clientId}/authorization-letter -- the current letter's file.
func (s *server) tpaLetterGet(w http.ResponseWriter, r *http.Request) {
	_, tenant, clientID, ok := s.tpaLetterClient(w, r)
	if !ok {
		return
	}
	var key, name, ctype string
	if s.db.QueryRow(r.Context(), `SELECT object_key, filename, content_type FROM public.tpa_client_letters WHERE tenant=$1 AND client_id::text=$2 AND ended_at IS NULL`,
		tenant, clientID).Scan(&key, &name, &ctype) != nil {
		http.Error(w, `{"error":"no letter on file"}`, http.StatusNotFound)
		return
	}
	obj, err := s.docs.mc.GetObject(r.Context(), docBucket, key, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, `{"error":"object missing"}`, http.StatusNotFound)
		return
	}
	defer obj.Close()
	ct, err := io.ReadAll(obj)
	if err != nil {
		http.Error(w, `{"error":"object missing"}`, http.StatusNotFound)
		return
	}
	pt, err := s.vaultOpenDoc(r, tenant, key, ct)
	if err != nil {
		http.Error(w, `{"error":"decrypt failed"}`, http.StatusBadGateway)
		return
	}
	if ctype == "" || ctype == "text/html" { // never drawn as a page of the portal
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	_, _ = w.Write(pt)
}

// tpaLetterRemove: DELETE /tpa/clients/{clientId}/authorization-letter -- no current letter; the old one stays as history.
func (s *server) tpaLetterRemove(w http.ResponseWriter, r *http.Request) {
	org, tenant, clientID, ok := s.tpaLetterClient(w, r)
	if !ok {
		return
	}
	by := uploaderName(r)
	if _, err := s.db.Exec(r.Context(), `UPDATE public.tpa_client_letters SET ended_at=now(), ended_by=$3 WHERE tenant=$1 AND client_id::text=$2 AND ended_at IS NULL`,
		tenant, clientID, by); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.logAudit(r.Context(), tenant, "", "TPA_CLIENT_LETTER_REMOVED", map[string]any{"org": "tpa:" + org.ID, "client_id": clientID, "by": by})
	writeJSON(w, http.StatusOK, map[string]any{"status": "removed"})
}

// attachClientLetter puts the client's current letter on a dispute just filed for it. The file stays where it is, sealed
// under its own key, as documents of a filed package do. A failure here never stops the filing.
func (s *server) attachClientLetter(r *http.Request, tenant, clientID, caseID, uploadedBy string) {
	var key, name, ctype string
	var size int64
	if s.db.QueryRow(r.Context(), `SELECT object_key, filename, content_type, size_bytes FROM public.tpa_client_letters WHERE tenant=$1 AND client_id::text=$2 AND ended_at IS NULL`,
		tenant, clientID).Scan(&key, &name, &ctype, &size) != nil {
		return
	}
	if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
		INSERT INTO tenant_%s.documents (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version, scan_status, filename, folder)
		VALUES ($1,$2,$3,$4,$5,false,$6,1,'CLEAN',$7,'INTAKE')`, sanitizeTenant(tenant)),
		newUUID(), caseID, key, size, ctype, uploadedBy, "Letter of authorization — "+name); err == nil {
		s.logActivity(r.Context(), tenant, caseID, "DOCUMENTS_ATTACHED", "The client's letter of authorization was attached from its profile")
	}
}
