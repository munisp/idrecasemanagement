// qagate.go — what a letter held for QA, and a letter actually sent, do to
// the case around them.
//
// The SOP's "send through QA" pattern has three effects the correspondence
// engine did not have:
//
//   - While a draft waits, the case shows it: Internal Status becomes "QA
//     Initial Review" for the first letter (acceptance / ineligible /
//     dismissal) and "QA Letter" for every later one. The status it had is
//     kept on the draft and put back when the draft is decided.
//   - The person who must approve is told -- the template's QA role, not the
//     whole tenant.
//   - A stage moves when its letter has really gone out, not when someone
//     clicked a status: the send is signalled to the case workflow, which
//     waits for it (ahca_workflow.py).
package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	qaStatusInitialReview = "QA Initial Review"
	qaStatusLetter        = "QA Letter"
)

// qaInternalStatus is the Internal Status a case shows while this template's
// draft waits at the QA gate.
func qaInternalStatus(template string) string {
	switch template {
	case "acceptance", "ineligible", "dismissal":
		return qaStatusInitialReview
	}
	return qaStatusLetter
}

// notifTargets lists the notification addresses a principal reads: their
// own, the tenant-wide broadcast, and one per role they hold ("role:PM").
// A tenant admin stands in for every tenant staff role.
func notifTargets(p principal) []string {
	out := []string{p.Subject, "*"}
	for _, r := range p.Roles {
		out = append(out, "role:"+r)
	}
	if contains(p.Roles, "TENANT_ADMIN") {
		for r := range tenantStaffRoles {
			if !contains(p.Roles, r) {
				out = append(out, "role:"+r)
			}
		}
	}
	return out
}

// markCaseInQA moves a programmed case to its QA status. It reports the
// status the case had and whether it changed anything: nothing changes when
// the program has no such status, or the case is already waiting on another
// draft.
func (s *server) markCaseInQA(r *http.Request, tenant, caseID, template string) (prev string, changed bool) {
	cfg := s.loadProgram(r, tenant)
	want := qaInternalStatus(template)
	if cfg == nil || !contains(cfg.Statuses.Internal, want) {
		return "", false
	}
	if err := s.db.QueryRow(r.Context(), fmt.Sprintf(
		`SELECT coalesce(internal_status,'') FROM tenant_%s.cases WHERE id::text=$1`, sanitizeTenant(tenant)), caseID).Scan(&prev); err != nil {
		return "", false
	}
	if prev == qaStatusInitialReview || prev == qaStatusLetter {
		return "", false
	}
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(
		`UPDATE tenant_%s.cases SET internal_status=$2, updated_at=now() WHERE id::text=$1`, sanitizeTenant(tenant)), caseID, want)
	return prev, true
}

// releaseCaseFromQA puts back the status a case had before its draft went to
// QA. It does nothing if the status has moved on since, or if another draft
// on the same case is still waiting.
func (s *server) releaseCaseFromQA(r *http.Request, tenant, caseID, qid string) {
	var prev *string
	if err := s.db.QueryRow(r.Context(),
		`SELECT prev_internal_status FROM public.qa_reviews WHERE tenant=$1 AND id=$2`, tenant, qid).Scan(&prev); err != nil || prev == nil {
		return
	}
	var stillWaiting bool
	_ = s.db.QueryRow(r.Context(), `
		SELECT EXISTS(SELECT 1 FROM public.qa_reviews
		WHERE tenant=$1 AND case_id=$2 AND id<>$3 AND status='PENDING' AND prev_internal_status IS NOT NULL)`,
		tenant, caseID, qid).Scan(&stillWaiting)
	if stillWaiting {
		// Hand the remembered status to the draft that is still waiting.
		_, _ = s.db.Exec(r.Context(), `
			UPDATE public.qa_reviews SET prev_internal_status=$4
			WHERE tenant=$1 AND case_id=$2 AND id<>$3 AND status='PENDING' AND prev_internal_status IS NOT NULL`,
			tenant, caseID, qid, *prev)
		return
	}
	_, _ = s.db.Exec(r.Context(), fmt.Sprintf(`
		UPDATE tenant_%s.cases SET internal_status=nullif($2,''), updated_at=now()
		WHERE id::text=$1 AND internal_status IN ($3,$4)`, sanitizeTenant(tenant)),
		caseID, *prev, qaStatusInitialReview, qaStatusLetter)
}

// afterLetterSent runs everything that follows a letter really going out,
// whichever path sent it (straight from the compose screen, or on QA
// approval): the dates the SOP ties to that letter, the RFI response clock,
// and the signal the workflow's stage gates wait for.
func (s *server) afterLetterSent(r *http.Request, tenant, caseID, template, rfiTo string) {
	if dateKeys, ok := templateDateKeys[template]; ok {
		today := time.Now().UTC().Format("2006-01-02")
		for _, key := range dateKeys {
			_, _ = s.db.Exec(r.Context(),
				fmt.Sprintf(`UPDATE tenant_%s.cases SET program_dates = program_dates || jsonb_build_object($2::text, $3::text), updated_at=now() WHERE id=$1`,
					sanitizeTenant(tenant)), caseID, key, today)
		}
	}
	var caseNumber string
	if err := s.db.QueryRow(r.Context(),
		fmt.Sprintf(`SELECT case_number FROM tenant_%s.cases WHERE id=$1`, sanitizeTenant(tenant)), caseID).Scan(&caseNumber); err != nil {
		return
	}
	wfID := fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), caseNumber)
	// RFI can go out at stages 3-5: the workflow's RFI watcher runs the
	// 15-day response clock and its day-15 lapse. Best-effort, like every
	// other auto-signal.
	if template == "rfi" {
		if rfiTo != "plan" {
			rfiTo = "provider"
		}
		_ = s.tc.SignalWorkflow(r.Context(), wfID, "", "RFI_SENT", map[string]any{"to": rfiTo})
	}
	_ = s.tc.SignalWorkflow(r.Context(), wfID, "", "LETTER_SENT", map[string]any{"template": template})
}
