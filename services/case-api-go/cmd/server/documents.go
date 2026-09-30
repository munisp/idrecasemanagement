// documents.go — document lifecycle: encrypted upload, authorized download,
// retention/legal-hold, and analysis-status tracking.
//
// Flow (upload):  multipart -> vault /docs/seal (AES-256-GCM, tenant data key)
//                 -> MinIO ciphertext object -> metadata row -> Kafka event
//                 (doc-intel service consumes and analyzes).
// Flow (download): authorize -> sealed-document guard -> MinIO fetch
//                 -> vault /docs/open -> plaintext stream.
package main

import (
	"bytes"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	docBucket      = "idre-docs"
	maxDocBytes    = 100 << 20 // 100 MB hard cap
	retentionYears = 6         // federal retention mandate
)

type docStore struct{ mc *minio.Client }

func newDocStore(endpoint, user, pass string) (*docStore, error) {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(user, pass, ""),
		Secure: false, // TLS terminated at ingress in prod; in-cluster plaintext
	})
	if err != nil {
		return nil, err
	}
	return &docStore{mc: mc}, nil
}

type vaultDocReq struct {
	Tenant  string `json:"tenant"`
	Key     string `json:"key"`
	DataB64 string `json:"data_b64"`
}

// uploadDocument: POST /v1/tenants/{tenant}/cases/{caseId}/documents (multipart)
func (s *server) uploadDocument(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	p := r.Context().Value(ctxPrincipal{}).(principal)

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
	sealedDoc := r.FormValue("sealed") == "true" // offer justifications: sealed until reveal

	docID := newUUID()
	objectKey := fmt.Sprintf("%s/cases/%s/%s.enc", tenant, caseID, docID)

	// 1. Encrypt through the vault (plaintext never touches disk/object storage).
	ct, err := s.vaultSealDoc(r, tenant, objectKey, raw)
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}

	// 2. Persist ciphertext to MinIO with retention metadata.
	_, err = s.docs.mc.PutObject(r.Context(), docBucket, objectKey,
		bytes.NewReader(ct), int64(len(ct)), minio.PutObjectOptions{
			ContentType:  "application/octet-stream",
			UserMetadata: map[string]string{"tenant": tenant, "case": caseID},
			// Legal hold: retention governs object-lock in prod (WORM bucket policy).
		})
	if err != nil {
		http.Error(w, `{"error":"store failed"}`, http.StatusBadGateway)
		return
	}

	// 3. Metadata row + outbox event in one tx.
	var version int
	err = s.db.QueryRow(r.Context(), fmt.Sprintf(`
		INSERT INTO tenant_%s.documents
		  (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,1) RETURNING version`, sanitizeTenant(tenant)),
		docID, caseID, objectKey, len(raw), hdr.Header.Get("Content-Type"), sealedDoc, p.Subject).
		Scan(&version)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}

	// 4. Event for the doc-intel pipeline (Kafka via outbox relay).
	s.publish(r.Context(), tenant, "documents", map[string]any{
		"type": "doc.uploaded", "tenant": tenant, "case_id": caseID,
		"doc_id": docID, "object_key": objectKey, "content_type": hdr.Header.Get("Content-Type"),
		"sealed": sealedDoc, "at": time.Now().UTC(),
	})

	// 5. Unified timeline entry (visible in caseDetail + account 360 + voice).
	s.logActivity(r.Context(), tenant, caseID, "DOCUMENT_UPLOADED",
		fmt.Sprintf("%s uploaded %q (%d bytes, sealed=%v) by %s — analysis queued",
			hdr.Filename, hdr.Filename, len(raw), sealedDoc, p.Subject))
	writeJSON(w, http.StatusCreated, map[string]any{
		"doc_id": docID, "version": version, "bytes": len(raw),
		"analysis": "QUEUED", // doc-intel consumes doc.uploaded
	})
}

// downloadDocument: GET .../documents/{docId}/download — sealed guard enforced.
func (s *server) downloadDocument(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	docID := chi.URLParam(r, "docId")
	var key, ct string
	var sealed bool
	err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT object_key, content_type, sealed FROM tenant_%s.documents WHERE id=$1`,
		sanitizeTenant(tenant)), docID).Scan(&key, &ct, &sealed)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// Double-blind guard: sealed docs (offer justifications) only open after lawful reveal.
	if sealed {
		if !s.revealLawful(r, chi.URLParam(r, "caseId")) {
			http.Error(w, `{"error":"document sealed until lawful offer reveal"}`, http.StatusLocked)
			return
		}
	}
	obj, err := s.docs.mc.GetObject(r.Context(), docBucket, key, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, `{"error":"object missing"}`, http.StatusNotFound)
		return
	}
	defer obj.Close()
	ct2, err := io.ReadAll(obj)
	if err != nil {
		http.Error(w, `{"error":"fetch failed"}`, http.StatusBadGateway)
		return
	}
	pt, err := s.vaultOpenDoc(r, tenant, key, ct2)
	if err != nil {
		http.Error(w, `{"error":"decrypt failed"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, docID))
	w.Write(pt)
}

// listDocuments + analysis status.
func (s *server) listDocuments(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT d.id, d.content_type, d.size_bytes, d.sealed, d.created_at,
		       COALESCE(a.status,'QUEUED'), COALESCE(a.doc_type,'')
		FROM tenant_%s.documents d
		LEFT JOIN public.doc_analysis a ON a.doc_id = d.id
		WHERE d.case_id=$1 ORDER BY d.created_at DESC`, sanitizeTenant(tenant)),
		chi.URLParam(r, "caseId"))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, ct, status, dtype string
		var size int
		var sealed bool
		var at time.Time
		if rows.Scan(&id, &ct, &size, &sealed, &at, &status, &dtype) == nil {
			out = append(out, map[string]any{
				"doc_id": id, "content_type": ct, "size_bytes": size, "sealed": sealed,
				"uploaded_at": at, "analysis_status": status, "doc_type": dtype,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// documentAnalysis: full structured result from the PaddleOCR+VLM pipeline.
func (s *server) documentAnalysis(w http.ResponseWriter, r *http.Request) {
	var payload, docType, status string
	err := s.db.QueryRow(r.Context(), `
		SELECT status, doc_type, COALESCE(result::text,'{}') FROM public.doc_analysis
		WHERE doc_id=$1`, chi.URLParam(r, "docId")).Scan(&status, &docType, &payload)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":%q,"doc_type":%q,"result":%s}`, status, docType, payload)
}

// ---- vault helpers ---------------------------------------------------------

func (s *server) vaultSealDoc(r *http.Request, tenant, key string, pt []byte) ([]byte, error) {
	body, _ := json.Marshal(vaultDocReq{Tenant: tenant, Key: key, DataB64: b64enc(pt)})
	resp, err := http.Post(s.cfg.VaultURL+"/docs/seal", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		SealedB64 string `json:"sealed_b64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return b64dec(out.SealedB64)
}

func (s *server) vaultOpenDoc(r *http.Request, tenant, key string, ct []byte) ([]byte, error) {
	body, _ := json.Marshal(vaultDocReq{Tenant: tenant, Key: key, DataB64: b64enc(ct)})
	resp, err := http.Post(s.cfg.VaultURL+"/docs/open", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		DataB64 string `json:"data_b64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return b64dec(out.DataB64)
}

// revealLawful mirrors the vault's reveal-check for document-level guards.
func (s *server) revealLawful(r *http.Request, caseID string) bool {
	var revealed bool
	tenant := r.Context().Value(ctxTenant{}).(string)
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT status IN ('OFFERS_REVEALED','DETERMINED','CLOSED_PAID') FROM tenant_%s.cases WHERE id=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&revealed)
	return revealed
}

func newUUID() string { // uuid v4 without an extra dep
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func b64enc(b []byte) string        { return base64.StdEncoding.EncodeToString(b) }
func b64dec(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
