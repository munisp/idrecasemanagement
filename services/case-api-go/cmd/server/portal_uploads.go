// Author: Patrick Munis at newwave
package main

// portal_uploads.go — big files can be stopped and resumed (portal uploads).
//
//   POST   /uploads                {"filename","size"} -> {"id","offset":0,"chunk_bytes"}
//   GET    /uploads/{id}           -> {"offset","size","status"}  (HEAD: Upload-Offset header)
//   PATCH  /uploads/{id}           the next chunk at Upload-Offset
//   DELETE /uploads/{id}           give up
//
// Every chunk is virus-scanned and vault-sealed before it is stored, so no
// plaintext lands anywhere; the first chunk's content must match the file's
// type. An interrupted upload resumes at the offset the server reports (the
// portal keeps the id across a reload). A finished upload is then used by any
// of the usual upload endpoints: the request sends upload_id instead of the
// file, resumedUpload rebuilds it with the file joined from its chunks, and
// the endpoint runs as for any upload -- with the whole file scanned again,
// since a signature can straddle two chunks. Only the person who started an
// upload can continue or use it; an unused one expires after 24 hours.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

const (
	resumableFromBytes = 8 << 20  // the portal sends files above this in chunks
	uploadChunkBytes   = 8 << 20  // the chunk size the portal uses
	uploadChunkMax     = 16 << 20 // the most one PATCH may carry
)

func uploadKey(tenant, id string, n int) string {
	return fmt.Sprintf("%s/uploads/%s/%05d.enc", tenant, id, n)
}

// uploadNamePolicy: a document type we accept, or a ZIP (for bulk uploads).
func uploadNamePolicy(name string) error {
	if strings.ToLower(filepath.Ext(name)) == ".zip" {
		return nil
	}
	return contentPolicy(name, nil)
}

func (s *server) createUpload(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	var in struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in) != nil || strings.TrimSpace(in.Filename) == "" || in.Size <= 0 {
		http.Error(w, `{"error":"filename and a positive size are required"}`, http.StatusBadRequest)
		return
	}
	if err := uploadNamePolicy(in.Filename); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnsupportedMediaType)
		return
	}
	if lim := docSizeLimitFor(in.Filename); in.Size > lim {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, fmt.Sprintf("%s is %s — the limit for this type is %s", in.Filename, humanBytes(in.Size), humanBytes(lim))), http.StatusRequestEntityTooLarge)
		return
	}
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.portal_uploads (tenant, owner, filename, size_bytes) VALUES ($1,$2,$3,$4) RETURNING id::text`,
		tenant, p.Subject, truncate(in.Filename, 200), in.Size).Scan(&id); err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "offset": 0, "chunk_bytes": uploadChunkBytes})
}

type portalUpload struct {
	id, filename, status string
	size, received       int64
	parts                int
}

func (s *server) ownUpload(r *http.Request, tenant, id string) (*portalUpload, bool) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	u := &portalUpload{id: id}
	err := s.db.QueryRow(r.Context(), `
		SELECT filename, status, size_bytes, received, parts FROM public.portal_uploads
		WHERE tenant=$1 AND id::text=$2 AND owner=$3 AND expires_at > now()`, tenant, id, p.Subject).
		Scan(&u.filename, &u.status, &u.size, &u.received, &u.parts)
	return u, err == nil
}

func (s *server) uploadStatus(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	u, ok := s.ownUpload(r, tenant, chi.URLParam(r, "uploadId"))
	if !ok {
		http.Error(w, `{"error":"no such upload (it may have expired) — start again"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(u.received, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(u.size, 10))
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": u.id, "filename": u.filename, "offset": u.received, "size": u.size, "status": u.status})
}

func (s *server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	u, ok := s.ownUpload(r, tenant, chi.URLParam(r, "uploadId"))
	if !ok {
		http.Error(w, `{"error":"no such upload (it may have expired) — start again"}`, http.StatusNotFound)
		return
	}
	if u.status != "OPEN" {
		http.Error(w, fmt.Sprintf(`{"error":"this upload is %s"}`, strings.ToLower(u.status)), http.StatusConflict)
		return
	}
	off, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || off != u.received {
		// Out of step (a retried chunk that did land): tell the client where to go on.
		w.Header().Set("Upload-Offset", strconv.FormatInt(u.received, 10))
		writeJSON(w, http.StatusConflict, map[string]any{"error": "send the chunk at the upload's offset", "offset": u.received})
		return
	}
	chunk, err := io.ReadAll(io.LimitReader(r.Body, uploadChunkMax+1))
	if err != nil || len(chunk) == 0 || len(chunk) > uploadChunkMax || off+int64(len(chunk)) > u.size {
		http.Error(w, `{"error":"a chunk must be 1 byte to 16 MB and not run past the file's size"}`, http.StatusBadRequest)
		return
	}
	if off == 0 { // the first bytes say what the file really is
		if strings.ToLower(filepath.Ext(u.filename)) == ".zip" {
			if !bytes.HasPrefix(chunk, []byte("PK\x03\x04")) {
				http.Error(w, `{"error":"this file is not a ZIP"}`, http.StatusUnsupportedMediaType)
				s.abortUpload(r, tenant, u)
				return
			}
		} else if err := contentPolicy(u.filename, chunk); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnsupportedMediaType)
			s.abortUpload(r, tenant, u)
			return
		}
	}
	if sig, ok := s.scanOrRefuse(w, bytes.NewReader(chunk), "upload"); !ok {
		if sig != "" {
			s.abortUpload(r, tenant, u) // infected: nothing of it is kept
		}
		return
	}
	key := uploadKey(tenant, u.id, u.parts+1)
	sealed, err := s.vaultSealDoc(r, tenant, key, chunk)
	if err != nil {
		http.Error(w, `{"error":"seal failed"}`, http.StatusBadGateway)
		return
	}
	if _, err := s.docs.mc.PutObject(r.Context(), docBucket, key, bytes.NewReader(sealed), int64(len(sealed)), minio.PutObjectOptions{}); err != nil {
		http.Error(w, `{"error":"store failed"}`, http.StatusBadGateway)
		return
	}
	newOff := off + int64(len(chunk))
	status := "OPEN"
	if newOff == u.size {
		status = "COMPLETE"
	}
	if tag, err := s.db.Exec(r.Context(), `
		UPDATE public.portal_uploads SET received=$3, parts=parts+1, status=$4, updated_at=now()
		WHERE tenant=$1 AND id::text=$2 AND received=$5 AND status='OPEN'`, tenant, u.id, newOff, status, off); err != nil || tag.RowsAffected() == 0 {
		http.Error(w, `{"error":"the upload moved on meanwhile — check its offset"}`, http.StatusConflict)
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(newOff, 10))
	writeJSON(w, http.StatusOK, map[string]any{"offset": newOff, "size": u.size, "status": status})
}

func (s *server) deleteUpload(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	u, ok := s.ownUpload(r, tenant, chi.URLParam(r, "uploadId"))
	if !ok {
		http.Error(w, `{"error":"no such upload"}`, http.StatusNotFound)
		return
	}
	s.abortUpload(r, tenant, u)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ABORTED"})
}

func (s *server) abortUpload(r *http.Request, tenant string, u *portalUpload) {
	_, _ = s.db.Exec(r.Context(), `UPDATE public.portal_uploads SET status='ABORTED', updated_at=now() WHERE tenant=$1 AND id::text=$2`, tenant, u.id)
	s.removeUploadParts(r, tenant, u)
}

func (s *server) removeUploadParts(r *http.Request, tenant string, u *portalUpload) {
	for n := 1; n <= u.parts+1; n++ {
		_ = s.docs.mc.RemoveObject(r.Context(), docBucket, uploadKey(tenant, u.id, n), minio.RemoveObjectOptions{})
	}
}

// joinUpload: the finished upload's whole file, from its sealed chunks.
func (s *server) joinUpload(r *http.Request, tenant string, u *portalUpload) ([]byte, error) {
	var buf bytes.Buffer
	for n := 1; n <= u.parts; n++ {
		key := uploadKey(tenant, u.id, n)
		obj, err := s.docs.mc.GetObject(r.Context(), docBucket, key, minio.GetObjectOptions{})
		if err != nil {
			return nil, err
		}
		ct, err := io.ReadAll(obj)
		obj.Close()
		if err != nil {
			return nil, err
		}
		pt, err := s.vaultOpenDoc(r, tenant, key, ct)
		if err != nil {
			return nil, err
		}
		buf.Write(pt)
	}
	if int64(buf.Len()) != u.size {
		return nil, fmt.Errorf("the upload is incomplete (%d of %d bytes)", buf.Len(), u.size)
	}
	return buf.Bytes(), nil
}

// resumedUpload lets every upload endpoint take a finished resumable upload:
// a multipart request with an upload_id field and no file is rebuilt with the
// file in its "file" field, and the endpoint runs unchanged. The upload is
// used up only when the endpoint accepted it (a refused one can be retried).
func (s *server) resumedUpload(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPut || !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			next.ServeHTTP(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		if err != nil {
			http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
			return
		}
		if len(raw) > 1<<20 || !bytes.Contains(raw, []byte(`name="upload_id"`)) {
			// An ordinary upload with its file: hand it on untouched.
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), r.Body))
			next.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, `{"error":"malformed form"}`, http.StatusBadRequest)
			return
		}
		tenant, _ := r.Context().Value(ctxTenant{}).(string)
		u, ok := s.ownUpload(r, tenant, r.FormValue("upload_id"))
		if !ok || u.status != "COMPLETE" {
			http.Error(w, `{"error":"that upload is not finished (or has expired) — resume it or start again"}`, http.StatusConflict)
			return
		}
		file, err := s.joinUpload(r, tenant, u)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, "the upload could not be read back: "+err.Error()), http.StatusBadGateway)
			return
		}
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, vs := range r.MultipartForm.Value {
			if k == "upload_id" {
				continue
			}
			for _, v := range vs {
				_ = mw.WriteField(k, v)
			}
		}
		fw, _ := mw.CreateFormFile("file", u.filename)
		_, _ = fw.Write(file)
		_ = mw.Close()
		r2 := r.Clone(r.Context())
		r2.Body = io.NopCloser(&body)
		r2.ContentLength = int64(body.Len())
		r2.Header.Set("Content-Type", mw.FormDataContentType())
		r2.Form, r2.PostForm, r2.MultipartForm = nil, nil, nil
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r2)
		if sw.code < 300 {
			_, _ = s.db.Exec(r.Context(), `UPDATE public.portal_uploads SET status='USED', updated_at=now() WHERE tenant=$1 AND id::text=$2`, tenant, u.id)
			s.removeUploadParts(r, tenant, u)
		}
	})
}

// sweepExpiredUploads removes the sealed chunks of uploads left unused past
// their expiry (run with the bulk filer's pass).
func (s *server) sweepExpiredUploads(ctx context.Context) {
	rows, err := s.db.Query(ctx, `
		UPDATE public.portal_uploads SET status='EXPIRED', updated_at=now()
		WHERE status IN ('OPEN','COMPLETE') AND expires_at < now()
		RETURNING tenant, id::text, parts`)
	if err != nil {
		return
	}
	type gone struct {
		tenant, id string
		parts      int
	}
	var list []gone
	for rows.Next() {
		var g gone
		if rows.Scan(&g.tenant, &g.id, &g.parts) == nil {
			list = append(list, g)
		}
	}
	rows.Close()
	for _, g := range list {
		for n := 1; n <= g.parts; n++ {
			_ = s.docs.mc.RemoveObject(ctx, docBucket, uploadKey(g.tenant, g.id, n), minio.RemoveObjectOptions{})
		}
	}
}
