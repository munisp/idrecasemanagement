// Author: Patrick Munis at newwave
// sopextras.go — smaller pieces of the FL AHCA workflow document.
//
//   - The Eligibility Checklist is filed on the case as a document each time
//     an eligibility review is recorded ("saves it to ShareFile").
//   - Ad hoc report: the agency asks for whatever it asks for, so the caller
//     picks the columns, the period and the statuses. Final report: every
//     case over the life of the contract, in the monthly layout.
//   - An 835 remittance (ERA) submitted as raw X12 text is converted to a
//     table, so nobody has to rebuild it in a spreadsheet by hand.
package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// storeGeneratedDocument files a document the system produced on a case:
// sealed through the vault and stored exactly like an upload, but with no
// analysis event (there is nothing to read out of our own output).
func (s *server) storeGeneratedDocument(r *http.Request, tenant, caseID, filename, contentType, folder string, raw []byte, by string) (string, error) {
	if s.docs == nil {
		return "", fmt.Errorf("document store not configured")
	}
	if !docFolders[folder] {
		folder = "GENERAL"
	}
	docID := newUUID()
	objectKey := fmt.Sprintf("%s/cases/%s/%s.enc", tenant, caseID, docID)
	ct, err := s.vaultSealDoc(r, tenant, objectKey, raw)
	if err != nil {
		return "", err
	}
	if _, err := s.docs.mc.PutObject(r.Context(), docBucket, objectKey, bytes.NewReader(ct), int64(len(ct)),
		minio.PutObjectOptions{ContentType: "application/octet-stream", UserMetadata: map[string]string{"tenant": tenant, "case": caseID}}); err != nil {
		return "", err
	}
	if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
		INSERT INTO tenant_%s.documents
		  (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version, scan_status, filename, folder, analysis_status)
		VALUES ($1,$2,$3,$4,$5,false,$6,1,'CLEAN',$7,$8,'NOT_APPLICABLE')`, sanitizeTenant(tenant)),
		docID, caseID, objectKey, len(raw), contentType, by, filename, folder); err != nil {
		return "", err
	}
	return docID, nil
}

// fileEligibilityChecklist renders the checklist for a review just recorded
// and files it on the case. Best-effort: the review is the record; a storage
// hiccup must not fail it.
func (s *server) fileEligibilityChecklist(r *http.Request, tenant, caseID, caseNumber, result, reason, decidedBy string, evidence map[string]any) {
	now := time.Now().UTC()
	page := eligibilityChecklistHTML(caseNumber, result, reason, decidedBy, now, evidence)
	name := fmt.Sprintf("Eligibility Checklist %s %s.html", caseNumber, now.Format("2006-01-02 1504"))
	if _, err := s.storeGeneratedDocument(r, tenant, caseID, name, "text/html", "INTAKE", []byte(page), decidedBy); err != nil {
		s.logActivity(r.Context(), tenant, caseID, "CHECKLIST_NOT_FILED", "Eligibility Checklist could not be filed on the case: "+err.Error())
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "CHECKLIST_FILED", name+" filed in the case documents (Intake)")
}

// ---- Ad hoc and final reports -------------------------------------------------

// flAllColumns: every reportable column, weekly first, then the monthly-only
// ones. Internal-only fields are not in either list, so they cannot be asked for.
func flAllColumns() []string {
	out := append([]string{}, flWeeklyHeaders...)
	for _, h := range flMonthlyHeaders {
		if !contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// flCell returns one case's value for a column by its header.
func flCell(c *flCaseRow, header string) (string, bool) {
	for i, h := range flWeeklyHeaders {
		if h == header {
			return c.weeklyRow()[i], true
		}
	}
	for i, h := range flMonthlyHeaders {
		if h == header {
			return c.monthlyRow()[i], true
		}
	}
	return "", false
}

// flAdhocReport: GET /reports/fl/adhoc?columns=A|B|C&from=&to=&status=X|Y
// columns are header names separated by "|"; none means all of them.
func (s *server) flAdhocReport(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, flReportRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	q := r.URL.Query()
	cols := flAllColumns()
	if raw := strings.TrimSpace(q.Get("columns")); raw != "" {
		cols = nil
		for _, c := range strings.Split(raw, "|") {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			if !contains(flAllColumns(), c) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown column: " + c, "columns": flAllColumns()})
				return
			}
			cols = append(cols, c)
		}
	}
	var statuses []string
	for _, st := range strings.Split(q.Get("status"), "|") {
		if st = strings.TrimSpace(st); st != "" {
			statuses = append(statuses, st)
		}
	}
	cases, err := s.flReportCases(r, tenant, q.Get("from"), q.Get("to"))
	if err != nil {
		http.Error(w, `{"error":"query"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="fl-adhoc-report-%s-%s.csv"`, tenant, time.Now().Format("2006-01-02")))
	cw := csv.NewWriter(w)
	_ = cw.Write(cols)
	for i := range cases {
		if len(statuses) > 0 && !contains(statuses, cases[i].agencyStatus()) {
			continue
		}
		row := make([]string, len(cols))
		for j, h := range cols {
			row[j], _ = flCell(&cases[i], h)
		}
		_ = cw.Write(row)
	}
	cw.Flush()
}

// flFinalReport: GET /reports/fl/final — contract task 2.4.5: every case over
// the life of the contract, in the monthly report's layout.
func (s *server) flFinalReport(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, flReportRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	// From the contract's effective date when it is on file, else from the start.
	from := ""
	_ = s.db.QueryRow(r.Context(),
		`SELECT coalesce(config->'contract'->>'effective_date','') FROM public.program_rules WHERE tenant=$1`, tenant).Scan(&from)
	cases, err := s.flReportCases(r, tenant, from, "")
	if err != nil {
		http.Error(w, `{"error":"query"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="fl-final-report-%s-%s.csv"`, tenant, time.Now().Format("2006-01-02")))
	cw := csv.NewWriter(w)
	_ = cw.Write(flMonthlyHeaders)
	for i := range cases {
		_ = cw.Write(cases[i].monthlyRow())
	}
	cw.Flush()
}

// flReportColumns: GET /reports/fl/columns — what an ad hoc report can contain.
func (s *server) flReportColumns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"columns": flAllColumns(), "statuses": flReportedStatuses})
}

// ---- 835 remittance (ERA) to a table ----------------------------------------

// era835Claim is one claim payment (CLP loop) of an 835.
type era835Claim struct {
	Payer, Payee, CheckNumber, CheckDate  string
	ClaimNumber, PayerClaimNumber, Status string
	Billed, Paid, PatientResp             string
	Patient                               string
	ServiceDate                           string
	Adjustments                           []string // "CO-45 120.00"
}

// parseERA835 reads an X12 835. Segments end with "~" (optionally followed by
// newlines) and elements are separated by "*"; both are taken from the ISA
// header when present. It returns nil when the text is not an 835.
func parseERA835(text string) []era835Claim {
	text = strings.TrimSpace(text)
	if !strings.Contains(text, "CLP") || !(strings.HasPrefix(text, "ISA") || strings.Contains(text, "ST*835") || strings.Contains(text, "BPR")) {
		return nil
	}
	elem, seg := "*", "~"
	if strings.HasPrefix(text, "ISA") && len(text) > 106 {
		elem, seg = string(text[3]), string(text[105])
	}
	if !strings.Contains(text, seg) {
		seg = "\n"
	}
	var out []era835Claim
	var payer, payee, checkNo, checkDate string
	var cur *era835Claim
	n1 := ""
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	date := func(v string) string { // CCYYMMDD -> MM/DD/YYYY
		if len(v) == 8 {
			return v[4:6] + "/" + v[6:8] + "/" + v[0:4]
		}
		return v
	}
	get := func(e []string, i int) string {
		if i < len(e) {
			return strings.TrimSpace(e[i])
		}
		return ""
	}
	for _, raw := range strings.Split(text, seg) {
		e := strings.Split(strings.TrimSpace(raw), elem)
		switch get(e, 0) {
		case "BPR":
			checkDate = date(get(e, 16))
		case "TRN":
			checkNo = get(e, 2)
		case "N1":
			n1 = get(e, 1)
			if n1 == "PR" {
				payer = get(e, 2)
			} else if n1 == "PE" {
				payee = get(e, 2)
			}
		case "CLP":
			flush()
			cur = &era835Claim{Payer: payer, Payee: payee, CheckNumber: checkNo, CheckDate: checkDate,
				ClaimNumber: get(e, 1), Status: era835Status(get(e, 2)), Billed: get(e, 3), Paid: get(e, 4),
				PatientResp: get(e, 5), PayerClaimNumber: get(e, 7)}
		case "NM1":
			if cur != nil && get(e, 1) == "QC" {
				cur.Patient = strings.TrimSpace(get(e, 4) + " " + get(e, 3))
			}
		case "DTM":
			if cur != nil && (get(e, 1) == "232" || get(e, 1) == "472") && cur.ServiceDate == "" {
				cur.ServiceDate = date(get(e, 2))
			}
		case "CAS":
			if cur != nil {
				for i := 2; i+1 < len(e); i += 3 {
					if get(e, i) != "" {
						cur.Adjustments = append(cur.Adjustments, get(e, 1)+"-"+get(e, i)+" "+get(e, i+1))
					}
				}
			}
		}
	}
	flush()
	return out
}

func era835Status(code string) string {
	switch code {
	case "1":
		return "Processed as primary"
	case "2":
		return "Processed as secondary"
	case "3":
		return "Processed as tertiary"
	case "4":
		return "Denied"
	case "19":
		return "Processed as primary, forwarded"
	case "22":
		return "Reversal of previous payment"
	}
	return code
}

var era835Headers = []string{"Claim Number", "Payer Claim Number", "Status", "Billed", "Paid", "Patient Responsibility",
	"Adjustments", "Patient", "Service Date", "Payer", "Payee", "Check / EFT Number", "Check Date"}

// documentAs835: GET /cases/{caseId}/documents/{docId}/835.csv — an uploaded
// 835 text file, as a spreadsheet-ready table.
func (s *server) documentAs835(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, caseStaffRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID, docID := chi.URLParam(r, "caseId"), chi.URLParam(r, "docId")
	var key, name string
	var parts []byte
	var size int64
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT object_key, coalesce(filename, id::text), parts, coalesce(size_bytes,0) FROM tenant_%s.documents
		WHERE id::text=$1 AND case_id::text=$2 AND NOT sealed`, sanitizeTenant(tenant)), docID, caseID).Scan(&key, &name, &parts, &size); err != nil {
		http.Error(w, `{"error":"document not found"}`, http.StatusNotFound)
		return
	}
	if size > 50<<20 {
		http.Error(w, `{"error":"file too large to convert (50 MB at most)"}`, http.StatusRequestEntityTooLarge)
		return
	}
	var buf bytes.Buffer
	if err := s.writeDocPlaintext(r, tenant, key, parts, &buf); err != nil {
		http.Error(w, `{"error":"could not read the document"}`, http.StatusBadGateway)
		return
	}
	claims := parseERA835(buf.String())
	if claims == nil {
		http.Error(w, `{"error":"this document is not an 835 remittance (X12) text file"}`, http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, strings.TrimSuffix(name, ".txt")))
	cw := csv.NewWriter(w)
	_ = cw.Write(era835Headers)
	for _, c := range claims {
		_ = cw.Write([]string{c.ClaimNumber, c.PayerClaimNumber, c.Status, c.Billed, c.Paid, c.PatientResp,
			strings.Join(c.Adjustments, "; "), c.Patient, c.ServiceDate, c.Payer, c.Payee, c.CheckNumber, c.CheckDate})
	}
	cw.Flush()
	s.logActivity(r.Context(), tenant, caseID, "ERA_CONVERTED", fmt.Sprintf("835 remittance %q converted to a table (%d claims)", name, len(claims)))
}
