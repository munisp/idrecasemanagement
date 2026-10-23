package main

// helpers.go — the specialists on a case, alongside its Reviewer.
//
// A case belongs to its Reviewer from intake to close (cases.assigned_to; casescope.go decides what a Reviewer sees
// from it). The Coder, the Clinical reviewer, the Attorney and the Arbitrator do their part of the SOP as well, and
// each can be named on the case (public.case_helpers, scripts/case_helpers.sql): one person per role, without taking
// the case off the Reviewer.
//
//   - The PM names or changes one on the case (assignCase with a specialist role), by hand or "Next in turn".
//   - When the case's next step belongs to a specialist role (workflowsteps.go: coding, clinical and attorney
//     review) and nobody holds that place, the role's rotation gives it to its next person (autoassign.go) and puts
//     the case in that discipline's queue, which lists a specialist's own cases first.
//
// Specialists see every case already (casescope.go), so this decides who is responsible, not who can look.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// helperRoles: the roles named alongside the Reviewer, and the review queue each works from ("" = none).
var helperRoles = map[string]string{"CODER": "CODING", "NURSE_PHYSICIAN": "CLINICAL", "ATTORNEY": "LEGAL", "ARBITRATOR": ""}

// disciplineRole: the specialist role behind a review queue.
var disciplineRole = map[string]string{"CODING": "CODER", "CLINICAL": "NURSE_PHYSICIAN", "LEGAL": "ATTORNEY"}

func isHelperRole(role string) bool { _, ok := helperRoles[role]; return ok }

// caseHelpers: role → user id of the specialists named on a case.
func (s *server) caseHelpers(ctx context.Context, tenant, caseID string) map[string]string {
	out := map[string]string{}
	rows, err := s.db.Query(ctx, `SELECT role, user_id FROM public.case_helpers WHERE tenant=$1 AND case_id=$2`, tenant, caseID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var role, who string
		if rows.Scan(&role, &who) == nil {
			out[role] = who
		}
	}
	return out
}

// helperView: the specialists on a case, by name, in a fixed order — for the case page.
func (s *server) helperView(ctx context.Context, tenant, caseID string) []map[string]any {
	h := s.caseHelpers(ctx, tenant, caseID)
	ids := []string{}
	for _, who := range h {
		ids = append(ids, who)
	}
	names := s.resolveSubjectNames(ctx, ids)
	out := []map[string]any{}
	for _, role := range []string{"CODER", "NURSE_PHYSICIAN", "ATTORNEY", "ARBITRATOR"} {
		if who, ok := h[role]; ok {
			out = append(out, map[string]any{"role": role, "role_label": assignableRoles[role], "id": who, "name": names[who]})
		}
	}
	return out
}

const upsertHelperSQL = `INSERT INTO public.case_helpers (tenant, case_id, role, user_id, assigned_by, assigned_at) VALUES ($1,$2,$3,$4,$5,now())
	ON CONFLICT (tenant, case_id, role) DO UPDATE SET user_id=$4, assigned_by=$5, assigned_at=now()`

// assignHelper: assignCase for a specialist role — names, changes or removes the Coder / Clinical reviewer /
// Attorney / Arbitrator on a case. The Reviewer keeps the case. Same body as assignCase: assignee (or next=true for
// the role's next in turn, or "" for whoever has the fewest open cases), expected (the person the caller saw in
// that place; a change since is a 409), action "unassign", reason.
func (s *server) assignHelper(w http.ResponseWriter, r *http.Request, p principal, tenant, caseID, cn, role, assignee, reason, action string, next bool, expected *string) {
	ctx := r.Context()
	label := assignableRoles[role]
	prev := s.caseHelpers(ctx, tenant, caseID)[role]
	if expected != nil && *expected != prev {
		s.assignmentChanged(w, r, prev)
		return
	}
	by := displayName(p)
	names := s.resolveSubjectNames(ctx, []string{prev})

	if action == "unassign" {
		if prev == "" {
			writeJSON(w, http.StatusOK, map[string]any{"role": role, "assigned_to": nil, "unchanged": true})
			return
		}
		tag, err := s.db.Exec(ctx, `DELETE FROM public.case_helpers WHERE tenant=$1 AND case_id=$2 AND role=$3 AND user_id=$4`, tenant, caseID, role, prev)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		if tag.RowsAffected() == 0 {
			s.assignmentChanged(w, r, s.caseHelpers(ctx, tenant, caseID)[role])
			return
		}
		s.notify(r, tenant, prev, "ASSIGNMENT", fmt.Sprintf("You are no longer the %s on %s (%s)", label, cn, by), "#/cases/"+caseID)
		s.logActivity(ctx, tenant, caseID, "MILESTONE", fmt.Sprintf("%s %s taken off the case by %s%s", label, names[prev], by, withReason(reason)))
		s.logAudit(ctx, tenant, caseID, "CASE_HELPER_REMOVED", map[string]any{"by": p.Subject, "role": role, "previous": prev, "reason": reason})
		writeJSON(w, http.StatusOK, map[string]any{"role": role, "assigned_to": nil})
		return
	}

	who, name, viaTurn := assignee, "", false
	switch {
	case next:
		changed := false
		var err error
		who, name, err = s.takeTurn(ctx, tenant, role, prev, func(tx pgx.Tx, who string) (bool, error) {
			if who == prev {
				return false, nil // the only one in the rotation, and they have it already
			}
			var cur *string
			if err := tx.QueryRow(ctx, `SELECT user_id FROM public.case_helpers WHERE tenant=$1 AND case_id=$2 AND role=$3 FOR UPDATE`,
				tenant, caseID, role).Scan(&cur); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return false, err
			}
			if deref(cur) != prev {
				changed = true
				return false, nil
			}
			_, err := tx.Exec(ctx, upsertHelperSQL, tenant, caseID, role, who, p.Subject)
			return err == nil, err
		})
		switch {
		case errors.Is(err, errRotationOff):
			writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("the %s rotation is off — pick a person, or turn it on on the Team page", label)})
			return
		case errors.Is(err, errRotationEmpty):
			writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("nobody is in the %s rotation — pick a person, or add one on the Team page", label)})
			return
		case err != nil:
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not take the next in turn: " + err.Error()})
			return
		case changed:
			s.assignmentChanged(w, r, s.caseHelpers(ctx, tenant, caseID)[role])
			return
		case who == "":
			writeJSON(w, http.StatusOK, map[string]any{"role": role, "assigned_to": prev, "name": names[prev], "unchanged": true})
			return
		}
		viaTurn = true
	case who == "":
		list, err := s.assigneesFor(ctx, tenant, role) // least loaded first
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "staff directory unavailable: " + err.Error()})
			return
		}
		for _, a := range list {
			if a.ID != prev || len(list) == 1 {
				who, name = a.ID, a.Username
				break
			}
		}
		if who == "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("no active %s on this tenant — add one on the Team page", label)})
			return
		}
	default:
		n, err := s.validAssignee(ctx, tenant, role, who)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "staff directory unavailable: " + err.Error()})
			return
		}
		if n == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("that person is not an active %s on this tenant", label)})
			return
		}
		name = n
	}
	if !viaTurn {
		if who == prev {
			writeJSON(w, http.StatusOK, map[string]any{"role": role, "assigned_to": who, "name": name, "unchanged": true})
			return
		}
		// Change-detected like the Reviewer's place: only when it still holds what we read.
		changed, err := s.writeHelper(ctx, tenant, caseID, role, who, p.Subject, prev)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		if changed {
			s.assignmentChanged(w, r, s.caseHelpers(ctx, tenant, caseID)[role])
			return
		}
	}
	how := ""
	if viaTurn {
		how = ", next in turn"
	}
	if prev != "" {
		s.notify(r, tenant, prev, "ASSIGNMENT", fmt.Sprintf("%s's %s place went to %s (%s)", cn, label, name, by), "#/cases/"+caseID)
		s.logActivity(ctx, tenant, caseID, "MILESTONE", fmt.Sprintf("%s changed from %s to %s%s by %s%s", label, names[prev], name, how, by, withReason(reason)))
	} else {
		s.logActivity(ctx, tenant, caseID, "MILESTONE", fmt.Sprintf("%s: %s%s, added by %s%s", label, name, how, by, withReason(reason)))
	}
	s.notify(r, tenant, who, "ASSIGNMENT", fmt.Sprintf("You are the %s on %s (%s)", label, cn, by), "#/cases/"+caseID)
	s.logAudit(ctx, tenant, caseID, "CASE_HELPER_ASSIGNED", map[string]any{
		"by": p.Subject, "role": role, "assigned_to": who, "previous": prev, "reason": reason,
		"rule": map[bool]string{true: "least_loaded_round_robin", false: ""}[viaTurn],
	})
	// The work goes with the case: a review the previous specialist had claimed is now the new one's, and a case
	// already at this specialist's step is in their queue even if nothing had opened it yet.
	if disc := helperRoles[role]; disc != "" && prev != "" {
		_, _ = s.db.Exec(ctx, `UPDATE public.specialty_reviews SET reviewer=$4
			WHERE tenant=$1 AND case_id=$2 AND discipline=$3 AND status='IN_REVIEW'`, tenant, caseID, disc, name)
	}
	s.ensureStepHelper(r, tenant, caseID)
	writeJSON(w, http.StatusOK, map[string]any{"role": role, "assigned_to": who, "name": name, "previous": prev})
}

// writeHelper puts `who` in a role's place on a case if it still holds `prev` ("" = nobody); changed=true when it
// does not (someone else changed it first).
func (s *server) writeHelper(ctx context.Context, tenant, caseID, role, who, by, prev string) (changed bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var cur *string
	if err := tx.QueryRow(ctx, `SELECT user_id FROM public.case_helpers WHERE tenant=$1 AND case_id=$2 AND role=$3 FOR UPDATE`,
		tenant, caseID, role).Scan(&cur); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if deref(cur) != prev {
		return true, nil
	}
	if prev == "" {
		tag, err := tx.Exec(ctx, `INSERT INTO public.case_helpers (tenant, case_id, role, user_id, assigned_by, assigned_at)
			VALUES ($1,$2,$3,$4,$5,now()) ON CONFLICT DO NOTHING`, tenant, caseID, role, who, by)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() == 0 { // inserted by someone else a moment ago
			return true, nil
		}
	} else if _, err := tx.Exec(ctx, `UPDATE public.case_helpers SET user_id=$4, assigned_by=$5, assigned_at=now()
		WHERE tenant=$1 AND case_id=$2 AND role=$3`, tenant, caseID, role, who, by); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}

// ensureStepHelper: when the case's next step belongs to a specialist role and nobody holds that place, give it to
// the role's next in turn and put the case in that role's queue. Best-effort and quiet: rotation off or empty, or
// the case not programmed, leaves things as they are. Called after a step is recorded and when the steps are read
// (so cases already at such a step are covered too).
func (s *server) ensureStepHelper(r *http.Request, tenant, caseID string) {
	ctx := r.Context()
	if s.loadProgram(r, tenant) == nil {
		return
	}
	f, err := s.workflowFacts(ctx, tenant, caseID)
	if err != nil || wfClosedStatuses[f.Internal] {
		return
	}
	role := ""
	for _, st := range buildWorkflowSteps(f) {
		if st.State == "next" {
			for _, ro := range st.OwnerRoles {
				if isHelperRole(ro) {
					role = ro
				}
			}
			break
		}
	}
	if role == "" {
		return
	}
	var cn string
	_ = s.db.QueryRow(ctx, fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&cn)
	label := assignableRoles[role]
	disc := helperRoles[role]
	// Who works it: the specialist named on the case, else the next one in turn (when the rotation is on).
	named := s.caseHelpers(ctx, tenant, caseID)[role]
	name := ""
	if named == "" {
		who, n, err := s.takeTurn(ctx, tenant, role, "", func(tx pgx.Tx, who string) (bool, error) {
			tag, err := tx.Exec(ctx, `INSERT INTO public.case_helpers (tenant, case_id, role, user_id, assigned_by, assigned_at)
				VALUES ($1,$2,$3,$4,'auto-assign',now()) ON CONFLICT DO NOTHING`, tenant, caseID, role, who)
			return err == nil && tag.RowsAffected() > 0, err // someone named one a moment ago: no turn used
		})
		if err != nil && !errors.Is(err, errRotationOff) && !errors.Is(err, errRotationEmpty) {
			log.Printf("step helper %s/%s %s: %v", tenant, caseID, role, err)
		}
		if who != "" {
			name = n
			s.logActivity(ctx, tenant, caseID, "MILESTONE", fmt.Sprintf("%s: %s, automatically in turn (the case reached their step)", label, name))
			s.logAudit(ctx, tenant, caseID, "CASE_HELPER_ASSIGNED", map[string]any{
				"by": "auto-assign", "role": role, "assigned_to": who, "previous": "", "auto": true, "rule": "least_loaded_round_robin",
			})
			s.notify(r, tenant, who, "ASSIGNMENT", fmt.Sprintf("You are the %s on %s — it's at your step (your turn in the rotation)", label, cn), "#/cases/"+caseID)
		}
	} else {
		name = s.resolveSubjectNames(ctx, []string{named})[named]
	}
	// The case goes in that discipline's queue whether or not anyone could be given it: the queue is where the
	// specialists find work, and a case at their step must be there (a no-op while it already has an open review).
	if disc != "" {
		note := fmt.Sprintf("The case reached the %s step; %s has it.", label, orDash(name))
		if name == "" {
			note = fmt.Sprintf("The case reached the %s step; nobody is named yet — claim it from the queue.", label)
		}
		s.openReview(r, tenant, caseID, cn, disc, note, "auto-assign")
	}
}

// helperCounts: user id → open cases they are named on as a specialist (adds to openCaseCounts' Reviewer load).
func (s *server) helperCounts(ctx context.Context, tenant string) map[string]int {
	out := map[string]int{}
	rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT h.user_id, count(*) FROM public.case_helpers h JOIN tenant_%s.cases c ON c.id::text = h.case_id
		WHERE h.tenant=$1 AND `+openCaseCond+` GROUP BY 1`, sanitizeTenant(tenant)), tenant)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var who string
		var n int
		if rows.Scan(&who, &n) == nil {
			out[who] = n
		}
	}
	return out
}

// catchUpStepQueues: once at start, every open case at its review stage with no open specialty review is checked,
// so a case the workflow had already moved to a specialist's step (before setDualStatus opened queue items itself)
// is put in that queue and given its specialist. Paced, so a large tenant is never one burst of queries.
func (s *server) catchUpStepQueues(stop <-chan struct{}) {
	ctx := context.Background()
	rows, err := s.db.Query(ctx, `SELECT tenant FROM public.program_rules`)
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
	opened := 0
	for _, tenant := range tenants {
		crows, err := s.db.Query(ctx, fmt.Sprintf(`
			SELECT c.id::text FROM tenant_%s.cases c
			WHERE `+openCaseCond+` AND c.internal_status IN ('Review In Progress','Plan - No Response',
			      'RFI Pending Provider Response','RFI Pending Plan Response','QA Final Determination')
			  AND NOT EXISTS (SELECT 1 FROM public.specialty_reviews v WHERE v.tenant=$1 AND v.case_id=c.id::text AND v.status<>'COMPLETE')`,
			sanitizeTenant(tenant)), tenant)
		if err != nil {
			continue
		}
		var ids []string
		for crows.Next() {
			var id string
			if crows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		crows.Close()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		for _, id := range ids {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			before := s.openReviewCount(ctx, tenant, id)
			s.ensureStepHelper(req, tenant, id)
			if s.openReviewCount(ctx, tenant, id) > before {
				opened++
			}
		}
	}
	if opened > 0 {
		slog.Info("specialty queues caught up", "opened", opened)
	}
}

func (s *server) openReviewCount(ctx context.Context, tenant, caseID string) int {
	var n int
	_ = s.db.QueryRow(ctx, `SELECT count(*) FROM public.specialty_reviews WHERE tenant=$1 AND case_id=$2 AND status<>'COMPLETE'`, tenant, caseID).Scan(&n)
	return n
}
