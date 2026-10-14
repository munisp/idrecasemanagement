package main

// tpa_bulk_disputes.go — a TPA files many disputes at once, each with its
// own documents.
//
// One ZIP:
//
//	disputes.csv          dispute_ref,party_name,disputed_amount,contact_name,notes
//	<dispute_ref>/...     that dispute's six documents (eob.pdf, itemized_bill.pdf,
//	                      proof_of_coverage.pdf, open_negotiation_notice.pdf,
//	                      offer_substantiation.pdf, idre_attestation.pdf)
//	                      -- or one merged.pdf holding all six
//
// Every row becomes its own package, checked by doc-intel exactly like a
// single filing. The row waits (WAITING_DOCS) until its package is READY,
// then the background filer opens the case through the same code a single
// filing uses (fileTPADispute), which also attaches the six documents to
// the case. A row whose package needs review or was rejected keeps waiting:
// the TPA fixes it in the package card (or staff decide it in the review
// queue) and the filer picks it up then.
//
// The older bulk path (POST /intake/bulk) filed every row at once with no
// documents at all; while documents are required it refuses TPA rows and
// points here.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

const (
	bulkDisputesMaxRows  = 500
	bulkDisputesMaxBytes = 512 << 20
	bulkFilerEvery       = 20 * time.Second
)

// zipEntry is one file inside a top-level folder of an uploaded ZIP.
type zipEntry struct {
	name string // path below the folder
	zf   *zip.File
}

// zipFolders groups a ZIP's files by top-level folder. Files at the root are
// returned separately (disputes.csv lives there).
func zipFolders(zr *zip.Reader) (folders map[string][]zipEntry, root map[string]*zip.File) {
	folders, root = map[string][]zipEntry{}, map[string]*zip.File{}
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() || strings.HasPrefix(zf.Name, "__MACOSX/") || strings.HasPrefix(baseName(zf.Name), "._") {
			continue
		}
		parts := strings.SplitN(strings.Trim(zf.Name, "/"), "/", 2)
		if len(parts) == 1 {
			root[strings.ToLower(parts[0])] = zf
			continue
		}
		if parts[0] != "" && parts[1] != "" {
			folders[parts[0]] = append(folders[parts[0]], zipEntry{parts[1], zf})
		}
	}
	return folders, root
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// lookupFolder finds a folder by name, ignoring case (spreadsheets and file
// managers disagree about case).
func lookupFolder(folders map[string][]zipEntry, name string) ([]zipEntry, bool) {
	if e, ok := folders[name]; ok {
		return e, true
	}
	for k, e := range folders {
		if strings.EqualFold(k, name) {
			return e, true
		}
	}
	return nil, false
}

// splitFolder: a merged.pdf makes the folder a MERGED package; otherwise the
// files go slot by slot.
func splitFolder(entries []zipEntry) (merged *zipEntry, discrete []zipEntry) {
	for i := range entries {
		if strings.EqualFold(baseName(entries[i].name), "merged.pdf") {
			merged = &entries[i]
		} else {
			discrete = append(discrete, entries[i])
		}
	}
	return merged, discrete
}

// fillTPAPackage stores a folder's documents into a package and announces
// them to doc-intel: the merged PDF to be split, or each file in its slot.
func (s *server) fillTPAPackage(r *http.Request, tenant, tpaID, pkgID string, merged *zipEntry, discrete []zipEntry) (attached, errs []string) {
	if merged != nil {
		rc, err := merged.zf.Open()
		if err != nil {
			return nil, []string{"merged.pdf unreadable"}
		}
		data, _ := io.ReadAll(io.LimitReader(rc, maxDocBytes))
		rc.Close()
		switch {
		case !bytes.HasPrefix(data, []byte("%PDF")):
			return nil, []string{"merged.pdf is not a PDF"}
		case contentPolicy("merged.pdf", data) != nil:
			return nil, []string{contentPolicy("merged.pdf", data).Error()}
		}
		if sig, ok := s.scanOrRefuse(discardWriter{}, bytes.NewReader(data), "file"); !ok {
			return nil, []string{"merged.pdf: malware scan rejected: " + sig}
		}
		objectKey := fmt.Sprintf("%s/tpa-packages/%s/merged-%s.enc", tenant, pkgID, newUUID())
		ct, err := s.vaultSealDoc(r, tenant, objectKey, data)
		if err == nil {
			_, err = s.docs.mc.PutObject(r.Context(), docBucket, objectKey, bytes.NewReader(ct), int64(len(ct)),
				minio.PutObjectOptions{UserMetadata: map[string]string{"tenant": tenant, "package": pkgID}})
		}
		if err != nil {
			return nil, []string{"merged.pdf store failed"}
		}
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.tpa_packages SET source_object_key=$3, status='VALIDATING', updated_at=now()
			WHERE tenant=$1 AND id=$2`, tenant, pkgID, objectKey)
		s.publish(r.Context(), tenant, "documents", map[string]any{
			"type": "tpa_doc.uploaded", "tenant": tenant, "package_id": pkgID,
			"object_key": objectKey, "filename": "merged.pdf", "merged": true, "at": time.Now().UTC(),
		})
		return []string{"merged.pdf"}, nil
	}
	for _, e := range discrete {
		slot := tpaSlotFromFilename(baseName(e.name))
		if slot == "" {
			errs = append(errs, e.name+": the file name does not say which of the six documents it is")
			continue
		}
		rc, err := e.zf.Open()
		if err != nil {
			errs = append(errs, e.name+": unreadable")
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(rc, maxDocBytes))
		rc.Close()
		if _, err := s.storePackageDoc(r, tenant, tpaID, pkgID, slot, baseName(e.name), data); err != nil {
			errs = append(errs, e.name+": "+err.Error())
			continue
		}
		attached = append(attached, slot)
	}
	return attached, errs
}

// attachPackageDocsToCase puts a filed package's accepted documents on the
// case, in its Intake folder, so the people working the case see what was
// checked. The stored (sealed) object is shared, not copied.
func (s *server) attachPackageDocsToCase(r *http.Request, tenant string, k onboardingKind, pkgID, caseID, uploadedBy string) int {
	rows, err := s.db.Query(r.Context(), fmt.Sprintf(`
		SELECT slot, object_key, coalesce(filename,'') FROM public.%s_package_docs
		WHERE tenant=$1 AND package_id=$2 AND status='ACCEPTED' AND object_key IS NOT NULL
		ORDER BY slot`, k.prefix), tenant, pkgID)
	if err != nil {
		slog.Error("attach package documents: query", "package", pkgID, "err", err)
		return 0
	}
	type doc struct{ slot, key, filename string }
	var docs []doc
	for rows.Next() {
		var d doc
		if rows.Scan(&d.slot, &d.key, &d.filename) == nil {
			docs = append(docs, d)
		}
	}
	rows.Close()
	n := 0
	for _, d := range docs {
		var size int64
		if st, err := s.docs.mc.StatObject(r.Context(), docBucket, d.key, minio.StatObjectOptions{}); err == nil {
			size = st.Size
		}
		label := strings.ReplaceAll(strings.ToLower(d.slot), "_", " ")
		filename := label + " — " + d.filename
		if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
			INSERT INTO tenant_%s.documents
			  (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version, scan_status, filename, folder)
			VALUES ($1,$2,$3,$4,'application/pdf',false,$5,1,'CLEAN',$6,'INTAKE')`, sanitizeTenant(tenant)),
			newUUID(), caseID, d.key, size, uploadedBy, filename); err != nil {
			slog.Error("attach package documents: insert", "package", pkgID, "slot", d.slot, "err", err)
			continue
		}
		n++
	}
	if n > 0 {
		s.logActivity(r.Context(), tenant, caseID, "DOCUMENTS_ATTACHED",
			fmt.Sprintf("%d checked filing documents attached from the onboarding package", n))
	}
	return n
}

// bulkDisputeRow is one line of disputes.csv and what became of it.
type bulkDisputeRow struct {
	Row        int      `json:"row"`
	DisputeRef string   `json:"dispute_ref"`
	Party      string   `json:"party"`
	Status     string   `json:"status"` // WAITING_DOCS | ERROR
	PackageID  string   `json:"package_id,omitempty"`
	Mode       string   `json:"mode,omitempty"`
	Attached   []string `json:"attached,omitempty"`
	Errors     []string `json:"errors,omitempty"`
}

// parseDollars reads "1980", "1,980.00" or "$1980.5" as cents.
func parseDollars(v string) (int64, bool) {
	v = strings.NewReplacer("$", "", ",", "", " ", "").Replace(strings.TrimSpace(v))
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return int64(f*100 + 0.5), true
}

// tpaBulkDisputes: POST /tpa/disputes/bulk (multipart: file=ZIP, batch_ref).
func (s *server) tpaBulkDisputes(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, bulkDisputesMaxBytes)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, `{"error":"the ZIP is too large (512 MB at most) or the upload was cut off"}`, http.StatusRequestEntityTooLarge)
		return
	}
	batchRef := strings.TrimSpace(r.FormValue("batch_ref"))
	if batchRef == "" || len(batchRef) > 128 {
		http.Error(w, `{"error":"batch_ref required (up to 128 characters) — your own reference, so a retried upload files nothing twice"}`, http.StatusBadRequest)
		return
	}
	var existing string
	if s.db.QueryRow(r.Context(), `SELECT id::text FROM public.tpa_dispute_batches WHERE tenant=$1 AND tpa_id=$2 AND batch_ref=$3`,
		tenant, org.ID, batchRef).Scan(&existing) == nil {
		writeJSON(w, http.StatusOK, map[string]any{"batch_id": existing, "batch_ref": batchRef, "idempotent_replay": true,
			"note": "this batch_ref was already uploaded — nothing was filed twice; see its report"})
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file field required (the ZIP)"}`, http.StatusBadRequest)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, bulkDisputesMaxBytes))
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		http.Error(w, `{"error":"not a valid ZIP archive"}`, http.StatusBadRequest)
		return
	}
	folders, root := zipFolders(zr)
	csvFile, ok := root["disputes.csv"]
	if !ok {
		http.Error(w, `{"error":"disputes.csv is missing at the top of the ZIP"}`, http.StatusBadRequest)
		return
	}
	rc, err := csvFile.Open()
	if err != nil {
		http.Error(w, `{"error":"disputes.csv unreadable"}`, http.StatusBadRequest)
		return
	}
	records, err := csv.NewReader(io.LimitReader(rc, 8<<20)).ReadAll()
	rc.Close()
	if err != nil || len(records) < 2 {
		http.Error(w, `{"error":"disputes.csv needs a header row and at least one dispute"}`, http.StatusBadRequest)
		return
	}
	col := map[string]int{}
	for i, h := range records[0] {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")))] = i
	}
	for _, need := range []string{"dispute_ref", "party_name", "disputed_amount"} {
		if _, ok := col[need]; !ok {
			http.Error(w, fmt.Sprintf(`{"error":"disputes.csv must have the columns dispute_ref, party_name, disputed_amount (missing %s); contact_name and notes are optional"}`, need), http.StatusBadRequest)
			return
		}
	}
	if len(records)-1 > bulkDisputesMaxRows {
		http.Error(w, fmt.Sprintf(`{"error":"at most %d disputes per ZIP — split the file"}`, bulkDisputesMaxRows), http.StatusBadRequest)
		return
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}

	var batchID string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.tpa_dispute_batches (tenant, tpa_id, submitter, batch_ref) VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant, tpa_id, batch_ref) DO NOTHING RETURNING id::text`,
		tenant, org.ID, p.Subject, batchRef).Scan(&batchID); err != nil {
		http.Error(w, `{"error":"this batch_ref is being uploaded already — see its report"}`, http.StatusConflict)
		return
	}
	seen := map[string]bool{}
	report := make([]bulkDisputeRow, 0, len(records)-1)
	waiting, failed := 0, 0
	for i, rec := range records[1:] {
		row := bulkDisputeRow{Row: i + 2, DisputeRef: get(rec, "dispute_ref"), Party: get(rec, "party_name"), Status: "ERROR"}
		fail := func(msg string) {
			row.Errors = append(row.Errors, msg)
			report = append(report, row)
			failed++
		}
		if row.DisputeRef == "" {
			fail("dispute_ref is empty")
			continue
		}
		if seen[strings.ToLower(row.DisputeRef)] {
			fail("dispute_ref appears twice in disputes.csv")
			continue
		}
		seen[strings.ToLower(row.DisputeRef)] = true
		amount, ok := parseDollars(get(rec, "disputed_amount"))
		if !ok {
			fail("disputed_amount must be a positive dollar amount")
			continue
		}
		var clientID, cstatus string
		var npi, tin, addr *string
		if err := s.db.QueryRow(r.Context(), `
			SELECT id::text, status, npi, tin, address FROM public.tpa_clients
			WHERE tenant=$1 AND tpa_id=$2 AND lower(party_name)=lower($3)`,
			tenant, org.ID, row.Party).Scan(&clientID, &cstatus, &npi, &tin, &addr); err != nil {
			fail("no client with this party_name on your roster — add it (or import the roster) first")
			continue
		}
		if cstatus != "ACTIVE" {
			fail("this client is suspended")
			continue
		}
		if npi == nil || *npi == "" || tin == nil || *tin == "" || addr == nil || *addr == "" {
			fail("the client's profile (NPI, TIN, address) is not complete")
			continue
		}
		var rails int
		_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM public.tpa_payment_methods
			WHERE tenant=$1 AND client_id=$2 AND verified AND status='ACTIVE'`, tenant, clientID).Scan(&rails)
		if rails == 0 {
			fail("the client has no verified payment method")
			continue
		}
		entries, ok := lookupFolder(folders, row.DisputeRef)
		if !ok || len(entries) == 0 {
			fail("no folder named " + row.DisputeRef + " with this dispute's documents in the ZIP")
			continue
		}
		merged, discrete := splitFolder(entries)
		row.Mode = "DISCRETE"
		if merged != nil {
			row.Mode = "MERGED"
		}
		if err := s.db.QueryRow(r.Context(), `
			INSERT INTO public.tpa_packages (tenant, tpa_id, client_id, mode) VALUES ($1,$2,$3,$4) RETURNING id::text`,
			tenant, org.ID, clientID, row.Mode).Scan(&row.PackageID); err != nil {
			fail("could not start this dispute's package")
			continue
		}
		row.Attached, row.Errors = s.fillTPAPackage(r, tenant, org.ID, row.PackageID, merged, discrete)
		if len(row.Attached) == 0 {
			fail("none of the documents could be stored")
			continue
		}
		if _, err := s.db.Exec(r.Context(), `
			INSERT INTO public.tpa_dispute_batch_items
			  (tenant, batch_id, row_no, dispute_ref, client_id, package_id, disputed_amount_cents, contact_name, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''))`,
			tenant, batchID, row.Row, row.DisputeRef, clientID, row.PackageID, amount,
			get(rec, "contact_name"), get(rec, "notes")); err != nil {
			fail("could not record the dispute")
			continue
		}
		row.Status = "WAITING_DOCS"
		report = append(report, row)
		waiting++
	}
	// Rows refused here are kept in the batch too, so its report is complete.
	for _, row := range report {
		if row.Status == "ERROR" {
			_, _ = s.db.Exec(r.Context(), `
				INSERT INTO public.tpa_dispute_batch_items (tenant, batch_id, row_no, dispute_ref, status, error)
				VALUES ($1,$2,$3,$4,'ERROR',$5) ON CONFLICT (batch_id, dispute_ref) DO NOTHING`,
				tenant, batchID, row.Row, fmt.Sprintf("%s (row %d)", row.DisputeRef, row.Row),
				strings.Join(row.Errors, "; "))
		}
	}
	s.logAudit(r.Context(), tenant, "", "TPA_BULK_DISPUTES", map[string]any{
		"tpa_id": org.ID, "batch_id": batchID, "batch_ref": batchRef, "rows": len(report), "waiting": waiting, "refused": failed})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"batch_id": batchID, "batch_ref": batchRef, "rows": report, "waiting_for_documents": waiting, "refused": failed,
		"note": "each dispute is filed by itself as soon as its six documents are accepted — follow the batch report",
	})
}

// tpaBulkDisputeBatches: GET /tpa/disputes/bulk -- this TPA's batches with
// how far each has got.
func (s *server) tpaBulkDisputeBatches(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	rows, err := s.queryRows(r, `
		SELECT b.id::text AS id, b.batch_ref, b.created_at::text AS created_at,
		       count(i.*) AS rows,
		       count(*) FILTER (WHERE i.status='FILED') AS filed,
		       count(*) FILTER (WHERE i.status IN ('WAITING_DOCS','FILING')) AS waiting,
		       count(*) FILTER (WHERE i.status='ERROR') AS refused
		FROM public.tpa_dispute_batches b
		LEFT JOIN public.tpa_dispute_batch_items i ON i.batch_id=b.id
		WHERE b.tenant=$1 AND b.tpa_id=$2
		GROUP BY b.id ORDER BY b.created_at DESC LIMIT 100`, tenant, org.ID)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": rows})
}

// tpaBulkDisputeBatch: GET /tpa/disputes/bulk/{batchId} -- every row with its
// package's state and, once filed, its case number.
func (s *server) tpaBulkDisputeBatch(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
		return
	}
	rows, err := s.queryRows(r, `
		SELECT i.row_no, i.dispute_ref, coalesce(c.party_name,'') AS party, i.status, coalesce(i.error,'') AS error,
		       i.disputed_amount_cents, coalesce(i.package_id::text,'') AS package_id, coalesce(pk.mode,'') AS mode,
		       coalesce(pk.status,'') AS package_status,
		       (SELECT count(*) FROM public.tpa_package_docs d WHERE d.package_id=i.package_id AND d.status='ACCEPTED') AS accepted,
		       (SELECT count(*) FROM public.tpa_package_docs d WHERE d.package_id=i.package_id AND d.status='NEEDS_REVIEW') AS needs_review,
		       (SELECT count(*) FROM public.tpa_package_docs d WHERE d.package_id=i.package_id AND d.status='REJECTED') AS rejected,
		       coalesce(i.case_number,'') AS case_number, coalesce(i.case_id,'') AS case_id
		FROM public.tpa_dispute_batch_items i
		JOIN public.tpa_dispute_batches b ON b.id=i.batch_id
		LEFT JOIN public.tpa_clients c ON c.id=i.client_id
		LEFT JOIN public.tpa_packages pk ON pk.id=i.package_id
		WHERE i.tenant=$1 AND b.tpa_id=$2 AND b.id::text=$3
		ORDER BY i.row_no`, tenant, org.ID, chi.URLParam(r, "batchId"))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": chi.URLParam(r, "batchId"), "items": rows})
}

// runBulkDisputeFiler files every waiting bulk row whose package is READY.
// Each row is claimed first (WAITING_DOCS -> FILING in one UPDATE), so two
// case-api replicas never file the same dispute.
func (s *server) runBulkDisputeFiler(stop <-chan struct{}) {
	t := time.NewTicker(bulkFilerEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if n := s.fileReadyBulkDisputes(); n > 0 {
			slog.Info("bulk disputes filed", "count", n)
		}
	}
}

func (s *server) fileReadyBulkDisputes() int {
	ctx := context.Background()
	rows, err := s.db.Query(ctx, `
		SELECT i.id::text FROM public.tpa_dispute_batch_items i
		JOIN public.tpa_packages pk ON pk.id=i.package_id
		WHERE i.status='WAITING_DOCS' AND pk.status='READY'
		ORDER BY i.created_at LIMIT 50`)
	if err != nil {
		return 0
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	filed := 0
	for _, id := range ids {
		var tenant, tpaID, clientID, pkgID, contact, notes string
		var amount int64
		if err := s.db.QueryRow(ctx, `
			UPDATE public.tpa_dispute_batch_items i SET status='FILING'
			FROM public.tpa_dispute_batches b
			WHERE i.id::text=$1 AND i.status='WAITING_DOCS' AND b.id=i.batch_id
			RETURNING i.tenant, b.tpa_id::text, i.client_id::text, i.package_id::text, i.disputed_amount_cents,
			          coalesce(i.contact_name,''), coalesce(i.notes,'')`, id).
			Scan(&tenant, &tpaID, &clientID, &pkgID, &amount, &contact, &notes); err != nil {
			continue // another replica claimed it
		}
		caseNumber, ferr := s.fileOneBulkDispute(ctx, tenant, tpaID, clientID, pkgID, contact, notes, amount)
		if ferr != nil {
			_, _ = s.db.Exec(ctx, `UPDATE public.tpa_dispute_batch_items SET status='ERROR', error=$2 WHERE id::text=$1`, id, ferr.Error())
			slog.Error("bulk dispute filing failed", "item", id, "err", ferr)
			continue
		}
		_, _ = s.db.Exec(ctx, `
			UPDATE public.tpa_dispute_batch_items SET status='FILED', filed_at=now(), case_number=$2,
			       case_id=(SELECT case_id FROM public.tpa_packages WHERE id::text=$3)
			WHERE id::text=$1`, id, caseNumber, pkgID)
		filed++
	}
	return filed
}

// fileOneBulkDispute re-checks what may have changed since the upload (the
// client suspended, its payment method removed, the package used elsewhere)
// and files the dispute.
func (s *server) fileOneBulkDispute(ctx context.Context, tenant, tpaID, clientID, pkgID, contact, notes string, amount int64) (string, error) {
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	var org tpaOrg
	if err := s.db.QueryRow(ctx, `
		SELECT id::text, name, coalesce(contact_name,''), coalesce(contact_email,''), status FROM public.tpas WHERE tenant=$1 AND id::text=$2`,
		tenant, tpaID).Scan(&org.ID, &org.Name, &org.ContactName, &org.ContactEmail, &org.Status); err != nil {
		return "", fmt.Errorf("TPA organization not found")
	}
	if org.Status != "ACTIVE" {
		return "", fmt.Errorf("the TPA organization is %s", org.Status)
	}
	var partyName, partyType, cstatus string
	if err := s.db.QueryRow(ctx, `SELECT party_name, party_type, status FROM public.tpa_clients WHERE tenant=$1 AND id::text=$2`,
		tenant, clientID).Scan(&partyName, &partyType, &cstatus); err != nil {
		return "", fmt.Errorf("client not found")
	}
	if cstatus != "ACTIVE" {
		return "", fmt.Errorf("the client was suspended before filing")
	}
	var rails int
	_ = s.db.QueryRow(ctx, `SELECT count(*) FROM public.tpa_payment_methods WHERE tenant=$1 AND client_id::text=$2 AND verified AND status='ACTIVE'`,
		tenant, clientID).Scan(&rails)
	if rails == 0 {
		return "", fmt.Errorf("the client has no verified payment method any more")
	}
	var pkgStatus string
	_ = s.db.QueryRow(ctx, `SELECT status FROM public.tpa_packages WHERE tenant=$1 AND id::text=$2`, tenant, pkgID).Scan(&pkgStatus)
	if pkgStatus != "READY" {
		return "", fmt.Errorf("the package is %s, not READY", pkgStatus)
	}
	_, caseNumber, _, err := s.fileTPADispute(r, tenant, &org, clientID, partyName, partyType, pkgID, contact, notes, amount)
	if err != nil {
		return "", fmt.Errorf("case creation failed: %v", err)
	}
	return caseNumber, nil
}
