// tpa_filing.go — a TPA's own dispute in full: every detail it filed, changed directly until the initial fee is
// paid and by change request after it (a case manager approves or rejects), with a change history its team and the
// office see -- never the health plan.
//
//	GET    /tpa/disputes/{caseId}/filing                         the filing, how it can change now, open request, history
//	PUT    /tpa/disputes/{caseId}/filing                         the filer's direct edit (AWAITING_PAYMENT only)
//	POST   /tpa/disputes/{caseId}/change-requests                a change asked for once paid {packet, reason}
//	DELETE /tpa/disputes/{caseId}/change-requests/{requestId}    the filer withdraws its open request
//	GET    /cases/{caseId}/change-requests                       staff: the case's requests
//	POST   /cases/{caseId}/change-requests/{requestId}/decide    staff: {approve, note}
//
// "The filing" is the filing packet as filing_packet.go keeps it: the packet's details on the case, the disputed
// amount and the claim rows. Every change -- these, the four-field correction (tpa_edit.go) and staff corrections
// (putFilingPacket) -- is written to public.dispute_changes with exactly what changed (field: from -> to) and goes
// on the audit chain. Requests and edits answer "not found" for a dispute another TPA filed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	chi "github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// filingState is a dispute's filing as it stands: the packet's details, the disputed amount and the claim rows.
type filingState struct {
	Details map[string]any `json:"details"`
	Amount  *int64         `json:"disputed_amount_cents"`
	Claims  []packetClaim  `json:"claims"`
}

// fieldChange is one changed value. A claim row's column is "claim <number>: <column>"; a claim added or removed
// is "claim <number>".
type fieldChange struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// packetDetailKeys are the case-detail keys that make up the filing packet (normalize's output).
func packetDetailKeys() map[string]bool {
	keys := map[string]bool{}
	for k := range packetText {
		keys[k] = true
	}
	for _, k := range packetDates {
		keys[k] = true
	}
	for _, k := range packetBools {
		keys[k] = true
	}
	for _, k := range []string{"claims_submitted", "contracted", "filing_party_type", "internal_dispute_exhausted",
		"line_of_business", "out_of_network", "packet_attestation", "packet_documents_submitted", "packet_provider_type",
		"plan_type", "plan_type_other", "provider_type", "provider_type_other", "type_of_service", "type_of_service_other"} {
		keys[k] = true
	}
	return keys
}

// readFilingState reads a dispute's filing, its status and case number.
func (s *server) readFilingState(ctx context.Context, tenant, caseID string) (st filingState, status, caseNumber string, err error) {
	var raw []byte
	var amount *int64
	err = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT coalesce(details,'{}'::jsonb), disputed_amount_cents, status, coalesce(case_number,'')
		FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&raw, &amount, &status, &caseNumber)
	if err != nil {
		return st, "", "", err
	}
	all := map[string]any{}
	_ = json.Unmarshal(raw, &all)
	keys := packetDetailKeys()
	st.Details = map[string]any{}
	for k, v := range all {
		if keys[k] && v != nil && fmtVal(v) != "" {
			st.Details[k] = v
		}
	}
	st.Amount = amount
	rows, err := s.db.Query(ctx, `
		SELECT claim_number, coalesce(patient_first_name,''), coalesce(patient_last_name,''), coalesce(type_of_service,''),
		       coalesce(to_char(date_of_service,'YYYY-MM-DD'),''), billed_cents, paid_cents, disputed_cents,
		       coalesce(to_char(date_claim_submitted,'YYYY-MM-DD'),''), coalesce(to_char(date_of_denial,'YYYY-MM-DD'),''),
		       coalesce(to_char(date_of_final_determination,'YYYY-MM-DD'),'')
		FROM public.case_claims WHERE tenant=$1 AND case_id=$2 ORDER BY claim_number`, tenant, caseID)
	if err != nil {
		return st, status, caseNumber, err
	}
	defer rows.Close()
	for rows.Next() {
		var c packetClaim
		if rows.Scan(&c.ClaimNumber, &c.PatientFirstName, &c.PatientLastName, &c.TypeOfService, &c.DateOfService,
			&c.BilledCents, &c.PaidCents, &c.DisputedCents, &c.DateClaimSubmitted, &c.DateOfDenial, &c.DateOfFinalDetermination) == nil {
			st.Claims = append(st.Claims, c)
		}
	}
	return st, status, caseNumber, nil
}

// fmtVal is a value as the history shows it.
func fmtVal(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case bool:
		if x {
			return "Yes"
		}
		return "No"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprint(int64(x))
		}
		return fmt.Sprint(x)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func centsText(c *int64) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("$%d.%02d", *c/100, *c%100)
}

// claimColumns are a claim row's columns as the history names them.
func claimColumns(c packetClaim) [][2]string {
	return [][2]string{
		{"patient first name", c.PatientFirstName}, {"patient last name", c.PatientLastName},
		{"type of service", c.TypeOfService}, {"date of service", c.DateOfService},
		{"amount billed", centsText(c.BilledCents)}, {"amount paid", centsText(c.PaidCents)},
		{"amount in dispute", centsText(c.DisputedCents)}, {"date claim submitted", c.DateClaimSubmitted},
		{"date of denial", c.DateOfDenial}, {"date of final determination", c.DateOfFinalDetermination},
	}
}

// diffFiling lists what differs between two filings, details sorted by key, then the amount, then the claims.
// Pure, so it is tested.
func diffFiling(before, after filingState) []fieldChange {
	out := []fieldChange{}
	keys := map[string]bool{}
	for k := range before.Details {
		keys[k] = true
	}
	for k := range after.Details {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		if a, b := fmtVal(before.Details[k]), fmtVal(after.Details[k]); a != b {
			out = append(out, fieldChange{k, a, b})
		}
	}
	if a, b := centsText(before.Amount), centsText(after.Amount); a != b {
		out = append(out, fieldChange{"disputed_amount", a, b})
	}
	old := map[string]packetClaim{}
	for _, c := range before.Claims {
		old[c.ClaimNumber] = c
	}
	seen := map[string]bool{}
	for _, c := range after.Claims {
		seen[c.ClaimNumber] = true
		o, had := old[c.ClaimNumber]
		if !had {
			out = append(out, fieldChange{"claim " + c.ClaimNumber, "", "added"})
			continue
		}
		oc, nc := claimColumns(o), claimColumns(c)
		for i := range nc {
			if oc[i][1] != nc[i][1] {
				out = append(out, fieldChange{"claim " + c.ClaimNumber + ": " + nc[i][0], oc[i][1], nc[i][1]})
			}
		}
	}
	for _, c := range before.Claims {
		if !seen[c.ClaimNumber] {
			out = append(out, fieldChange{"claim " + c.ClaimNumber, "on the dispute", "removed"})
		}
	}
	return out
}

// replaceFiling saves a filer's whole form as the dispute's filing: values it leaves empty are cleared and claim rows
// it no longer has are removed (unlike a staff correction, which only overwrites what it sends). Returns what changed.
func (s *server) replaceFiling(ctx context.Context, tenant, caseID string, before filingState, details map[string]any,
	disputed *int64, claims []packetClaim) ([]fieldChange, error) {
	after := filingState{Details: details, Amount: disputed, Claims: claims}
	if disputed == nil {
		after.Amount = before.Amount // the amount is never cleared, only changed
	}
	changes := diffFiling(before, after)
	if len(changes) == 0 {
		return changes, nil
	}
	save := map[string]any{}
	for k, v := range details {
		save[k] = v
	}
	for k := range before.Details {
		if _, kept := details[k]; !kept {
			save[k] = nil
		}
	}
	keep := map[string]bool{}
	for _, c := range claims {
		keep[c.ClaimNumber] = true
	}
	var remove []string
	for _, c := range before.Claims {
		if !keep[c.ClaimNumber] {
			remove = append(remove, c.ClaimNumber)
		}
	}
	_, err := s.applyFilingPacket(ctx, tenant, caseID, save, disputed, claims, remove, true)
	return changes, err
}

// recordChange writes one history row. Errors are logged, never fatal: the audit chain has the same entry.
func (s *server) recordChange(ctx context.Context, tenant, caseID, kind, bySub, byName, bySide string, changes []fieldChange,
	reason, requestID string) {
	js, _ := json.Marshal(changes)
	var req any
	if requestID != "" {
		req = requestID
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO public.dispute_changes (tenant, case_id, kind, by_sub, by_name, by_side, changes, reason, request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9)`, tenant, caseID, kind, bySub, byName, bySide, string(js), reason, req); err != nil {
		logEvent(ctx, "error", tenant, caseID, "DISPUTE_CHANGE_NOT_RECORDED", map[string]any{"error": err.Error()})
	}
}

// changeSummary is a short line for the timeline and notices: "3 changes: provider_npi, disputed_amount, ...".
func changeSummary(changes []fieldChange) string {
	names := []string{}
	for i, c := range changes {
		if i == 4 {
			names = append(names, fmt.Sprintf("and %d more", len(changes)-4))
			break
		}
		names = append(names, c.Field)
	}
	return fmt.Sprintf("%s: %s", countOf(len(changes), "change"), strings.Join(names, ", "))
}

func countOf(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// filingClosed: a dispute past changing.
func filingClosed(status string) bool {
	return strings.HasPrefix(status, "CLOSED") || status == "WITHDRAWN" || status == "SETTLED_IN_NEGOTIATION"
}

// filingMode is how the filer may change its dispute now: "direct", "request" or "closed".
func filingMode(status string) string {
	switch {
	case status == "AWAITING_PAYMENT":
		return "direct"
	case filingClosed(status):
		return "closed"
	}
	return "request"
}

// amountRaised: a corrected amount can cross the program's escalation trigger. Intake only checked the original
// figure, so a raised amount is checked -- once per case.
func (s *server) amountRaised(r *http.Request, tenant, caseID string, oldAmount, newAmount int64) {
	if newAmount <= oldAmount {
		return
	}
	var already bool
	_ = s.db.QueryRow(r.Context(), `
		SELECT EXISTS(SELECT 1 FROM public.escalations
		WHERE tenant=$1 AND case_id=$2 AND clock='PROGRAM_TRIGGER')`, tenant, caseID).Scan(&already)
	if !already {
		s.escalationTrigger(r, tenant, caseID, newAmount)
	}
}

func amountOf(c *int64) int64 {
	if c == nil {
		return 0
	}
	return *c
}

// tpaOwnDispute: the caller's TPA filed this dispute. Anything else answers "not found".
func (s *server) tpaOwnDispute(w http.ResponseWriter, r *http.Request) (tenant, caseID string, org *tpaOrg, p principal, ok bool) {
	p = r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant = r.Context().Value(ctxTenant{}).(string)
	org, err := s.tpaForPrincipal(r, tenant)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	caseID = chi.URLParam(r, "caseId")
	var originTPA string
	_ = s.db.QueryRow(r.Context(), `SELECT coalesce(tpa_id::text,'') FROM public.case_origin WHERE tenant=$1 AND case_id=$2`,
		tenant, caseID).Scan(&originTPA)
	if originTPA == "" || originTPA != org.ID {
		http.Error(w, `{"error":"dispute not found"}`, http.StatusNotFound)
		return
	}
	return tenant, caseID, org, p, true
}

func (s *server) disputeHistory(ctx context.Context, tenant, caseID string) []map[string]any {
	rows, err := s.db.Query(ctx, `SELECT kind, by_name, by_side, changes, reason, coalesce(request_id::text,''), created_at
		FROM public.dispute_changes WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC, id DESC LIMIT 200`, tenant, caseID)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var kind, by, side, reason, req string
		var changes json.RawMessage
		var at time.Time
		if rows.Scan(&kind, &by, &side, &changes, &reason, &req, &at) == nil {
			out = append(out, map[string]any{"kind": kind, "by": by, "side": side, "changes": changes, "reason": reason,
				"request_id": req, "at": at})
		}
	}
	return out
}

func (s *server) changeRequests(ctx context.Context, tenant, caseID string, onlyOpen bool) []map[string]any {
	rows, err := s.db.Query(ctx, `SELECT id::text, requested_name, changes, reason, status, coalesce(decided_name,''),
		decided_at, decision_note, created_at
		FROM public.dispute_change_requests WHERE tenant=$1 AND case_id=$2 AND ($3=false OR status='PENDING')
		ORDER BY created_at DESC LIMIT 100`, tenant, caseID, onlyOpen)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, by, reason, status, decidedBy, note string
		var changes json.RawMessage
		var decidedAt *time.Time
		var at time.Time
		if rows.Scan(&id, &by, &changes, &reason, &status, &decidedBy, &decidedAt, &note, &at) == nil {
			out = append(out, map[string]any{"id": id, "requested_by": by, "changes": changes, "reason": reason,
				"status": status, "decided_by": decidedBy, "decided_at": decidedAt, "decision_note": note, "created_at": at})
		}
	}
	return out
}

// tpaFilingGet: GET /tpa/disputes/{caseId}/filing.
func (s *server) tpaFilingGet(w http.ResponseWriter, r *http.Request) {
	tenant, caseID, _, _, ok := s.tpaOwnDispute(w, r)
	if !ok {
		return
	}
	st, status, number, err := s.readFilingState(r.Context(), tenant, caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, `{"error":"dispute not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	open := s.changeRequests(r.Context(), tenant, caseID, true)
	var openReq any
	if len(open) > 0 {
		openReq = open[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{"case_id": caseID, "case_number": number, "status": status,
		"mode": filingMode(status), "filing": st, "open_request": openReq, "history": s.disputeHistory(r.Context(), tenant, caseID)})
}

// readPacketBody decodes and checks a filer's form; on a problem it answers and returns ok=false.
func readPacketBody(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return false
	}
	return true
}

// tpaFilingPut: PUT /tpa/disputes/{caseId}/filing — the filer's own edit while the fee is unpaid.
func (s *server) tpaFilingPut(w http.ResponseWriter, r *http.Request) {
	tenant, caseID, org, p, ok := s.tpaOwnDispute(w, r)
	if !ok {
		return
	}
	var pk filingPacket
	if !readPacketBody(w, r, &pk) {
		return
	}
	pk.RemoveClaims = nil
	details, disputed, claims, problems := pk.normalize()
	if len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the filing has problems", "problems": problems})
		return
	}
	before, status, number, err := s.readFilingState(r.Context(), tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"dispute not found"}`, http.StatusNotFound)
		return
	}
	if filingMode(status) != "direct" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the fee for this dispute is paid: ask for a change instead, and a case manager will review it"})
		return
	}
	changes, err := s.replaceFiling(r.Context(), tenant, caseID, before, details, disputed, claims)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	if len(changes) > 0 {
		who := displayName(p)
		s.recordChange(r.Context(), tenant, caseID, "EDITED", p.Subject, who, "FILER", changes, "", "")
		s.logAudit(r.Context(), tenant, caseID, "TPA_FILING_EDITED", map[string]any{"by": p.Subject, "tpa_id": org.ID,
			"case_number": number, "changes": changes})
		s.logActivity(r.Context(), tenant, caseID, "TPA_FILING_EDITED", fmt.Sprintf("Filing edited by %s (%s): %s", who, org.Name, changeSummary(changes)))
		if disputed != nil {
			s.amountRaised(r, tenant, caseID, amountOf(before.Amount), *disputed)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"case_number": number, "changes": changes})
}

type changeRequestBody struct {
	Packet filingPacket `json:"packet"`
	Reason string       `json:"reason"`
}

// tpaChangeRequest: POST /tpa/disputes/{caseId}/change-requests — once the fee is paid.
func (s *server) tpaChangeRequest(w http.ResponseWriter, r *http.Request) {
	tenant, caseID, org, p, ok := s.tpaOwnDispute(w, r)
	if !ok {
		return
	}
	var in changeRequestBody
	if !readPacketBody(w, r, &in) {
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || len(in.Reason) > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "say why the change is needed (up to 2,000 characters)"})
		return
	}
	in.Packet.RemoveClaims = nil
	details, disputed, claims, problems := in.Packet.normalize()
	if len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the filing has problems", "problems": problems})
		return
	}
	before, status, number, err := s.readFilingState(r.Context(), tenant, caseID)
	if err != nil {
		http.Error(w, `{"error":"dispute not found"}`, http.StatusNotFound)
		return
	}
	switch filingMode(status) {
	case "direct":
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the fee is not paid yet: edit the dispute directly"})
		return
	case "closed":
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this dispute is " + status + ": it can no longer be changed"})
		return
	}
	after := filingState{Details: details, Amount: disputed, Claims: claims}
	if disputed == nil {
		after.Amount = before.Amount
	}
	changes := diffFiling(before, after)
	if len(changes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nothing would change"})
		return
	}
	packetJS, _ := json.Marshal(in.Packet)
	changesJS, _ := json.Marshal(changes)
	who := displayName(p)
	var reqID string
	err = s.db.QueryRow(r.Context(), `INSERT INTO public.dispute_change_requests
		(tenant, case_id, tpa_id, requested_by, requested_name, packet, changes, reason)
		VALUES ($1,$2,$3::uuid,$4,$5,$6::jsonb,$7::jsonb,$8) RETURNING id::text`,
		tenant, caseID, org.ID, p.Subject, who, string(packetJS), string(changesJS), in.Reason).Scan(&reqID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a change request for this dispute is already waiting for a case manager: withdraw it to send a new one"})
		return
	}
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	s.recordChange(r.Context(), tenant, caseID, "REQUESTED", p.Subject, who, "FILER", changes, in.Reason, reqID)
	s.logAudit(r.Context(), tenant, caseID, "TPA_CHANGE_REQUESTED", map[string]any{"by": p.Subject, "tpa_id": org.ID,
		"case_number": number, "request_id": reqID, "changes": changes, "reason": in.Reason})
	s.logActivity(r.Context(), tenant, caseID, "TPA_CHANGE_REQUESTED",
		fmt.Sprintf("Change requested by %s (%s): %s. Reason: %s", who, org.Name, changeSummary(changes), in.Reason))
	link := "#/cases/" + caseID
	body := fmt.Sprintf("%s asks to change dispute %s (%s)", org.Name, number, changeSummary(changes))
	s.notify(r, tenant, "role:CASE_MANAGER", "CHANGE_REQUEST", body, link)
	var assignee string
	_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT coalesce(assigned_to,'') FROM tenant_%s.cases WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&assignee)
	if assignee != "" {
		s.notify(r, tenant, assignee, "CHANGE_REQUEST", body, link)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"request_id": reqID, "changes": changes})
}

// tpaChangeRequestWithdraw: DELETE /tpa/disputes/{caseId}/change-requests/{requestId}.
func (s *server) tpaChangeRequestWithdraw(w http.ResponseWriter, r *http.Request) {
	tenant, caseID, org, p, ok := s.tpaOwnDispute(w, r)
	if !ok {
		return
	}
	reqID := chi.URLParam(r, "requestId")
	var changes json.RawMessage
	err := s.db.QueryRow(r.Context(), `UPDATE public.dispute_change_requests SET status='WITHDRAWN', decided_by=$4,
		decided_name=$5, decided_at=now() WHERE tenant=$1 AND case_id=$2 AND id::text=$3 AND status='PENDING' RETURNING changes`,
		tenant, caseID, reqID, p.Subject, displayName(p)).Scan(&changes)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, `{"error":"no open change request with that id on this dispute"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	var list []fieldChange
	_ = json.Unmarshal(changes, &list)
	s.recordChange(r.Context(), tenant, caseID, "WITHDRAWN", p.Subject, displayName(p), "FILER", list, "", reqID)
	s.logAudit(r.Context(), tenant, caseID, "TPA_CHANGE_WITHDRAWN", map[string]any{"by": p.Subject, "tpa_id": org.ID, "request_id": reqID})
	s.logActivity(r.Context(), tenant, caseID, "TPA_CHANGE_WITHDRAWN", fmt.Sprintf("Change request withdrawn by %s (%s)", displayName(p), org.Name))
	w.WriteHeader(http.StatusNoContent)
}

func canDecideChange(p principal) bool {
	return hasAnyRole(p, "CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN")
}

// caseChangeRequests: GET /cases/{caseId}/change-requests — staff: the case's requests, newest first, and history.
func (s *server) caseChangeRequests(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN", "FINANCE", "AUDITOR") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	writeJSON(w, http.StatusOK, map[string]any{"requests": s.changeRequests(r.Context(), tenant, caseID, false),
		"history": s.disputeHistory(r.Context(), tenant, caseID), "can_decide": canDecideChange(p)})
}

type changeDecision struct {
	Approve bool   `json:"approve"`
	Note    string `json:"note"`
}

// caseChangeDecide: POST /cases/{caseId}/change-requests/{requestId}/decide — staff approve (the change is applied,
// as it compares with the dispute now) or reject (a note is required).
func (s *server) caseChangeDecide(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !canDecideChange(p) {
		http.Error(w, `{"error":"forbidden: a case manager or the PM decides change requests"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID, reqID := chi.URLParam(r, "caseId"), chi.URLParam(r, "requestId")
	var in changeDecision
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	if !in.Approve && in.Note == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "say why the request is rejected: the filer sees this note"})
		return
	}
	if len(in.Note) > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the note is too long (2,000 characters at most)"})
		return
	}
	who := displayName(p)
	status := map[bool]string{true: "APPROVED", false: "REJECTED"}[in.Approve]
	// Claim the request first, so two people deciding at once cannot both apply it.
	var packetJS []byte
	var tpaID string
	err := s.db.QueryRow(r.Context(), `UPDATE public.dispute_change_requests SET status=$4, decided_by=$5, decided_name=$6,
		decided_at=now(), decision_note=$7 WHERE tenant=$1 AND case_id=$2 AND id::text=$3 AND status='PENDING'
		RETURNING packet, coalesce(tpa_id::text,'')`, tenant, caseID, reqID, status, p.Subject, who, in.Note).Scan(&packetJS, &tpaID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, `{"error":"no open change request with that id on this case (already decided or withdrawn?)"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	undo := func() { // the request goes back to waiting when the change cannot be applied
		_, _ = s.db.Exec(r.Context(), `UPDATE public.dispute_change_requests SET status='PENDING', decided_by=NULL,
			decided_name=NULL, decided_at=NULL, decision_note='' WHERE tenant=$1 AND id::text=$2`, tenant, reqID)
	}
	var changes []fieldChange
	_, _, number, _ := s.readFilingState(r.Context(), tenant, caseID)
	if in.Approve {
		var pk filingPacket
		_ = json.Unmarshal(packetJS, &pk)
		details, disputed, claims, problems := pk.normalize()
		if len(problems) > 0 {
			undo()
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the requested filing has problems", "problems": problems})
			return
		}
		before, _, _, err := s.readFilingState(r.Context(), tenant, caseID)
		if err == nil {
			changes, err = s.replaceFiling(r.Context(), tenant, caseID, before, details, disputed, claims)
		}
		if err != nil {
			undo()
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		if disputed != nil {
			s.amountRaised(r, tenant, caseID, amountOf(before.Amount), *disputed)
		}
	} else {
		var js json.RawMessage
		_ = s.db.QueryRow(r.Context(), `SELECT changes FROM public.dispute_change_requests WHERE tenant=$1 AND id::text=$2`,
			tenant, reqID).Scan(&js)
		_ = json.Unmarshal(js, &changes)
	}
	s.recordChange(r.Context(), tenant, caseID, status, p.Subject, who, "OFFICE", changes, in.Note, reqID)
	s.logAudit(r.Context(), tenant, caseID, "TPA_CHANGE_"+status, map[string]any{"by": p.Subject, "tpa_id": tpaID,
		"case_number": number, "request_id": reqID, "changes": changes, "note": in.Note})
	verb := map[bool]string{true: "approved and applied", false: "rejected"}[in.Approve]
	s.logActivity(r.Context(), tenant, caseID, "TPA_CHANGE_"+status, fmt.Sprintf("Change request %s by %s%s", verb, who,
		map[bool]string{true: ": " + changeSummary(changes), false: ". Note: " + in.Note}[in.Approve]))
	s.notifyOrg(r.Context(), tenant, "tpa", tpaID, caseID, "CHANGE_REQUEST_"+status,
		fmt.Sprintf("Your change request for dispute %s was %s%s", number, verb,
			map[bool]string{true: "", false: ": " + in.Note}[in.Approve || in.Note == ""]), "#/tpa")
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "changes": changes})
}
