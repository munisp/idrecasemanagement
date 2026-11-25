package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Deadline extensions (scripts/deadline_extensions.sql): the PM moves one of a case's program clocks by a number of
// days, with a written reason. The projection (projectProgramClocks) adds the extensions to the clock's due date and
// hard limit, and the case's workflow is signalled DEADLINE_EXTENDED so its follow-ups and breach wait as well.

// nonExtendableClocks are statutory windows nobody can move: AHCA no longer grants extensions on the filing window
// (the same rule the eligibility override enforces in program.go).
var nonExtendableClocks = map[string]bool{"FILING_ELIGIBILITY": true}

const maxExtensionDays = 90

// clockExtensions: clock → total days extended, and every extension (oldest first) for the deadline table.
func (s *server) clockExtensions(ctx context.Context, tenant, caseID string) (map[string]int, map[string][]map[string]any) {
	total, list := map[string]int{}, map[string][]map[string]any{}
	rows, err := s.db.Query(ctx, `
		SELECT clock, days, reason, granted_by, created_at FROM public.case_deadline_extensions
		WHERE tenant=$1 AND case_id=$2 ORDER BY created_at DESC`, tenant, caseID)
	if err != nil {
		return total, list // not migrated yet: no extensions, the deadlines still project
	}
	defer rows.Close()
	for rows.Next() {
		var clock, reason, by string
		var days int
		var at any
		if rows.Scan(&clock, &days, &reason, &by, &at) == nil {
			total[clock] += days
			list[clock] = append(list[clock], map[string]any{"days": days, "reason": reason, "granted_by": by, "at": at})
		}
	}
	return total, list
}

// extendDeadline: POST /cases/{caseId}/clocks/{clock}/extend {"days": 5, "reason": "..."}
func (s *server) extendDeadline(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, extensionRoles...) {
		http.Error(w, `{"error":"forbidden: deadlines are extended by the PM"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID, clock := chi.URLParam(r, "caseId"), strings.ToUpper(chi.URLParam(r, "clock"))
	in, ok := readExtension(w, r)
	if !ok {
		return
	}
	rule, caseNumber, code, msg := s.extensionTarget(r, tenant, caseID, clock)
	if code != 0 {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), code)
		return
	}
	if s.refusePastContractLimit(w, r, tenant, caseID, rule, in.Days, in.AgencyApproval) {
		return
	}
	id, signalled, err := s.applyExtension(r, tenant, caseID, caseNumber, rule, in.Days, in.Reason, displayName(p), p.Subject, in.AgencyApproval)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "clock": clock, "days": in.Days, "workflow_signalled": signalled})
}

// extensionRoles extend a deadline, or ask a party to agree to one.
var extensionRoles = []string{"PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN"}

type extensionIn struct {
	Days    int    `json:"days"`
	Reason  string `json:"reason"`
	Side    string `json:"side"`     // a request: PROVIDER | HEALTH_PLAN (default: the side that filed)
	DueDays int    `json:"due_days"` // a request: days the side has to answer (default 5)
	// AgencyApproval: AHCA's approval (a reference: who approved, when, how) -- required for an extension that takes the
	// deadline past the clock's contract limit (max_days), and kept with the extension and on the audit chain.
	AgencyApproval string `json:"agency_approval"`
}

func readExtension(w http.ResponseWriter, r *http.Request) (extensionIn, bool) {
	var in extensionIn
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if json.Unmarshal(raw, &in) != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return in, false
	}
	in.Reason = strings.TrimSpace(in.Reason)
	in.AgencyApproval = strings.TrimSpace(in.AgencyApproval)
	if len([]rune(in.AgencyApproval)) > 500 {
		http.Error(w, `{"error":"the AHCA approval reference is too long (500 characters at most)"}`, http.StatusBadRequest)
		return in, false
	}
	if in.Days < 1 || in.Days > maxExtensionDays {
		http.Error(w, fmt.Sprintf(`{"error":"an extension is 1 to %d days"}`, maxExtensionDays), http.StatusBadRequest)
		return in, false
	}
	if len([]rune(in.Reason)) < 10 {
		http.Error(w, `{"error":"give the reason for the extension (at least 10 characters) — it is kept on the case record"}`, http.StatusBadRequest)
		return in, false
	}
	return in, true
}

// extensionTarget: the clock a case's extension moves, if it can be moved. code != 0: refused, with why.
func (s *server) extensionTarget(r *http.Request, tenant, caseID, clock string) (*ClockRule, string, int, string) {
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		return nil, "", http.StatusBadRequest, "no program rules for tenant"
	}
	var rule *ClockRule
	for i := range cfg.Clocks {
		if cfg.Clocks[i].Name == clock {
			rule = &cfg.Clocks[i]
		}
	}
	if rule == nil {
		return nil, "", http.StatusNotFound, fmt.Sprintf("unknown deadline %q", clock)
	}
	if nonExtendableClocks[clock] {
		return nil, "", http.StatusUnprocessableEntity, "this deadline is statutory and cannot be extended (AHCA no longer grants extensions on the filing window)"
	}
	var caseNumber string
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)),
		caseID).Scan(&caseNumber); err != nil {
		return nil, "", http.StatusNotFound, "not found"
	}
	// Only a running clock can be extended: one that hasn't started has no due date to move.
	dates := s.programDates(r, tenant, caseID)
	if rule.Basis != "received_at" && strings.TrimSpace(dates[rule.Basis]) == "" {
		return nil, "", http.StatusUnprocessableEntity, "this deadline has not started yet — it can be extended once its clock is running"
	}
	return rule, caseNumber, 0, ""
}

// applyExtension records an extension and tells the case's workflow, the case record and the Reviewers.
func (s *server) applyExtension(r *http.Request, tenant, caseID, caseNumber string, rule *ClockRule, days int, reason, by, bySub, agencyApproval string) (string, bool, error) {
	clock := rule.Name
	// Record first, then tell the workflow: a failed insert must never leave the workflow extended with no record.
	var id string
	if err := s.db.QueryRow(r.Context(), `
		INSERT INTO public.case_deadline_extensions (tenant, case_id, clock, days, reason, granted_by, agency_approval)
		VALUES ($1,$2,$3,$4,$5,$6,nullif($7,'')) RETURNING id`,
		tenant, caseID, clock, days, reason, by, agencyApproval).Scan(&id); err != nil {
		return "", false, err
	}
	signalled := false
	if s.tc != nil {
		wfID := fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), caseNumber)
		signalled = s.sigCase(r.Context(), tenant, caseID, wfID, "DEADLINE_EXTENDED",
			map[string]any{"clock": clock, "days": days, "by": by}) == nil
	}
	if signalled {
		_, _ = s.db.Exec(r.Context(), `UPDATE public.case_deadline_extensions SET workflow_signalled=true WHERE id=$1`, id)
	}
	msg := fmt.Sprintf("%s deadline extended by %d day%s by %s — %s", clockLabel(rule), days, map[bool]string{true: "", false: "s"}[days == 1], by, reason)
	if agencyApproval != "" {
		msg += " (past the contract limit, approved by AHCA: " + agencyApproval + ")"
	}
	s.logActivity(r.Context(), tenant, caseID, "DEADLINE_EXTENDED", msg)
	s.logAudit(r.Context(), tenant, caseID, "DEADLINE_EXTENDED", map[string]any{
		"by": bySub, "clock": clock, "days": days, "reason": reason, "extension_id": id, "workflow_signalled": signalled,
		"agency_approval": agencyApproval})
	s.notify(r, tenant, "role:CASE_MANAGER", "DEADLINE_EXTENDED", fmt.Sprintf("Case %s: %s", caseNumber, msg), "#/cases/"+caseID)
	return id, signalled, nil
}

// refusePastContractLimit answers 422 when an extension would take the deadline past the clock's contract limit
// (max_days, counted with the extensions already granted) without AHCA's approval. Decided 2026-11-13: an extension
// past the limit is allowed only with AHCA's approval, recorded. Returns true when it answered.
func (s *server) refusePastContractLimit(w http.ResponseWriter, r *http.Request, tenant, caseID string, rule *ClockRule, days int, agencyApproval string) bool {
	if rule.MaxDays <= 0 {
		return false
	}
	var granted int
	_ = s.db.QueryRow(r.Context(), `SELECT coalesce(sum(days),0) FROM public.case_deadline_extensions WHERE tenant=$1 AND case_id=$2 AND clock=$3`,
		tenant, caseID, rule.Name).Scan(&granted)
	total := rule.Days + granted + days
	if total <= rule.MaxDays || len([]rune(agencyApproval)) >= 4 {
		return false
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error": fmt.Sprintf("this would put the %s deadline at day %d, past the contract's limit of %d days: an extension past it needs AHCA's approval — enter AHCA's approval reference",
			clockLabel(rule), total, rule.MaxDays),
		"needs_agency_approval": true, "contract_limit_days": rule.MaxDays, "would_be_day": total})
	return true
}

func clockLabel(rule *ClockRule) string {
	if rule.Label != "" {
		return rule.Label
	}
	return rule.Name
}

// requestExtension: POST /cases/{caseId}/clocks/{clock}/extension-request {"days","reason","side","due_days"} --
// the PM or a tenant admin asks a side on the platform (the TPA filing for the provider, or the health plan) to
// agree to more time on a deadline. The side answers in its portal (document request EXTENSION); agreeing
// extends the deadline at once (orgDocRequestSubmit → applyExtension).
func (s *server) requestExtension(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, extensionRoles...) {
		http.Error(w, `{"error":"forbidden: the PM or a tenant admin asks for more time"}`, http.StatusForbidden)
		return
	}
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID, clock := chi.URLParam(r, "caseId"), strings.ToUpper(chi.URLParam(r, "clock"))
	in, ok := readExtension(w, r)
	if !ok {
		return
	}
	rule, _, code, msg := s.extensionTarget(r, tenant, caseID, clock)
	if code != 0 {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), code)
		return
	}
	if s.refusePastContractLimit(w, r, tenant, caseID, rule, in.Days, in.AgencyApproval) {
		return
	}
	side := strings.ToUpper(strings.TrimSpace(in.Side))
	if side == "" {
		side = "PROVIDER"
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT CASE WHEN upper(coalesce(details->>'filing_party_type','')) = 'HEALTH_PLAN'
			THEN 'HEALTH_PLAN' ELSE 'PROVIDER' END FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&side)
	}
	if side != "PROVIDER" && side != "HEALTH_PLAN" {
		http.Error(w, `{"error":"side must be PROVIDER or HEALTH_PLAN"}`, http.StatusBadRequest)
		return
	}
	if !s.sideJoined(r.Context(), tenant, caseID, side) {
		http.Error(w, `{"error":"the other side is not part of this dispute yet — it joins once the dispute is found eligible"}`, http.StatusConflict)
		return
	}
	pc := s.sideContact(r.Context(), tenant, caseID, side)
	if pc.org == "" {
		http.Error(w, `{"error":"this side has no portal account — ask it by email, and use Extend once it agrees"}`, http.StatusConflict)
		return
	}
	var open bool
	_ = s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM public.document_requests WHERE tenant=$1 AND case_id=$2
		AND reason='EXTENSION' AND status='OPEN' AND meta->>'clock'=$3)`, tenant, caseID, clock).Scan(&open)
	if open {
		http.Error(w, `{"error":"more time on this deadline has already been asked for and is waiting for an answer"}`, http.StatusConflict)
		return
	}
	if in.DueDays < 1 || in.DueDays > 30 {
		in.DueDays = 5
	}
	due, newDue := "", ""
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		for _, v := range s.programClockViews(r, tenant, caseID, cfg) {
			if v.Clock == clock && v.Due != "" {
				due = v.Due
				if t, err := time.Parse("2006-01-02", v.Due); err == nil {
					newDue = t.AddDate(0, 0, in.Days).Format("2006-01-02")
				}
			}
		}
	}
	item := fmt.Sprintf("Your agreement to %d more day%s on the %s deadline", in.Days, map[bool]string{true: "", false: "s"}[in.Days == 1], clockLabel(rule))
	if due != "" && newDue != "" {
		item += fmt.Sprintf(" (from %s to about %s)", due, newDue)
	}
	id, _, err := s.openDocRequest(r, tenant, caseID, side, "EXTENSION", "extension_request", []string{item}, in.Reason, in.DueDays, p.Subject)
	if err != nil {
		http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
		return
	}
	meta, _ := json.Marshal(map[string]any{"clock": clock, "label": clockLabel(rule), "days": in.Days, "reason": in.Reason, "due": due, "new_due": newDue,
		"agency_approval": in.AgencyApproval})
	_, _ = s.db.Exec(r.Context(), `UPDATE public.document_requests SET meta=$2::jsonb WHERE id::text=$1`, id, string(meta))
	s.logActivity(r.Context(), tenant, caseID, "EXTENSION_REQUESTED", fmt.Sprintf("%s asked %s for %d more day%s on the %s deadline — %s",
		displayName(p), pc.name, in.Days, map[bool]string{true: "", false: "s"}[in.Days == 1], clockLabel(rule), in.Reason))
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "side": side, "asked": pc.name, "clock": clock, "days": in.Days})
}

// extensionAnswered: the side agreed (the deadline moves now) or declined (the PM who asked is told).
func (s *server) extensionAnswered(r *http.Request, tenant, caseID, caseNumber, reqID, orgName string, agreed bool) (bool, error) {
	var metaRaw []byte
	_ = s.db.QueryRow(r.Context(), `SELECT coalesce(meta,'{}'::jsonb)::text FROM public.document_requests WHERE id::text=$1`, reqID).Scan(&metaRaw)
	var m struct {
		Clock, Reason  string
		Days           int
		AgencyApproval string `json:"agency_approval"`
	}
	_ = json.Unmarshal(metaRaw, &m)
	if !agreed || m.Clock == "" {
		return false, nil
	}
	cfg := s.loadProgram(r, tenant)
	if cfg == nil {
		return false, fmt.Errorf("no program rules")
	}
	for i := range cfg.Clocks {
		if cfg.Clocks[i].Name == m.Clock {
			_, signalled, err := s.applyExtension(r, tenant, caseID, caseNumber, &cfg.Clocks[i], m.Days,
				m.Reason+" — agreed by "+orgName+" in its portal", orgName, "", m.AgencyApproval)
			return signalled, err
		}
	}
	return false, fmt.Errorf("unknown deadline %s", m.Clock)
}
