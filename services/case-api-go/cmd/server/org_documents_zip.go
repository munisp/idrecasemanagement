// Author: Patrick Munis at newwave
package main

// org_documents_zip.go — a TPA (or a health plan) downloads the documents of
// its own disputes in one ZIP: all of them, one provider's, one bulk upload's,
// or the disputes it picks.
//
//   GET /tpa/documents.zip[?case_id=..&case_id=..|?client_id=..|?batch_id=..]
//   GET /hp/documents.zip[?case_id=..|?party_id=..]
//
// One folder per dispute (its case number). Sealed offers and anything that
// did not pass the virus scan are left out. A manifest.csv lists every file
// with its SHA-256, so the download can be checked against what is on file.

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	orgZipMaxCases = 300
	orgZipMaxBytes = 2 << 30 // 2 GB of documents per download
)

func (s *server) orgDocumentsZip(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"not linked to an organization"}`, http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		where, args := fmt.Sprintf("o.tenant=$1 AND o.%s::text=$2", py.originCol()), []any{tenant, py.id}
		if ids := q["case_id"]; len(ids) > 0 {
			args = append(args, ids)
			where += fmt.Sprintf(" AND o.case_id = ANY($%d)", len(args))
		}
		if c := q.Get("client_id"); c != "" && kind == "tpa" {
			args = append(args, c)
			where += fmt.Sprintf(" AND o.client_id::text=$%d", len(args))
		}
		if p := q.Get("party_id"); p != "" && kind == "hp" {
			args = append(args, p)
			where += fmt.Sprintf(" AND o.hp_party_id::text=$%d", len(args))
		}
		if b := q.Get("batch_id"); b != "" && kind == "tpa" {
			args = append(args, b)
			where += fmt.Sprintf(" AND o.case_id IN (SELECT case_id FROM public.tpa_dispute_batch_items WHERE batch_id::text=$%d)", len(args))
		}
		// The office's own documents only once they were sent to this organization (outsideDocFilter).
		args = append(args, py.org())
		where += fmt.Sprintf(" AND (NOT "+officeDocSQL+" OR d.id IN (SELECT pd.doc_id FROM public.party_documents pd WHERE pd.tenant=o.tenant AND pd.org=$%d))", len(args))
		t := sanitizeTenant(tenant)
		rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
			SELECT c.case_number, coalesce(o.initiating_party_name,''), d.object_key, coalesce(d.filename, d.id::text),
			       coalesce(d.folder,''), d.parts, coalesce(d.size_bytes,0), d.created_at
			FROM public.case_origin o
			JOIN tenant_%s.cases c ON c.id::text = o.case_id
			JOIN tenant_%s.documents d ON d.case_id::text = o.case_id
			WHERE %s AND d.scan_status='CLEAN' AND NOT d.sealed AND NOT `+internalDocSQL+`
			ORDER BY c.case_number, d.folder, d.created_at`, t, t, where), args...)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		type doc struct {
			caseNumber, party, key, name, folder string
			parts                                []byte
			size                                 int64
			at                                   time.Time
		}
		var docs []doc
		cases := map[string]bool{}
		var total int64
		for rows.Next() {
			var d doc
			if rows.Scan(&d.caseNumber, &d.party, &d.key, &d.name, &d.folder, &d.parts, &d.size, &d.at) == nil {
				docs = append(docs, d)
				cases[d.caseNumber] = true
				total += d.size
			}
		}
		rows.Close()
		if len(docs) == 0 {
			http.Error(w, `{"error":"no documents to download for these disputes"}`, http.StatusNotFound)
			return
		}
		if len(cases) > orgZipMaxCases || total > orgZipMaxBytes {
			http.Error(w, fmt.Sprintf(`{"error":"that is %d disputes and %s of documents — download at most %d disputes or 2 GB at a time (pick a provider or a bulk upload)"}`,
				len(cases), humanBytes(total), orgZipMaxCases), http.StatusRequestEntityTooLarge)
			return
		}
		name := fmt.Sprintf("%s documents %s.zip", py.name, time.Now().UTC().Format("2006-01-02"))
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", strings.NewReplacer(`"`, "", "/", "-").Replace(name)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		zw := zip.NewWriter(w)
		fl, _ := w.(http.Flusher)
		var manifest strings.Builder
		mw := csv.NewWriter(&manifest)
		_ = mw.Write([]string{"case_number", "initiating_party", "file", "folder", "received", "bytes", "sha256"})
		used := map[string]int{}
		for _, d := range docs {
			path := zipSafe(d.caseNumber) + "/" + zipSafe(d.name)
			if n := used[path]; n > 0 {
				path = fmt.Sprintf("%s/%d-%s", zipSafe(d.caseNumber), n, zipSafe(d.name))
			}
			used[zipSafe(d.caseNumber)+"/"+zipSafe(d.name)]++
			entry, err := zw.CreateHeader(&zip.FileHeader{Name: path, Method: zip.Deflate, Modified: d.at})
			if err != nil {
				continue
			}
			h := sha256.New()
			cw := &countWriter{}
			if err := s.writeDocPlaintext(r, tenant, d.key, d.parts, io.MultiWriter(entry, h, cw)); err != nil {
				return // mid-stream: the client sees a broken download, nothing partial passes as complete
			}
			_ = mw.Write([]string{d.caseNumber, d.party, path, d.folder, d.at.Format(time.RFC3339), fmt.Sprint(cw.n), hex.EncodeToString(h.Sum(nil))})
			if fl != nil {
				fl.Flush()
			}
		}
		mw.Flush()
		if m, err := zw.Create("manifest.csv"); err == nil {
			_, _ = io.WriteString(m, manifest.String())
		}
		_ = zw.Close()
	}
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// zipSafe keeps a ZIP entry name to one plain path segment.
func zipSafe(s string) string {
	s = strings.NewReplacer("/", "-", "\\", "-", "..", "-", ":", "-").Replace(strings.TrimSpace(s))
	if s == "" {
		return "document"
	}
	return s
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	default:
		return fmt.Sprintf("%d KB", n/1024)
	}
}
