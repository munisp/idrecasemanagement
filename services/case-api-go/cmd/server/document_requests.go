// Author: Patrick Munis at newwave
package main

// document_requests.go — a reviewer asks one side of a dispute for documents,
// and that side answers in the portal.
//
// A request names who is asked (the provider's side or the health plan's),
// why (verification, an eligibility re-check, a missing or unreadable
// document, an RFI), what is needed and by when. When the side is an
// organization on the platform -- the TPA filing for the provider, or the
// registered plan -- the request is
// in its portal (bell, Document requests page) and the email only says to
// sign in and upload. A side with no portal account gets the secure upload
// link by email, as before.
//
// Uploads land on the case like any other party document (sealed, read by
// doc-intel). Submitting tells the reviewer who asked, and an RFI's response
// is signalled to the case workflow (RFI_RESPONSE_RECEIVED) -- no one has to
// record it by hand.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

var docRequestReasons = map[string]string{
	"VERIFICATION":        "Verification",
	"ELIGIBILITY_RECHECK": "Eligibility re-check",
	"MISSING_DOCUMENT":    "Missing document",
	"CLEARER_COPY":        "Clearer copy needed",
	"RFI":                 "Request for information",
	"OTHER":               "Other",
}

// docRequestRoles may ask for documents: the Reviewer, the PM and the case's
// specialists, and admins.
var docRequestRoles = []string{"CASE_MANAGER", "PM", "ARBITRATOR", "ATTORNEY", "CODER", "NURSE_PHYSICIAN", "DOCTOR", "NURSE",
	"TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

// partyContact: who answers for one side of a dispute -- its organization on
// the platform ("tpa:<id>" / "plan:<id>", or "") and where its notices go.
type partyContact struct {
	org, email, name string
}

func (s *server) sideContact(ctx context.Context, tenant, caseID, side string) partyContact {
	var filerType, requester, filerTPA, filerPlan, tpaEmail, planEmail, tpaName, planName string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`
		SELECT coalesce(c.details->>'filing_party_type','PROVIDER'), coalesce(c.details->>'requester_email',''),
		       coalesce(o.tpa_id::text,''), coalesce(o.health_plan_id::text,''),
		       coalesce(t.contact_email,''), coalesce(h.contact_email,''), coalesce(t.name,''), coalesce(h.name,'')
		FROM tenant_%s.cases c
		LEFT JOIN public.case_origin o ON o.tenant=$1 AND o.case_id=c.id::text
		LEFT JOIN public.tpas t ON t.id=o.tpa_id
		LEFT JOIN public.health_plans h ON h.id=o.health_plan_id
		WHERE c.id::text=$2`, sanitizeTenant(tenant)), tenant, caseID).
		Scan(&filerType, &requester, &filerTPA, &filerPlan, &tpaEmail, &planEmail, &tpaName, &planName)
	if strings.EqualFold(filerType, side) {
		switch {
		case filerTPA != "":
			return partyContact{"tpa:" + filerTPA, tpaEmail, tpaName}
		case filerPlan != "":
			return partyContact{"plan:" + filerPlan, planEmail, planName}
		}
		return partyContact{"", requester, ""}
	}
	// The other side: the responding party, on the platform when it was linked.
	var rName, rEmail, rPlan, rTPA, rPlanEmail, rTPAEmail, rTPAName string
	_ = s.db.QueryRow(ctx, `
		SELECT rs.name, coalesce(rs.email,''), coalesce(rs.plan_id::text,''), coalesce(rs.tpa_id::text,''),
		       coalesce(h.contact_email,''), coalesce(t.contact_email,''), coalesce(t.name,'')
		FROM public.case_respondents rs
		LEFT JOIN public.health_plans h ON h.id=rs.plan_id
		LEFT JOIN public.tpas t ON t.id=rs.tpa_id
		WHERE rs.tenant=$1 AND rs.case_id=$2`, tenant, caseID).Scan(&rName, &rEmail, &rPlan, &rTPA, &rPlanEmail, &rTPAEmail, &rTPAName)
	if rPlan != "" {
		return partyContact{"plan:" + rPlan, firstNonEmpty(rPlanEmail, rEmail), rName}
	}
	// A provider on the other side is reached by its notice email (a TPA
	// files for its clients; it does not answer for them).
	return partyContact{"", rEmail, rName}
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// createDocRequest: POST /cases/{caseId}/document-requests
// {"side":"PROVIDER|HEALTH_PLAN","reason":"...","items":["..."],"message":"...","due_days":15}
func (s *server) createDocRequest(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, docRequestRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		Side    string   `json:"side"`
		Reason  string   `json:"reason"`
		Items   []string `json:"items"`
		Message string   `json:"message"`
		DueDays int      `json:"due_days"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in) != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	in.Side = strings.ToUpper(strings.TrimSpace(in.Side))
	if in.Side != "PROVIDER" && in.Side != "HEALTH_PLAN" {
		http.Error(w, `{"error":"say which side is asked: PROVIDER or HEALTH_PLAN"}`, http.StatusBadRequest)
		return
	}
	if _, ok := docRequestReasons[in.Reason]; !ok {
		http.Error(w, `{"error":"reason must be one of VERIFICATION, ELIGIBILITY_RECHECK, MISSING_DOCUMENT, CLEARER_COPY, RFI, OTHER"}`, http.StatusBadRequest)
		return
	}
	items := []string{}
	for _, it := range in.Items {
		if it = strings.TrimSpace(it); it != "" {
			items = append(items, truncate(it, 200))
		}
	}
	if len(items) == 0 && strings.TrimSpace(in.Message) == "" {
		http.Error(w, `{"error":"say which documents you need"}`, http.StatusBadRequest)
		return
	}
	if in.DueDays <= 0 || in.DueDays > 90 {
		in.DueDays = 15
	}
	var exists bool
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM tenant_%s.cases WHERE id::text=$1)`, sanitizeTenant(tenant)), caseID).Scan(&exists)
	if !exists {
		http.Error(w, `{"error":"case not found"}`, http.StatusNotFound)
		return
	}
	if !s.sideJoined(r.Context(), tenant, caseID, in.Side) {
		http.Error(w, `{"error":"the other side is told of this dispute once it is found eligible — ask it for documents after the eligibility review"}`, http.StatusConflict)
		return
	}
	id, pc, err := s.openDocRequest(r, tenant, caseID, in.Side, in.Reason, "reviewer", items, strings.TrimSpace(in.Message), in.DueDays, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "in_portal": pc.org != "", "sent_to": pc.email})
}

// openDocRequest records a request and tells the side asked: in its portal
// and by email (portal accounts are told to sign in; others get an upload link).
func (s *server) openDocRequest(r *http.Request, tenant, caseID, side, reason, source string, items []string, message string, dueDays int, by string) (string, partyContact, error) {
	pc := s.sideContact(r.Context(), tenant, caseID, side)
	itemsJSON, _ := json.Marshal(items)
	due := time.Now().UTC().AddDate(0, 0, dueDays).Format("2006-01-02")
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.document_requests (tenant, case_id, side, org, reason, items, message, due_date, source, requested_by)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6::jsonb,NULLIF($7,''),$8::date,$9,$10) RETURNING id::text`,
		tenant, caseID, side, pc.org, reason, string(itemsJSON), message, due, source, by).Scan(&id); err != nil {
		return "", pc, err
	}
	var caseNumber string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber)
	what := strings.Join(items, "; ")
	if what == "" {
		what = message
	}
	label := docRequestReasons[reason]
	if pc.org != "" {
		kind, orgID, _ := strings.Cut(pc.org, ":")
		s.notifyOrg(r.Context(), tenant, kind, orgID, caseID, "DOCUMENTS_REQUESTED",
			fmt.Sprintf("Documents requested for %s (%s): %s — due %s. Upload them under Document requests.", caseNumber, strings.ToLower(label), truncate(what, 160), due),
			"#/document-requests")
	}
	if pc.email != "" && source != "rfi_letter" { // an RFI's letter is the email
		subject := fmt.Sprintf("Documents requested — dispute %s", caseNumber)
		var body strings.Builder
		fmt.Fprintf(&body, "The review of dispute %s needs documents from you (%s).\n\n", caseNumber, strings.ToLower(label))
		for _, it := range items {
			fmt.Fprintf(&body, "  • %s\n", it)
		}
		if message != "" {
			fmt.Fprintf(&body, "\n%s\n", message)
		}
		fmt.Fprintf(&body, "\nPlease send them by %s.\n\n", due)
		if pc.org != "" {
			fmt.Fprintf(&body, "Sign in to the portal and upload them under Document requests:\n%s/app.html#/document-requests\n\n"+
				"Please upload in the portal rather than replying to this email.", strings.TrimRight(s.cfg.PortalBaseURL, "/"))
		} else {
			fmt.Fprintf(&body, "Upload them through this secure link:\n%s", s.autoShareLink(r, tenant, caseID, "upload", ""))
		}
		err := s.sendMail([]string{pc.email}, nil, subject, body.String())
		errs := []string{}
		if err != nil {
			errs = append(errs, err.Error())
		}
		s.logCorrespondence(r, tenant, caseID, "OUT", "document_request", subject, body.String(), []string{pc.email}, nil, displayName(r.Context().Value(ctxPrincipal{}).(principal)), errs...)
	}
	s.logActivity(r.Context(), tenant, caseID, "DOCUMENTS_REQUESTED", fmt.Sprintf("%s requested from the %s side: %s (due %s)%s",
		label, map[string]string{"PROVIDER": "provider", "HEALTH_PLAN": "health plan"}[side], truncate(what, 300), due,
		map[bool]string{true: " — in its portal", false: " — by email"}[pc.org != ""]))
	s.logAudit(r.Context(), tenant, caseID, "DOCUMENTS_REQUESTED", map[string]any{
		"by": by, "request_id": id, "side": side, "reason": reason, "source": source, "org": pc.org, "items": len(items), "due": due})
	return id, pc, nil
}

// rfiDocRequest: an RFI letter that went out opens its request, so the side
// answers in the portal and the workflow hears of the answer.
func (s *server) rfiDocRequest(r *http.Request, tenant, caseID, rfiTo string) {
	side := "PROVIDER"
	if rfiTo == "plan" {
		side = "HEALTH_PLAN"
	}
	var open bool
	_ = s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM public.document_requests WHERE tenant=$1 AND case_id=$2 AND source='rfi_letter' AND side=$3 AND status='OPEN')`,
		tenant, caseID, side).Scan(&open)
	if open {
		return
	}
	by := ""
	if p, ok := r.Context().Value(ctxPrincipal{}).(principal); ok {
		by = p.Subject
	}
	_, _, _ = s.openDocRequest(r, tenant, caseID, side, "RFI", "rfi_letter", []string{"The information asked for in the RFI letter"}, "", 15, by)
}

// portalRFI: the side an RFI goes to answers in the portal -- its letter
// points there instead of carrying an upload link.
func (s *server) portalRFI(ctx context.Context, tenant, caseID, rfiTo string) bool {
	side := "PROVIDER"
	if rfiTo == "plan" {
		side = "HEALTH_PLAN"
	}
	return s.sideContact(ctx, tenant, caseID, side).org != ""
}

// listCaseDocRequests: GET /cases/{caseId}/document-requests -- staff view.
func (s *server) listCaseDocRequests(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	reqs, err := s.docRequests(r.Context(), tenant, "r.case_id=$2", chi.URLParam(r, "caseId"))
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": reqs})
}

func (s *server) docRequests(ctx context.Context, tenant, where string, arg any) ([]map[string]any, error) {
	rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT r.id::text, r.case_id, coalesce(c.case_number,''), r.side, coalesce(r.org,''), r.reason, r.items::text, coalesce(r.message,''),
		       coalesce(r.due_date::text,''), r.source, r.status, r.created_at, coalesce(r.submitted_at::text,''), coalesce(r.response_note,''),
		       coalesce(r.requested_by,''),
		       coalesce((SELECT json_agg(json_build_object('doc_id', f.doc_id, 'filename', f.filename, 'at', f.created_at) ORDER BY f.created_at)
		                 FROM public.document_request_files f WHERE f.request_id=r.id), '[]'::json)::text,
		       coalesce(c.details->>'initiating_party',''), coalesce(rs.name,'')
		FROM public.document_requests r
		LEFT JOIN tenant_%s.cases c ON c.id::text=r.case_id
		LEFT JOIN public.case_respondents rs ON rs.tenant=r.tenant AND rs.case_id=r.case_id
		WHERE r.tenant=$1 AND %s
		ORDER BY (r.status='OPEN') DESC, r.created_at DESC LIMIT 300`, sanitizeTenant(tenant), where), tenant, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	subjects := []string{}
	for rows.Next() {
		var id, caseID, num, side, org, reason, items, msg, due, source, status, submitted, note, by, files, filer, other string
		var at time.Time
		if rows.Scan(&id, &caseID, &num, &side, &org, &reason, &items, &msg, &due, &source, &status, &at, &submitted, &note, &by, &files, &filer, &other) != nil {
			continue
		}
		var it, fl any
		_ = json.Unmarshal([]byte(items), &it)
		_ = json.Unmarshal([]byte(files), &fl)
		overdue := false
		if d, err := time.Parse("2006-01-02", due); err == nil && status == "OPEN" && time.Now().UTC().After(d.Add(24*time.Hour)) {
			overdue = true
		}
		out = append(out, map[string]any{"id": id, "case_id": caseID, "case_number": num, "side": side, "in_portal": org != "",
			"reason": reason, "reason_label": docRequestReasons[reason], "items": it, "message": msg, "due_date": due, "overdue": overdue,
			"source": source, "status": status, "created_at": at, "submitted_at": submitted, "response_note": note, "files": fl,
			"filer": filer, "other_party": other, "requested_by": by})
		if by != "" {
			subjects = append(subjects, by)
		}
	}
	names := s.resolveSubjectNames(ctx, subjects)
	for _, x := range out {
		if by, _ := x["requested_by"].(string); by != "" {
			x["requested_by"] = firstNonEmpty(names[by], "the review team")
		}
	}
	return out, nil
}

// closeDocRequest: POST /cases/{caseId}/document-requests/{reqId}/close
// {"action":"complete|cancel"} -- the reviewer has what was needed, or no
// longer needs it.
func (s *server) closeDocRequest(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, docRequestRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	var in struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in)
	status := map[string]string{"complete": "COMPLETE", "cancel": "CANCELLED"}[in.Action]
	if status == "" {
		http.Error(w, `{"error":"action must be complete or cancel"}`, http.StatusBadRequest)
		return
	}
	var org, caseID string
	if s.db.QueryRow(r.Context(), `
		UPDATE public.document_requests SET status=$4, closed_at=now(), closed_by=$5
		WHERE tenant=$1 AND case_id=$2 AND id::text=$3 AND status IN ('OPEN','SUBMITTED') RETURNING coalesce(org,''), case_id`,
		tenant, chi.URLParam(r, "caseId"), chi.URLParam(r, "reqId"), status, p.Subject).Scan(&org, &caseID) != nil {
		http.Error(w, `{"error":"no open request with that id on this case"}`, http.StatusConflict)
		return
	}
	s.logActivity(r.Context(), tenant, caseID, "DOCUMENT_REQUEST_"+status, "Document request "+strings.ToLower(status)+" by "+displayName(p))
	s.logAudit(r.Context(), tenant, caseID, "DOCUMENT_REQUEST_"+status, map[string]any{"by": p.Subject, "request_id": chi.URLParam(r, "reqId"), "org": org})
	if status == "CANCELLED" && org != "" {
		kind, id, _ := strings.Cut(org, ":")
		s.notifyOrg(r.Context(), tenant, kind, id, caseID, "DOCUMENT_REQUEST_CANCELLED", "A document request was withdrawn — nothing more is needed for it", "#/document-requests")
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

// orgDocRequests: GET /tpa/document-requests, /hp/document-requests -- what
// the organization is asked for.
func (s *server) orgDocRequests(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"not linked"}`, http.StatusNotFound)
			return
		}
		reqs, err := s.docRequests(r.Context(), tenant, "r.org=$2 AND r.status <> 'CANCELLED'", py.org())
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		open := 0
		for _, x := range reqs {
			if x["status"] == "OPEN" {
				open++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"requests": reqs, "open": open})
	}
}

// orgRequest loads an open request of the caller's organization.
func (s *server) orgRequest(r *http.Request, tenant, kind, reqID string) (caseID string, py payer, ok bool) {
	py, err := s.payerFor(r, tenant, kind)
	if err != nil {
		return "", py, false
	}
	err = s.db.QueryRow(r.Context(), `SELECT case_id FROM public.document_requests WHERE tenant=$1 AND id::text=$2 AND org=$3 AND status='OPEN'`,
		tenant, reqID, py.org()).Scan(&caseID)
	return caseID, py, err == nil
}

// orgDocRequestUpload: POST /tpa/document-requests/{reqId}/files (multipart "file").
func (s *server) orgDocRequestUpload(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		reqID := chi.URLParam(r, "reqId")
		caseID, py, ok := s.orgRequest(r, tenant, kind, reqID)
		if !ok {
			http.Error(w, `{"error":"no open request with that id for your organization"}`, http.StatusNotFound)
			return
		}
		docID, filename, code, msg := s.storeCaseUpload(w, r, tenant, caseID, "requested:"+py.name, "Requested — ")
		if code != 0 {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), code)
			return
		}
		p := r.Context().Value(ctxPrincipal{}).(principal)
		_, _ = s.db.Exec(r.Context(), `INSERT INTO public.document_request_files (request_id, tenant, doc_id, filename, uploaded_by) VALUES ($1::uuid,$2,$3::uuid,$4,$5)`,
			reqID, tenant, docID, filename, p.Subject)
		s.logActivity(r.Context(), tenant, caseID, "REQUESTED_DOCUMENT", fmt.Sprintf("%s uploaded a requested document: %s", py.name, filename))
		writeJSON(w, http.StatusCreated, map[string]any{"doc_id": docID, "filename": filename})
	}
}

// orgDocRequestSubmit: POST /tpa/document-requests/{reqId}/submit {"note":"..."}.
func (s *server) orgDocRequestSubmit(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		reqID := chi.URLParam(r, "reqId")
		caseID, py, ok := s.orgRequest(r, tenant, kind, reqID)
		if !ok {
			http.Error(w, `{"error":"no open request with that id for your organization"}`, http.StatusNotFound)
			return
		}
		var in struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
		var files int
		_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM public.document_request_files WHERE request_id::text=$1`, reqID).Scan(&files)
		if files == 0 {
			http.Error(w, `{"error":"upload the requested documents before submitting"}`, http.StatusBadRequest)
			return
		}
		p := r.Context().Value(ctxPrincipal{}).(principal)
		var reason, source, requestedBy, caseNumber string
		if s.db.QueryRow(r.Context(), `
			UPDATE public.document_requests SET status='SUBMITTED', submitted_at=now(), submitted_by=$3, response_note=NULLIF($4,'')
			WHERE tenant=$1 AND id::text=$2 AND status='OPEN' RETURNING reason, source, coalesce(requested_by,'')`,
			tenant, reqID, p.Subject, strings.TrimSpace(in.Note)).Scan(&reason, &source, &requestedBy) != nil {
			http.Error(w, `{"error":"this request was already answered"}`, http.StatusConflict)
			return
		}
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber)
		msg := fmt.Sprintf("%s sent the documents you requested for %s (%d file%s)", py.name, caseNumber, files, map[bool]string{true: "", false: "s"}[files == 1])
		if reason == "ELIGIBILITY_RECHECK" {
			msg += " — ready for the eligibility re-check"
		}
		if requestedBy != "" {
			s.notify(r, tenant, requestedBy, "DOCUMENTS_RECEIVED", msg, "#/cases/"+caseID)
		} else {
			s.notify(r, tenant, "role:CASE_MANAGER", "DOCUMENTS_RECEIVED", msg, "#/cases/"+caseID)
		}
		s.logActivity(r.Context(), tenant, caseID, "DOCUMENTS_RECEIVED", msg+orDash(" — note: "+strings.TrimSpace(in.Note)))
		signalled := false
		if source == "rfi_letter" || reason == "RFI" {
			// The RFI's 15-day clock stops: the response is in.
			wfID := fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), caseNumber)
			signalled = s.tc != nil && s.tc.SignalWorkflow(r.Context(), wfID, "", "RFI_RESPONSE_RECEIVED",
				map[string]any{"source": "portal document request", "by": py.name, "documents": files}) == nil
			s.logActivity(r.Context(), tenant, caseID, "RFI_RESPONSE_RECEIVED", "RFI response received in the portal from "+py.name)
		}
		s.logAudit(r.Context(), tenant, caseID, "DOCUMENTS_SUBMITTED", map[string]any{
			"by": p.Subject, "request_id": reqID, "files": files, "org": py.org(), "workflow_signalled": signalled})
		writeJSON(w, http.StatusOK, map[string]any{"status": "SUBMITTED", "workflow_signalled": signalled})
	}
}

// storeCaseUpload takes one uploaded file (multipart "file"), checks it like
// every party upload, seals it in the vault and records it as a party
// document of the case; doc-intel reads it. code != 0: refused, with why.
func (s *server) storeCaseUpload(w http.ResponseWriter, r *http.Request, tenant, caseID, uploadedBy, namePrefix string) (docID, filename string, code int, msg string) {
	var status string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT status FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&status)
	if status == "CLOSED" || status == "AWAITING_PAYMENT" {
		return "", "", http.StatusConflict, "this dispute is not open for documents"
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDocBytes)
	if err := r.ParseMultipartForm(maxDocBytes); err != nil {
		return "", "", http.StatusRequestEntityTooLarge, "file too large or malformed"
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return "", "", http.StatusBadRequest, "file field required"
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return "", "", http.StatusBadRequest, "read failed"
	}
	if err := docPolicy(hdr.Filename, raw); err != nil {
		return "", "", http.StatusUnsupportedMediaType, err.Error()
	}
	if sig, ok := s.scanOrRefuse(discardWriter{}, bytes.NewReader(raw), "file"); !ok {
		return "", "", http.StatusUnsupportedMediaType, "malware scan rejected: " + sig
	}
	docID = newUUID()
	objectKey := fmt.Sprintf("%s/cases/%s/%s.enc", tenant, caseID, docID)
	ct, err := s.vaultSealDoc(r, tenant, objectKey, raw)
	if err != nil {
		return "", "", http.StatusBadGateway, "seal failed"
	}
	if _, err := s.docs.mc.PutObject(r.Context(), docBucket, objectKey, bytes.NewReader(ct), int64(len(ct)),
		minio.PutObjectOptions{UserMetadata: map[string]string{"tenant": tenant, "case": caseID}}); err != nil {
		return "", "", http.StatusBadGateway, "store failed"
	}
	ctype := hdr.Header.Get("Content-Type")
	filename = namePrefix + hdr.Filename
	if _, err := s.db.Exec(r.Context(), fmt.Sprintf(`
		INSERT INTO tenant_%s.documents
		  (id, case_id, object_key, size_bytes, content_type, sealed, uploaded_by, version, scan_status, filename, folder)
		VALUES ($1,$2::uuid,$3,$4,$5,false,$6,1,'CLEAN',$7,'PARTY_UPLOADS')`, sanitizeTenant(tenant)),
		docID, caseID, objectKey, len(raw), ctype, uploadedBy, filename); err != nil {
		return "", "", http.StatusInternalServerError, "db"
	}
	s.publish(r.Context(), tenant, "documents", map[string]any{
		"type": "doc.uploaded", "tenant": tenant, "case_id": caseID, "doc_id": docID,
		"object_key": objectKey, "content_type": ctype, "sealed": false, "at": time.Now().UTC(),
	})
	return docID, filename, 0, ""
}

// sideJoined: the filing side always is; the other side once it has joined
// the dispute (eligibleForOtherSide).
func (s *server) sideJoined(ctx context.Context, tenant, caseID, side string) bool {
	var filerType string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT coalesce(details->>'filing_party_type','PROVIDER') FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&filerType)
	if strings.EqualFold(filerType, side) {
		return true
	}
	var joined bool
	err := s.db.QueryRow(ctx, `SELECT `+eligibleForOtherSide+` FROM public.case_respondents rs WHERE rs.tenant=$1 AND rs.case_id=$2`, tenant, caseID).Scan(&joined)
	return err != nil || joined // no respondent row (an older case): nothing to gate on
}
