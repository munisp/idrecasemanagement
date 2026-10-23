package main

// engines.go — CRM + case-management engine enhancements:
//   1. Statutory clock projection (GET /cases/clocks, GET /cases/{id}/clocks) —
//      server-computed business/calendar-day countdowns for every 45 CFR Part 149
//      clock, so every UI surface shows the same truth.
//   2. Bulk case operations (POST /cases/bulk) — assign / status change with
//      per-item results, activity logging, and outbox events.
//   3. Queue "grab next" (POST /queues/grab-next) — atomic claim of the oldest
//      unassigned actionable case (FOR UPDATE SKIP LOCKED).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---- Statutory clock projection -------------------------------------------------

type ClockView struct {
	Clock     string `json:"clock"`              // NEGOTIATION_30BD | OFFER_WINDOW_10BD | DETERMINATION_30BD | PAYMENT_30CD
	Label     string `json:"label"`              // human label
	Basis     string `json:"basis"`              // business | calendar
	TotalDays int    `json:"total_days"`         // statutory length
	Remaining int    `json:"remaining"`          // days remaining (negative = breach)
	Due       string `json:"due"`                // YYYY-MM-DD
	State     string `json:"state"`              // ok | watch | risk | breach | closed | paused
	Cite      string `json:"cite"`               // 45 CFR citation
	BasisNote string `json:"basis_note"`         // provenance of the start timestamp
	HardDue   string `json:"hard_due,omitempty"` // contract hard limit, when later than Due (state "late" in between)
	// PM deadline extensions (deadline_extensions.go): days added to Due/HardDue, and each extension granted.
	ExtendedDays int              `json:"extended_days,omitempty"`
	Extensions   []map[string]any `json:"extensions,omitempty"`
	Extendable   bool             `json:"extendable,omitempty"`
	// MetOn: the day the step this clock times was done (state met / met_late); the clock no longer counts.
	MetOn  string `json:"met_on,omitempty"`
	Target bool   `json:"target,omitempty"` // a working target, not a deadline (state over_target when passed)
}

// addBusinessDays returns the date n business days after start (weekend-aware;
// tenant holiday calendars in state_config.extra_holidays can refine this later).
func addBusinessDays(start time.Time, n int) time.Time {
	d := start
	for n > 0 {
		d = d.AddDate(0, 0, 1)
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			n--
		}
	}
	return d
}

func clockState(remaining, total int) string {
	switch {
	case remaining < 0:
		return "breach"
	case total > 0 && remaining*4 < total: // <25%
		return "risk"
	case total > 0 && remaining*2 < total: // <50%
		return "watch"
	default:
		return "ok"
	}
}

type clockRow struct {
	id, status          string
	openedAt, updatedAt time.Time
	negEnd, offerEnds   *time.Time
}

func projectClocks(c clockRow, today time.Time) []ClockView {
	var out []ClockView
	mk := func(clock, label, basis string, total int, due time.Time, cite, note string) ClockView {
		rem := 0
		if basis == "business" {
			if due.After(today) {
				rem = businessDaysBetween(today, due)
			} else {
				rem = -businessDaysBetween(due, today)
			}
		} else {
			rem = int(due.Sub(today).Hours() / 24)
		}
		return ClockView{Clock: clock, Label: label, Basis: basis, TotalDays: total,
			Remaining: rem, Due: due.Format("2006-01-02"),
			State: clockState(rem, total), Cite: cite, BasisNote: note}
	}
	switch c.status {
	case "NEGOTIATION_TRACKED":
		if c.negEnd != nil {
			out = append(out, mk("NEGOTIATION_30BD", "Open negotiation (30bd)", "business", 30, *c.negEnd,
				"45 CFR 149.510(b)(1)", "negotiation end date supplied at initiation"))
		}
	case "OFFER_WINDOW_OPEN":
		if c.offerEnds != nil {
			out = append(out, mk("OFFER_WINDOW_10BD", "Offer window (10bd)", "business", 10, *c.offerEnds,
				"45 CFR 149.510(b)(2)(ii)(B)", "set when the window opened"))
		}
	case "OFFERS_SEALED", "IN_REVIEW":
		start := c.updatedAt
		note := "status-change timestamp (offer-window close not recorded)"
		if c.offerEnds != nil {
			start = *c.offerEnds
			note = "offer-window close"
		}
		out = append(out, mk("DETERMINATION_30BD", "Determination due (30bd)", "business", 30,
			addBusinessDays(start, 30), "45 CFR 149.510(c)(4)(ii)(B)", note))
	case "DETERMINED", "PAYMENT_PENDING":
		out = append(out, mk("PAYMENT_30CD", "Payment due (30cd)", "calendar", 30,
			c.updatedAt.AddDate(0, 0, 30), "45 CFR 149.510(c)(4)(vii)",
			"determination timestamp (status change)"))
	}
	return out
}

func (s *server) loadClockRows(r *http.Request, tenant, caseID string) []clockRow {
	q := fmt.Sprintf(`SELECT id::text, status, opened_at, updated_at, open_negotiation_end, offer_window_ends_at
	                  FROM tenant_%s.cases WHERE status NOT LIKE 'CLOSED%%' AND status<>'SETTLED_IN_NEGOTIATION'`, sanitizeTenant(tenant))
	args := []any{}
	if caseID != "" {
		q += " AND id=$1"
		args = append(args, caseID)
	}
	q += " ORDER BY opened_at DESC LIMIT 200"
	rows, err := s.db.Query(r.Context(), q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []clockRow
	for rows.Next() {
		var c clockRow
		if rows.Scan(&c.id, &c.status, &c.openedAt, &c.updatedAt, &c.negEnd, &c.offerEnds) == nil {
			out = append(out, c)
		}
	}
	return out
}

// GET /cases/clocks — batch projection for grids (open cases only).
func (s *server) casesClocks(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	today := time.Now().Truncate(24 * time.Hour)
	out := []map[string]any{}
	for _, c := range s.loadClockRows(r, tenant, "") {
		out = append(out, map[string]any{"case_id": c.id, "clocks": projectClocks(c, today)})
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /cases/{caseId}/clocks — per-case projection for the workspace header.
func (s *server) caseClocks(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	caseID := chi.URLParam(r, "caseId")
	// Tenants with program_rules run the generic program clock engine (G1);
	// tenants without (federal NSA) keep the statutory 45 CFR projection.
	if cfg := s.loadProgram(r, tenant); cfg != nil {
		s.caseProgramClocks(w, r, tenant, caseID, cfg)
		return
	}
	today := time.Now().Truncate(24 * time.Hour)
	rows := s.loadClockRows(r, tenant, caseID)
	if len(rows) == 0 {
		http.Error(w, `{"error":"not found or no active clocks"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, projectClocks(rows[0], today))
}

// ---- Bulk operations --------------------------------------------------------------

// POST /cases/bulk {action: "assign"|"status", case_ids: [...], role?, assignee?, status?}
// Per-item results; every success writes activity + outbox event atomically per item.
func (s *server) bulkCases(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	// Tighter than routine single-case actions -- one call mutates many cases
	// at once, so the gate stays at management roles, not full case staff.
	if !hasAnyRole(p, "CASE_MANAGER", "PM", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires CASE_MANAGER, PM, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	var in struct {
		Action   string   `json:"action"`
		CaseIDs  []string `json:"case_ids"`
		Role     string   `json:"role"`
		Assignee string   `json:"assignee"`
		Status   string   `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.CaseIDs) == 0 {
		http.Error(w, `{"error":"action and case_ids required"}`, http.StatusBadRequest)
		return
	}
	if len(in.CaseIDs) > 200 {
		http.Error(w, `{"error":"max 200 items per bulk operation"}`, http.StatusBadRequest)
		return
	}
	t := sanitizeTenant(tenant)
	// Bulk assign: anyone allowed here may take cases themselves; only the PM (or an admin) routes them to someone
	// else -- the same rule as the single-case assign (assignment.go). A named assignee must be real staff.
	bulkAssignee, bulkAssigneeName := p.Subject, displayName(p)
	if in.Action == "assign" {
		if in.Role == "" {
			in.Role = "CASE_MANAGER"
		}
		if in.Assignee != "" && in.Assignee != p.Subject {
			if !hasAnyRole(p, "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
				http.Error(w, `{"error":"forbidden: only the PM assigns cases to someone else"}`, http.StatusForbidden)
				return
			}
			name, err := s.validAssignee(r.Context(), tenant, in.Role, in.Assignee)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "staff directory unavailable: " + err.Error()})
				return
			}
			if name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("that person is not an active %s on this tenant", assignableRoles[in.Role])})
				return
			}
			bulkAssignee, bulkAssigneeName = in.Assignee, name
		}
	}
	results := []map[string]any{}
	for _, id := range in.CaseIDs {
		res := map[string]any{"case_id": id}
		if lock := s.caseLockedForRequest(r, tenant, id); lock != "" {
			res["ok"], res["error"] = false, caseLockMessage(lock)
			results = append(results, res)
			continue
		}
		var err error
		switch in.Action {
		case "assign":
			role, assignee := in.Role, bulkAssignee
			var prev *string
			var cn string
			_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT case_number, assigned_to FROM tenant_%s.cases WHERE id=$1`, t), id).Scan(&cn, &prev)
			if isHelperRole(role) { // a specialist is named alongside the Reviewer, who keeps the case (helpers.go)
				was := s.caseHelpers(r.Context(), tenant, id)[role]
				_, err = s.db.Exec(r.Context(), upsertHelperSQL, tenant, id, role, assignee, p.Subject)
				if err == nil && was != assignee {
					s.logActivity(r.Context(), tenant, id, "MILESTONE", fmt.Sprintf("%s: %s, added by %s (bulk)", assignableRoles[role], bulkAssigneeName, displayName(p)))
					s.logAudit(r.Context(), tenant, id, "CASE_HELPER_ASSIGNED", map[string]any{"by": p.Subject, "role": role, "assigned_to": assignee, "previous": was, "bulk": true})
					if was != "" {
						s.notify(r, tenant, was, "ASSIGNMENT", fmt.Sprintf("%s's %s place went to %s (%s)", cn, assignableRoles[role], bulkAssigneeName, displayName(p)), "#/cases/"+id)
					}
					s.notify(r, tenant, assignee, "ASSIGNMENT", fmt.Sprintf("You are the %s on %s (%s)", assignableRoles[role], cn, displayName(p)), "#/cases/"+id)
				}
				break
			}
			_, err = s.db.Exec(r.Context(), fmt.Sprintf(
				`UPDATE tenant_%s.cases SET assigned_to=$1, assigned_role=$2, updated_at=now() WHERE id=$3`, t), assignee, role, id)
			if err == nil {
				s.logActivity(r.Context(), tenant, id, "MILESTONE", fmt.Sprintf("Assigned to %s (%s) by %s (bulk)", bulkAssigneeName, assignableRoles[role], displayName(p)))
				s.logAudit(r.Context(), tenant, id, "CASE_ASSIGNED", map[string]any{
					"by": p.Subject, "assigned_to": assignee, "role": role, "bulk": true,
				})
				if prev != nil && *prev != "" && *prev != assignee && *prev != "unassigned-pool" {
					s.notify(r, tenant, *prev, "ASSIGNMENT", fmt.Sprintf("%s was reassigned to %s by %s", cn, bulkAssigneeName, displayName(p)), "#/cases/"+id)
				}
				if assignee != p.Subject {
					s.notify(r, tenant, assignee, "ASSIGNMENT", fmt.Sprintf("You were assigned %s by %s", cn, displayName(p)), "#/cases/"+id)
				}
			}
		case "status":
			_, err = s.db.Exec(r.Context(), fmt.Sprintf(
				`UPDATE tenant_%s.cases SET status=$1, updated_at=now() WHERE id=$2`, t), in.Status, id)
			if err == nil {
				s.logActivity(r.Context(), tenant, id, "STATUS_CHANGE", fmt.Sprintf("Status changed to %s (bulk) by %s", in.Status, p.Subject))
				s.logAudit(r.Context(), tenant, id, "CASE_STATUS_CHANGED", map[string]any{
					"by": p.Subject, "status": in.Status, "bulk": true,
				})
			}
		default:
			http.Error(w, `{"error":"action must be assign|status"}`, http.StatusBadRequest)
			return
		}
		if err != nil {
			res["ok"], res["error"] = false, "db"
		} else {
			res["ok"] = true
			s.publish(r.Context(), tenant, "cases", map[string]any{
				"type": "BULK_" + in.Action, "case_id": id, "actor": p.Subject, "at": time.Now().UTC()})
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// ---- Grab-next (high-volume triage) ------------------------------------------------

// POST /queues/grab-next — atomically claim the oldest unassigned actionable case.
func (s *server) grabNext(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "CASE_MANAGER", "ARBITRATOR", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden: requires CASE_MANAGER, ARBITRATOR, FEDERAL_ADMIN, or PLATFORM_ADMIN"}`, http.StatusForbidden)
		return
	}
	if reviewerScoped(p) { // see casescope.go: a Reviewer does not pick from the unassigned cases
		http.Error(w, `{"error":"cases are given out in turn or by the PM — there is nothing to grab"}`, http.StatusForbidden)
		return
	}
	t := sanitizeTenant(tenant)
	var id, cn, status string
	err := s.db.QueryRow(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET assigned_to=$1, assigned_role='CASE_MANAGER', updated_at=now()
		WHERE id = (
			SELECT id FROM tenant_%s.cases
			WHERE assigned_to IS NULL
			  AND status IN ('INITIATED','OFFER_WINDOW_OPEN','NEGOTIATION_TRACKED')
			ORDER BY opened_at ASC LIMIT 1
			FOR UPDATE SKIP LOCKED
		) RETURNING id::text, case_number, status`, t, t), p.Subject).
		Scan(&id, &cn, &status)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"claimed": false, "message": "Queue is empty — nothing unassigned right now."})
		return
	}
	s.logActivity(r.Context(), tenant, id, "MILESTONE", fmt.Sprintf("Claimed from queue by %s (grab-next)", p.Subject))
	s.notify(r, tenant, p.Subject, "ASSIGNMENT", fmt.Sprintf("You claimed %s from the queue", cn), "#/cases/"+id)
	s.logAudit(r.Context(), tenant, id, "CASE_CLAIMED", map[string]any{
		"by": p.Subject, "case_number": cn, "status": status, "via": "grab-next",
	})
	writeJSON(w, http.StatusOK, map[string]any{"claimed": true, "case_id": id, "case_number": cn, "status": status})
}
