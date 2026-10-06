// call_tracking.go — phone call tracker + inquiries log (program-layer).
// Distinct from public.correspondence_log: that table is the record of what
// actually went out on one of the 16 templates; this one is the informal
// audit trail of calls staff made/received and inbound inquiries, which the
// AHCA workflow narrative requires independent of any formal letter.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

var callTrackingRoles = []string{"CASE_MANAGER", "PM", "CODER", "NURSE_PHYSICIAN", "ATTORNEY", "FEDERAL_ADMIN", "PLATFORM_ADMIN", serviceRole}

// logCall: POST /v1/tenants/{tenant}/calls (tenant-level, pre-case)
//          POST /v1/tenants/{tenant}/cases/{caseId}/calls (case-scoped)
func (s *server) logCall(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, callTrackingRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		CaseID         string `json:"case_id"`
		RequesterEmail string `json:"requester_email"`
		Direction      string `json:"direction"`
		Phone          string `json:"phone"`
		Summary        string `json:"summary"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if caseID == "" {
		caseID = in.CaseID
	}
	if in.Direction != "INBOUND" && in.Direction != "OUTBOUND" {
		http.Error(w, `{"error":"direction must be INBOUND or OUTBOUND"}`, http.StatusBadRequest)
		return
	}
	if in.Phone == "" || in.Summary == "" {
		http.Error(w, `{"error":"phone and summary are required"}`, http.StatusBadRequest)
		return
	}
	var id int64
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.call_log (tenant, case_id, requester_email, direction, reviewer, phone, summary)
		VALUES ($1, NULLIF($2,''), NULLIF($3,''), $4, $5, $6, $7) RETURNING id`,
		tenant, caseID, in.RequesterEmail, in.Direction, p.Subject, in.Phone, in.Summary).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if caseID != "" {
		s.logActivity(r.Context(), tenant, caseID,
			"CALL", fmt.Sprintf("%s call with %s by %s: %s", in.Direction, in.Phone, p.Subject, in.Summary))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// listCalls: GET /v1/tenants/{tenant}/calls?case_id=|email=
//            GET /v1/tenants/{tenant}/cases/{caseId}/calls
func (s *server) listCalls(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if caseID == "" {
		caseID = r.URL.Query().Get("case_id")
	}
	email := r.URL.Query().Get("email")
	var rows []map[string]any
	var err error
	switch {
	case caseID != "":
		rows, err = s.queryRows(r, `
			SELECT id, direction, reviewer, phone, summary, created_at
			FROM public.call_log WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC`, tenant, caseID)
	case email != "":
		rows, err = s.queryRows(r, `
			SELECT id, direction, reviewer, phone, summary, created_at
			FROM public.call_log WHERE tenant=$1 AND requester_email=$2 ORDER BY created_at DESC`, tenant, email)
	default:
		http.Error(w, `{"error":"case_id or email required"}`, http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": rows})
}

// logInquiry: POST /v1/tenants/{tenant}/inquiries
//             POST /v1/tenants/{tenant}/cases/{caseId}/inquiries
func (s *server) logInquiry(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, callTrackingRoles...) {
		http.Error(w, `{"error":"forbidden: requires case staff role"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	var in struct {
		CaseID         string `json:"case_id"`
		RequesterEmail string `json:"requester_email"`
		Method         string `json:"method"`
		InquiryType    string `json:"inquiry_type"`
		Detail         string `json:"detail"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if caseID == "" {
		caseID = in.CaseID
	}
	if in.Method != "EMAIL" && in.Method != "PHONE_CALL" {
		http.Error(w, `{"error":"method must be EMAIL or PHONE_CALL"}`, http.StatusBadRequest)
		return
	}
	switch in.InquiryType {
	case "GENERAL_QUESTION", "GENERAL_INQUIRY", "SUBMISSION_DOCUMENTS":
	default:
		http.Error(w, `{"error":"inquiry_type must be GENERAL_QUESTION, GENERAL_INQUIRY, or SUBMISSION_DOCUMENTS"}`, http.StatusBadRequest)
		return
	}
	var id int64
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.inquiry_log (tenant, case_id, requester_email, method, inquiry_type, detail, logged_by)
		VALUES ($1, NULLIF($2,''), NULLIF($3,''), $4, $5, $6, $7) RETURNING id`,
		tenant, caseID, in.RequesterEmail, in.Method, in.InquiryType, in.Detail, p.Subject).Scan(&id)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if caseID != "" {
		s.logActivity(r.Context(), tenant, caseID,
			"INQUIRY", fmt.Sprintf("%s inquiry (%s) logged by %s", in.Method, in.InquiryType, p.Subject))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// listInquiries: GET /v1/tenants/{tenant}/inquiries?case_id=|email=
//                GET /v1/tenants/{tenant}/cases/{caseId}/inquiries
func (s *server) listInquiries(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	if caseID == "" {
		caseID = r.URL.Query().Get("case_id")
	}
	email := r.URL.Query().Get("email")
	var rows []map[string]any
	var err error
	switch {
	case caseID != "":
		rows, err = s.queryRows(r, `
			SELECT id, method, inquiry_type, detail, logged_by, created_at
			FROM public.inquiry_log WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC`, tenant, caseID)
	case email != "":
		rows, err = s.queryRows(r, `
			SELECT id, method, inquiry_type, detail, logged_by, created_at
			FROM public.inquiry_log WHERE tenant=$1 AND requester_email=$2 ORDER BY created_at DESC`, tenant, email)
	default:
		http.Error(w, `{"error":"case_id or email required"}`, http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"inquiries": rows})
}
