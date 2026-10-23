package main

// tpa_bulk_disputes.go — a TPA files many disputes at once, each with its
// own documents.
//
// One ZIP:
//
//	disputes.csv          dispute_ref,provider_group,party_name,disputed_amount,other_party_name,
//	                      other_party_email,... (provider_group: optional, the TPA's group the
//	                      provider belongs to -- it tells apart two providers of the same name)
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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
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
		data, _ := io.ReadAll(io.LimitReader(rc, maxDocBytes+1)) // one byte past the limit: an oversized entry is refused, not cut short
		rc.Close()
		switch {
		case !bytes.HasPrefix(data, []byte("%PDF")):
			return nil, []string{"merged.pdf is not a PDF"}
		case docPolicy("merged.pdf", data) != nil:
			return nil, []string{docPolicy("merged.pdf", data).Error()}
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
		// A file that is none of the six is one more of the dispute's documents, of the kind its name suggests.
		slot := tpaSlotFromFilename(baseName(e.name))
		if slot == "" {
			slot = newMoreSlot(moreKindFromFilename(baseName(e.name)))
		}
		rc, err := e.zf.Open()
		if err != nil {
			errs = append(errs, e.name+": unreadable")
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(rc, maxDocBytes+1)) // one byte past the limit: an oversized entry is refused, not cut short
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
		SELECT slot, object_key, coalesce(filename,''), coalesce(fields,'{}'::jsonb), status, coalesce(reasons::text,'[]') FROM public.%s_package_docs
		WHERE tenant=$1 AND package_id=$2 AND object_key IS NOT NULL
		ORDER BY slot`, k.prefix), tenant, pkgID)
	if err != nil {
		slog.Error("attach package documents: query", "package", pkgID, "err", err)
		return 0
	}
	type doc struct {
		slot, key, filename, status, reasons string
		fields                               []byte
	}
	// Every document of the set goes onto the case -- flagged ones too (the Reviewer is told which) -- each file once.
	var docs []doc
	seenKey := map[string]bool{}
	var flagged []string
	for rows.Next() {
		var d doc
		if rows.Scan(&d.slot, &d.key, &d.filename, &d.fields, &d.status, &d.reasons) == nil && !seenKey[d.key] {
			seenKey[d.key] = true
			docs = append(docs, d)
			if d.status != "ACCEPTED" {
				why := ""
				var rs []map[string]any
				if json.Unmarshal([]byte(d.reasons), &rs) == nil && len(rs) > 0 {
					if v, ok := rs[0]["detail"].(string); ok {
						why = v
					} else if v, ok := rs[0]["code"].(string); ok {
						why = v
					}
				}
				if why != "" {
					why = " (" + why + ")"
				}
				flagged = append(flagged, strings.ReplaceAll(strings.ToLower(d.slot), "_", " ")+": "+d.filename+why)
			}
		}
	}
	rows.Close()
	if len(flagged) > 0 {
		defer s.logActivity(r.Context(), tenant, caseID, "FILING_DOCS_FLAGGED",
			fmt.Sprintf("%d document%s filed with this dispute did not match what the automatic check expected — look at them when confirming the filing packet: %s",
				len(flagged), map[bool]string{true: "", false: "s"}[len(flagged) == 1], truncate(strings.Join(flagged, "; "), 1500)))
	}
	n := 0
	for _, d := range docs {
		var size int64
		if st, err := s.docs.mc.StatObject(r.Context(), docBucket, d.key, minio.StatObjectOptions{}); err == nil {
			size = st.Size
		}
		label := strings.ReplaceAll(strings.ToLower(d.slot), "_", " ")
		if k := moreDocKind(d.slot); k != "" {
			label = strings.ReplaceAll(strings.ToLower(k), "_", " ")
		}
		filename := label + " — " + d.filename
		docID := newUUID()
		if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
			INSERT INTO tenant_%s.documents
			  (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version, scan_status, filename, folder)
			VALUES ($1,$2,$3,$4,'application/pdf',false,$5,1,'CLEAN',$6,'INTAKE')`, sanitizeTenant(tenant)),
			docID, caseID, d.key, size, uploadedBy, filename); err != nil {
			slog.Error("attach package documents: insert", "package", pkgID, "slot", d.slot, "err", err)
			continue
		}
		// The document was already read and accepted for its slot: record that
		// as its analysis. Without one, the unread-documents sweep sent it to
		// doc-intel again, whose PDF library crashed on these one-page
		// segments and took the whole document queue down with it.
		s.recordPackageAnalysis(r.Context(), tenant, docID, caseID, d.slot, pkgID, d.fields)
		n++
	}
	if n > 0 {
		s.logActivity(r.Context(), tenant, caseID, "DOCUMENTS_ATTACHED",
			fmt.Sprintf("%d checked filing documents attached from the onboarding package", n))
	}
	slots := make([]packageSlotFacts, 0, len(docs))
	for _, d := range docs {
		slots = append(slots, packageSlotFacts{Slot: d.slot, Fields: d.fields})
	}
	if facts := packageCaseFacts(slots); len(facts) > 0 {
		b, _ := json.Marshal(facts)
		// Only where the case has no value yet: a value already on the case
		// (entered by a person) beats a document's reading.
		_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
			UPDATE tenant_%s.cases SET details = $2::jsonb || coalesce(details,'{}'::jsonb) WHERE id=$1`,
			sanitizeTenant(tenant)), caseID, b)
	}
	return n
}

// packageSlotFacts is one filed slot and the eligibility facts doc-intel read
// off it when the document set was checked (tpa_docs.py, package_fields).
type packageSlotFacts struct {
	Slot   string
	Fields []byte
}

// packageCaseFacts picks the provider type and network status for the case
// from its filed documents: a value a document states outright beats one
// worked out from the provider's name (kept with its basis, so the
// eligibility review asks for a second look), and the EOB is read first.
// Dates are not copied here: they stay with each document's analysis, where
// the eligibility review takes the last decision date and the earliest date
// of service across all of them. Pure, so it is tested.
func packageCaseFacts(slots []packageSlotFacts) map[string]any {
	rank := func(slot string) int {
		switch slot {
		case "EOB":
			return 0
		case "ITEMIZED_BILL":
			return 1
		case "OFFER_SUBSTANTIATION":
			return 2
		}
		return 3
	}
	sorted := append([]packageSlotFacts(nil), slots...)
	sort.SliceStable(sorted, func(i, j int) bool { return rank(sorted[i].Slot) < rank(sorted[j].Slot) })
	out := map[string]any{}
	var inferredType, inferredBasis string
	for _, sl := range sorted {
		var f map[string]any
		if json.Unmarshal(sl.Fields, &f) != nil {
			continue
		}
		if pt, _ := f["provider_type"].(string); pt != "" && out["provider_type"] == nil {
			if basis, _ := f["provider_type_basis"].(string); basis != "" {
				if inferredType == "" {
					inferredType, inferredBasis = pt, basis
				}
			} else {
				out["provider_type"] = pt
			}
		}
		if c, ok := f["contracted"].(bool); ok && out["contracted"] == nil {
			out["contracted"] = c
		}
	}
	if out["provider_type"] == nil && inferredType != "" {
		out["provider_type"] = inferredType
		out["provider_type_basis"] = "inferred: " + inferredBasis
	}
	if c, ok := out["contracted"].(bool); ok {
		out["out_of_network"] = map[bool]string{true: "No", false: "Yes"}[c]
	}
	return out
}

// packageDocType names an accepted slot in doc-intel's document types.
func packageDocType(slot string) string {
	if k := moreDocKind(slot); k != "" {
		return strings.ToLower(k)
	}
	switch slot {
	case "EOB":
		return "eob"
	case "ITEMIZED_BILL", "OFFER_SUBSTANTIATION":
		return "idr_claim"
	}
	return strings.ToLower(slot)
}

// The facts read off the document when its set was checked go in as its
// normalized fields -- the same place a case document's analysis keeps them,
// so the eligibility review finds the dates of service and the plan's
// decision date on a filed set too.
func (s *server) recordPackageAnalysis(ctx context.Context, tenant, docID, caseID, slot, pkgID string, fields []byte) {
	if len(fields) == 0 {
		fields = []byte("{}")
	}
	_, _ = s.db.Exec(ctx, `
		INSERT INTO public.doc_analysis (doc_id, tenant, case_id, status, doc_type, result, analyzed_at)
		VALUES ($1::uuid,$2,$3::uuid,'ANALYZED',$4,
		        jsonb_build_object('source','onboarding_package','slot',$5::text,'package_id',$6::text,'verdict','ACCEPTED',
		                           'normalized',$7::jsonb), now())
		ON CONFLICT DO NOTHING`, docID, tenant, caseID, packageDocType(slot), slot, pkgID, fields)
}

// backfillPackageAnalyses gives every already-attached package document its
// analysis record (documents attached before recordPackageAnalysis existed).
// Runs once at start; idempotent.
func (s *server) backfillPackageAnalyses() {
	ctx := context.Background()
	rows, err := s.db.Query(ctx, `SELECT substr(schema_name, 8) FROM information_schema.schemata WHERE schema_name LIKE 'tenant\_%'`)
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
	total := int64(0)
	for _, t := range tenants {
		tag, err := s.db.Exec(ctx, fmt.Sprintf(`
			INSERT INTO public.doc_analysis (doc_id, tenant, case_id, status, doc_type, result, analyzed_at)
			SELECT d.id, $1, d.case_id::uuid, 'ANALYZED', 'onboarding_package',
			       jsonb_build_object('source','onboarding_package','verdict','ACCEPTED'), now()
			FROM tenant_%s.documents d
			WHERE d.folder='INTAKE' AND (d.uploaded_by LIKE 'tpa:%%' OR d.uploaded_by LIKE 'health-plan:%%')
			  AND NOT EXISTS (SELECT 1 FROM public.doc_analysis a WHERE a.doc_id=d.id AND a.status NOT IN ('QUEUED','STALLED'))
			ON CONFLICT (doc_id) DO UPDATE SET status='ANALYZED', result=EXCLUDED.result, analyzed_at=now()`, sanitizeTenant(t)), t)
		if err == nil {
			total += tag.RowsAffected()
		} else {
			slog.Error("backfill package analyses", "tenant", t, "err", err)
		}
	}
	if total > 0 {
		slog.Info("package documents given their analysis record", "count", total)
	}
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
	raw, err := io.ReadAll(io.LimitReader(f, zipMaxBytes+1))
	if err != nil {
		http.Error(w, `{"error":"read failed"}`, http.StatusBadRequest)
		return
	}
	zr, ok := s.openUploadedZip(w, raw) // size, readable, entries, virus scan
	if !ok {
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
	for _, need := range []string{"dispute_ref", "party_name", "disputed_amount", "other_party_name", "other_party_email"} {
		if _, ok := col[need]; !ok {
			http.Error(w, fmt.Sprintf(`{"error":"disputes.csv must have the columns dispute_ref, party_name, disputed_amount, other_party_name, other_party_email (missing %s); provider_group, other_party_naic, contact_name, notes, provider_type and in_network (yes/no) are optional"}`, need), http.StatusBadRequest)
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
		other := respondent{Side: "HEALTH_PLAN", Name: get(rec, "other_party_name"), Email: get(rec, "other_party_email"), NAIC: get(rec, "other_party_naic")}
		if msg := other.check(); msg != "" {
			fail(msg)
			continue
		}
		if msg := emailReachMsg(r.Context(), "other_party_email", other.Email); msg != "" {
			fail(msg)
			continue
		}
		answers, msg := s.filerEligibilityAnswers(r, tenant, get(rec, "provider_type"), get(rec, "in_network"))
		if msg != "" {
			fail(msg)
			continue
		}
		answersJSON, _ := json.Marshal(answers)
		clientID, cstatus, npi, tin, addr, why := s.bulkClient(r.Context(), tenant, org.ID, row.Party, get(rec, "provider_group"))
		if why != "" {
			fail(why)
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
		if why := s.clientNPIBlock(r.Context(), tenant, clientID); why != "" {
			fail(why)
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
		if zipHasSampleDocs(entries) {
			fail("this folder still holds the sample documents — replace them with this dispute's real documents")
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
			  (tenant, batch_id, row_no, dispute_ref, client_id, package_id, disputed_amount_cents, contact_name, notes,
			   other_party_name, other_party_email, other_party_naic, eligibility_answers)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10,$11,NULLIF($12,''),$13::jsonb)`,
			tenant, batchID, row.Row, row.DisputeRef, clientID, row.PackageID, amount,
			get(rec, "contact_name"), get(rec, "notes"), other.Name, other.Email, other.NAIC, answersJSON); err != nil {
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
		       count(*) FILTER (WHERE i.status='ERROR') AS refused,
		       (SELECT count(*) FROM public.tpa_dispute_batch_items x JOIN public.invoices f ON f.tenant=x.tenant AND f.case_id=x.case_id AND f.kind='INITIAL_FEE' AND f.status='OPEN'
		         WHERE x.batch_id=b.id AND x.status='FILED') AS unpaid,
		       (SELECT coalesce(sum(f.amount_cents),0) FROM public.tpa_dispute_batch_items x JOIN public.invoices f ON f.tenant=x.tenant AND f.case_id=x.case_id AND f.kind='INITIAL_FEE' AND f.status='OPEN'
		         WHERE x.batch_id=b.id AND x.status='FILED') AS unpaid_cents
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
	rows, err := s.queryRows(r, fmt.Sprintf(`
		SELECT i.row_no, i.dispute_ref, coalesce(c.party_name,'') AS party, coalesce(pg.name,'') AS provider_group, i.status, coalesce(i.error,'') AS error,
		       coalesce(i.notes,'') AS notes,
		       coalesce(rs.name, i.other_party_name, '') AS other_party, coalesce(rs.response_status,'') AS response_status,
		       coalesce(cs.program_dates->>'date_health_plan_response_due','') AS response_due,
		       coalesce(cs.program_dates->>'plan_notified_at','') AS notified_at,
		       coalesce(least(rs.notified_at, rs.emailed_at)::text,'') AS told_at,
		       coalesce(cs.status,'') AS case_status, coalesce(cs.internal_status,'') AS internal_status,
		       coalesce(el.result,'') AS eligibility, coalesce(el.reason,'') AS eligibility_reason,
		       coalesce(el.created_at::text,'') AS eligibility_at, coalesce(el.override,false) AS eligibility_override,
		       (SELECT coalesce(json_agg(json_build_object('slot', d.slot, 'status', d.status, 'filename', coalesce(d.filename,'')) ORDER BY d.slot), '[]'::json)
		          FROM public.tpa_package_docs d WHERE d.package_id=i.package_id) AS docs,
		       i.disputed_amount_cents, coalesce(i.package_id::text,'') AS package_id, coalesce(pk.mode,'') AS mode,
		       coalesce(pk.status,'') AS package_status,
		       (SELECT count(*) FROM public.tpa_package_docs d WHERE d.package_id=i.package_id AND d.status='ACCEPTED') AS accepted,
		       (SELECT count(*) FROM public.tpa_package_docs d WHERE d.package_id=i.package_id AND d.status='NEEDS_REVIEW') AS needs_review,
		       (SELECT count(*) FROM public.tpa_package_docs d WHERE d.package_id=i.package_id AND d.status='REJECTED') AS rejected,
		       coalesce(i.case_number,'') AS case_number, coalesce(i.case_id,'') AS case_id,
		       coalesce((SELECT f.status FROM public.invoices f WHERE f.tenant=i.tenant AND f.case_id=i.case_id AND f.kind='INITIAL_FEE' LIMIT 1),'') AS fee_status,
		       coalesce((SELECT f.due_date::text FROM public.invoices f WHERE f.tenant=i.tenant AND f.case_id=i.case_id AND f.kind='INITIAL_FEE' LIMIT 1),'') AS fee_due,
		       coalesce((SELECT f.amount_cents FROM public.invoices f WHERE f.tenant=i.tenant AND f.case_id=i.case_id AND f.kind='INITIAL_FEE' LIMIT 1),0) AS fee_cents
		FROM public.tpa_dispute_batch_items i
		JOIN public.tpa_dispute_batches b ON b.id=i.batch_id
		LEFT JOIN public.tpa_clients c ON c.id=i.client_id
		LEFT JOIN public.tpa_provider_groups pg ON pg.id=c.group_id
		LEFT JOIN public.tpa_packages pk ON pk.id=i.package_id
		LEFT JOIN tenant_%s.cases cs ON cs.id::text=i.case_id
		LEFT JOIN public.case_respondents rs ON rs.tenant=i.tenant AND rs.case_id=i.case_id
		LEFT JOIN LATERAL (SELECT e.result, e.reason, e.created_at, e.override FROM public.eligibility_reviews e
		                   WHERE e.tenant=i.tenant AND e.case_id=i.case_id ORDER BY e.created_at DESC LIMIT 1) el ON true
		WHERE i.tenant=$1 AND b.tpa_id=$2 AND b.id::text=$3
		ORDER BY i.row_no`, sanitizeTenant(tenant)), tenant, org.ID, chi.URLParam(r, "batchId"))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.attachFeeStates(r.Context(), tenant, rows)
	s.fillResponseDue(r, tenant, rows)
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
	s.healStuckPackages(ctx)
	s.sweepExpiredUploads(ctx)
	s.reconcileOpenFeeCheckouts(ctx)
	s.reconcilePendingRefunds(ctx)
	s.reconcilePendingSetups(ctx)
	s.recheckClientNPIs(ctx)
	s.startVerificationSweep()
	rows, err := s.db.Query(ctx, `
		SELECT i.id::text FROM public.tpa_dispute_batch_items i
		JOIN public.tpa_packages pk ON pk.id=i.package_id
		WHERE i.status='WAITING_DOCS' AND pk.status<>'FILED'
		  AND EXISTS (SELECT 1 FROM public.tpa_package_docs d WHERE d.package_id=pk.id)
		  AND NOT EXISTS (SELECT 1 FROM public.tpa_package_docs d WHERE d.package_id=pk.id AND d.status IN ('PENDING','ANALYZING'))
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
	type batchKey struct{ tenant, batch string }
	filedIn := map[batchKey][]string{}
	for _, id := range ids {
		var tenant, tpaID, clientID, pkgID, contact, notes, batchID, oName, oEmail, oNAIC string
		var amount int64
		var answersJSON []byte
		if err := s.db.QueryRow(ctx, `
			UPDATE public.tpa_dispute_batch_items i SET status='FILING'
			FROM public.tpa_dispute_batches b
			WHERE i.id::text=$1 AND i.status='WAITING_DOCS' AND b.id=i.batch_id
			RETURNING i.tenant, b.tpa_id::text, i.client_id::text, i.package_id::text, i.disputed_amount_cents,
			          coalesce(i.contact_name,''), coalesce(i.notes,''), i.batch_id::text,
			          coalesce(i.other_party_name,''), coalesce(i.other_party_email,''), coalesce(i.other_party_naic,''),
			          coalesce(i.eligibility_answers,'{}'::jsonb)`, id).
			Scan(&tenant, &tpaID, &clientID, &pkgID, &amount, &contact, &notes, &batchID, &oName, &oEmail, &oNAIC, &answersJSON); err != nil {
			continue // another replica claimed it
		}
		var answers map[string]any
		_ = json.Unmarshal(answersJSON, &answers)
		caseNumber, ferr := s.fileOneBulkDispute(ctx, tenant, tpaID, clientID, pkgID, contact, notes, amount,
			respondent{Side: "HEALTH_PLAN", Name: oName, Email: oEmail, NAIC: oNAIC}, answers)
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
		filedIn[batchKey{tenant, batchID}] = append(filedIn[batchKey{tenant, batchID}], caseNumber)
	}
	// One email per batch for this round, not one payment email per dispute.
	for k, nums := range filedIn {
		s.emailBatchFiled(ctx, k.tenant, k.batch, nums)
	}
	return filed
}

// fileOneBulkDispute re-checks what may have changed since the upload (the
// client suspended, its payment method removed, the package used elsewhere)
// and files the dispute.
func (s *server) fileOneBulkDispute(ctx context.Context, tenant, tpaID, clientID, pkgID, contact, notes string, amount int64, other respondent, answers map[string]any) (string, error) {
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
	if why := s.clientNPIBlock(ctx, tenant, clientID); why != "" {
		return "", fmt.Errorf("%s", why)
	}
	var rails int
	_ = s.db.QueryRow(ctx, `SELECT count(*) FROM public.tpa_payment_methods WHERE tenant=$1 AND client_id::text=$2 AND verified AND status='ACTIVE'`,
		tenant, clientID).Scan(&rails)
	if rails == 0 {
		return "", fmt.Errorf("the client has no verified payment method any more")
	}
	if ok, pkgStatus := s.packageFileable(ctx, tenant, "tpa", pkgID); !ok {
		return "", fmt.Errorf("the package is %s, not READY", pkgStatus)
	}
	caseID, caseNumber, _, err := s.fileTPADisputeOpt(r, tenant, &org, clientID, partyName, partyType, pkgID, contact, notes, amount, true)
	if err != nil {
		return "", fmt.Errorf("case creation failed: %v", err)
	}
	if other.Name != "" {
		s.recordRespondent(r, tenant, caseID, other)
	}
	s.applyFilerEligibilityAnswers(ctx, tenant, caseID, answers)
	s.logAudit(ctx, tenant, caseID, "BULK_DISPUTE_FILED", map[string]any{
		"tpa_id": tpaID, "client_id": clientID, "package_id": pkgID, "case_number": caseNumber, "by": "system:bulk-filer",
		"notice": caseNumber + " filed for " + partyName})
	return caseNumber, nil
}

// healStuckPackages recounts waiting bulk rows' packages whose six documents
// are all accepted but which still say VALIDATING (a recount lost to a race
// between two document verdicts), so the row files on this round.
func (s *server) healStuckPackages(ctx context.Context) {
	rows, err := s.db.Query(ctx, `
		SELECT i.tenant, i.package_id::text FROM public.tpa_dispute_batch_items i
		JOIN public.tpa_packages pk ON pk.id=i.package_id
		WHERE i.status='WAITING_DOCS' AND pk.status='VALIDATING'
		  AND (SELECT count(*) FROM public.tpa_package_docs d WHERE d.package_id=pk.id AND d.status='ACCEPTED' AND d.slot = ANY($2)) >= $1`, len(tpaSlots), tpaSlots)
	if err != nil {
		return
	}
	type stuck struct{ tenant, pkg string }
	var list []stuck
	for rows.Next() {
		var x stuck
		if rows.Scan(&x.tenant, &x.pkg) == nil {
			list = append(list, x)
		}
	}
	rows.Close()
	if len(list) == 0 {
		return
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	for _, x := range list {
		if st := s.refreshOnboardingPackage(r, x.tenant, onboardingKinds["tpa"], x.pkg); st == "READY" {
			slog.Info("package recounted to READY", "package", x.pkg)
		}
	}
}

// bulkClient: the roster client a disputes.csv row names -- by party_name, within its provider_group when the row
// gives one. Two providers of the same name in different groups need the group to tell them apart. why != "":
// refused, with why.
func (s *server) bulkClient(ctx context.Context, tenant, tpaID, party, group string) (id, status string, npi, tin, addr *string, why string) {
	type cand struct {
		id, status, group string
		npi, tin, addr    *string
	}
	rows, err := s.db.Query(ctx, `
		SELECT c.id::text, c.status, c.npi, c.tin, c.address, coalesce(g.name,'')
		FROM public.tpa_clients c LEFT JOIN public.tpa_provider_groups g ON g.id=c.group_id
		WHERE c.tenant=$1 AND c.tpa_id=$2 AND lower(c.party_name)=lower($3)`, tenant, tpaID, party)
	if err != nil {
		return "", "", nil, nil, nil, "the roster could not be read — try again"
	}
	var all []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.status, &c.npi, &c.tin, &c.addr, &c.group) == nil {
			all = append(all, c)
		}
	}
	rows.Close()
	if len(all) == 0 {
		return "", "", nil, nil, nil, "no client with this party_name on your roster — add it (or import the roster) first"
	}
	pick := all
	if group = strings.TrimSpace(group); group != "" {
		var exists bool
		_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.tpa_provider_groups WHERE tenant=$1 AND tpa_id=$2 AND lower(name)=lower($3))`,
			tenant, tpaID, group).Scan(&exists)
		if !exists {
			return "", "", nil, nil, nil, "no provider group named " + group + " — check the name on the Clients tab, or leave provider_group empty"
		}
		pick = nil
		for _, c := range all {
			if strings.EqualFold(c.group, group) {
				pick = append(pick, c)
			}
		}
		if len(pick) == 0 {
			in := all[0].group
			if in == "" {
				return "", "", nil, nil, nil, party + " is not in the provider group " + group + " (it is in no group)"
			}
			return "", "", nil, nil, nil, party + " is in the provider group " + in + ", not " + group
		}
	}
	if len(pick) > 1 {
		return "", "", nil, nil, nil, "more than one client is named " + party + " — say which with provider_group"
	}
	c := pick[0]
	return c.id, c.status, c.npi, c.tin, c.addr, ""
}
