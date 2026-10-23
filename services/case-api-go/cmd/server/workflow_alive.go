// workflow_alive.go — whether a case's workflow is still running.
//
// A case's steps move by signals to its Temporal workflow. A workflow that has ended -- finished after the case
// closed, or stopped by an admin -- cannot take a signal, and the signal used to come back as a bare 502 ("signal
// failed") with the step buttons still offered. The steps endpoint now says whether the workflow is running, so the
// case page offers no step it cannot move, and a signal to an ended workflow is answered with what happened.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	temporalclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

// caseWorkflowID is the id the case's workflow was started under: AHCA-<TENANT>-<case number> on a programmed
// tenant, IDR-<TENANT>-<case number> on a federal one.
func (s *server) caseWorkflowID(r *http.Request, tenant, caseNumber string) string {
	if s.loadProgram(r, tenant) != nil {
		return fmt.Sprintf("AHCA-%s-%s", strings.ToUpper(tenant), caseNumber)
	}
	return fmt.Sprintf("IDR-%s-%s", strings.ToUpper(tenant), caseNumber)
}

// workflowEnded: the error Temporal gives for a signal (or query) to a workflow that is no longer running, or that
// never existed.
func workflowEnded(err error) bool {
	var nf *serviceerror.NotFound
	if errors.As(err, &nf) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "workflow execution already completed") || strings.Contains(msg, "workflow not found")
}

const endedWorkflowMsg = "This case's workflow has ended (it finished, or an admin stopped it), so its steps can't be moved any more. " +
	"Reopen the case, or ask an admin."

// The case page re-reads its steps every few seconds; a workflow's state is cached briefly so that is not a Temporal
// call each time.
var (
	wfAliveMu    sync.Mutex
	wfAliveCache = map[string]wfAlive{}
)

type wfAlive struct {
	running bool
	at      time.Time
}

// workflowRunning: (running, known). known is false when Temporal could not be asked (unreachable, no client):
// the page then behaves as before rather than hiding the steps on a guess.
func (s *server) workflowRunning(ctx context.Context, wfID string) (bool, bool) {
	wfAliveMu.Lock()
	if c, ok := wfAliveCache[wfID]; ok && time.Since(c.at) < 20*time.Second {
		wfAliveMu.Unlock()
		return c.running, true
	}
	wfAliveMu.Unlock()
	if s.tc == nil {
		return false, false
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	d, err := s.tc.DescribeWorkflowExecution(cctx, wfID, "")
	running := false
	switch {
	case err == nil:
		running = d.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
	case workflowEnded(err):
		running = false
	default:
		return false, false
	}
	wfAliveMu.Lock()
	if len(wfAliveCache) > 5000 {
		wfAliveCache = map[string]wfAlive{}
	}
	wfAliveCache[wfID] = wfAlive{running: running, at: time.Now()}
	wfAliveMu.Unlock()
	return running, true
}

// forgetWorkflowRunning drops a cached state (after a signal was refused, so the next read asks again).
func forgetWorkflowRunning(wfID string) {
	wfAliveMu.Lock()
	delete(wfAliveCache, wfID)
	wfAliveMu.Unlock()
}

// ---- Self-healing: an open case always has a workflow to take its steps -------------------------------------------
//
// A step used to stop at "This case's workflow has ended" whenever the case's workflow was not running, though the case
// itself was still open: a workflow stopped by hand, one that failed, one moved between namespaces, or a case whose
// workflow never started. signalCaseWorkflow delivers a step to the case's AHCA workflow and, when that workflow has
// ended while the case is open, starts it again from the signals it had received (the workflow's resume input:
// activities are skipped until the first step the case is really at) and then delivers the step. A workflow that
// finished its stages normally takes no further signals, and its remaining steps are records, so they are recorded
// without it. Only a closed case refuses a step.

// wfStatusResume: for a case that never had a workflow, the signals its internal status says already happened.
var wfStatusResume = func() map[string][]map[string]any {
	sig := func(name string, data map[string]any) map[string]any {
		if data == nil {
			data = map[string]any{}
		}
		return map[string]any{"name": name, "data": data}
	}
	packet := []map[string]any{sig("PACKET_COMPLETE", map[string]any{"estimate_requested": false})}
	eligible := append(append([]map[string]any{}, packet...), sig("ELIGIBILITY_RESULT", map[string]any{"result": "ELIGIBLE"}))
	accepted := append(append([]map[string]any{}, eligible...), sig("LETTER_SENT", map[string]any{"template": "acceptance"}))
	notified := append(append([]map[string]any{}, accepted...), sig("LETTER_SENT", map[string]any{"template": "plan_notification"}))
	review := append(append([]map[string]any{}, notified...), sig("PLAN_RESPONSE_RECEIVED", nil))
	reviewed := append(append([]map[string]any{}, review...), sig("CODING_REVIEW_COMPLETE", map[string]any{"clinical_requested": false}),
		sig("ATTORNEY_REVIEW_COMPLETE", nil))
	sentFL := append(append([]map[string]any{}, reviewed...), sig("LETTER_SENT", map[string]any{"template": "final_order"}))
	return map[string][]map[string]any{
		"": {}, "Initial Review Pending": {}, "QA Initial Review": packet,
		"Provider Acceptance Letter Issued": accepted, "Plan Notification Packet Issued": notified,
		"Review In Progress": review, "Plan - No Response": review,
		"RFI Pending Provider Response": append(append([]map[string]any{}, review...), sig("RFI_SENT", map[string]any{"to": "provider"})),
		"RFI Pending Plan Response":     append(append([]map[string]any{}, review...), sig("RFI_SENT", map[string]any{"to": "plan"})),
		"QA Final Determination":        reviewed, "Determination Sent to FL": sentFL, "Determination sent to FL": sentFL,
	}
}()

// signalCaseWorkflow sends signal to the case's workflow. recorded=true: the workflow had finished its stages, so the
// step is recorded without it (the caller carries on with its records). err is the original "ended" error only when
// the case is closed or the workflow could not be started again.
func (s *server) signalCaseWorkflow(ctx context.Context, tenant, caseID, wfID, signal string, data any) (recorded bool, err error) {
	if s.tc == nil {
		return false, fmt.Errorf("workflow engine not configured")
	}
	err = s.tc.SignalWorkflow(ctx, wfID, "", signal, data)
	if err == nil || !workflowEnded(err) || !strings.HasPrefix(wfID, "AHCA-") {
		return false, err
	}
	var caseNumber, status, internal string
	if qerr := s.db.QueryRow(ctx, fmt.Sprintf(`SELECT case_number, coalesce(status,''), coalesce(internal_status,'') FROM tenant_%s.cases WHERE id::text=$1`,
		sanitizeTenant(tenant)), caseID).Scan(&caseNumber, &status, &internal); qerr != nil {
		return false, err
	}
	if strings.HasPrefix(status, "CLOSED") || wfClosedStatuses[internal] {
		return false, err // a closed case: the message is right
	}
	d, derr := s.tc.DescribeWorkflowExecution(ctx, wfID, "")
	if derr == nil && d.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED {
		return true, nil
	}
	if derr != nil {
		if _, known := wfStatusResume[internal]; !known {
			// No run to resume from and no stage this status maps to (a final order already issued, say): there is
			// nothing left for a workflow to wait on, so the step is recorded rather than refused.
			return true, nil
		}
	}
	if rerr := s.resumeCaseWorkflow(ctx, tenant, caseID, caseNumber, internal, wfID, derr == nil); rerr != nil {
		slog.Error("workflow resume failed", "workflow", wfID, "err", rerr)
		return false, err
	}
	return false, s.tc.SignalWorkflow(ctx, wfID, "", signal, data)
}

// resumeCaseWorkflow starts the case's workflow again: from the signals its last run received (and any that run was
// itself resumed with), or -- for a case that never had one -- from its internal status. It waits for the new run to
// finish catching up, so the step that follows is acted on, not skipped as already past.
func (s *server) resumeCaseWorkflow(ctx context.Context, tenant, caseID, caseNumber, internal, wfID string, hadRun bool) error {
	signals := []map[string]any{}
	source := "its internal status"
	if hadRun {
		source = "its previous run's signals"
		it := s.tc.GetWorkflowHistory(ctx, wfID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		dc := converter.GetDefaultDataConverter()
		for it.HasNext() {
			ev, err := it.Next()
			if err != nil {
				return err
			}
			if a := ev.GetWorkflowExecutionStartedEventAttributes(); a != nil {
				var in map[string]any
				if p := a.GetInput().GetPayloads(); len(p) > 0 && dc.FromPayload(p[0], &in) == nil {
					if res, ok := in["resume"].(map[string]any); ok {
						if old, ok := res["signals"].([]any); ok {
							for _, o := range old {
								if m, ok := o.(map[string]any); ok {
									signals = append(signals, m)
								}
							}
						}
					}
				}
			}
			if a := ev.GetWorkflowExecutionSignaledEventAttributes(); a != nil {
				var data any = map[string]any{}
				if p := a.GetInput().GetPayloads(); len(p) > 0 {
					_ = dc.FromPayload(p[0], &data)
				}
				signals = append(signals, map[string]any{"name": a.GetSignalName(), "data": data})
			}
		}
	} else {
		st, ok := wfStatusResume[internal]
		if !ok {
			return fmt.Errorf("no resume known for internal status %q", internal)
		}
		signals = st
	}
	input := map[string]any{"tenant": tenant, "case_id": caseID, "case_number": caseNumber, "resume": map[string]any{"signals": signals}}
	if _, err := s.tc.ExecuteWorkflow(ctx, temporalclient.StartWorkflowOptions{ID: wfID, TaskQueue: "idre-ahca"}, "AhcaDisputeWorkflow", input); err != nil {
		return err
	}
	forgetWorkflowRunning(wfID)
	// Catching up happens in the run's first workflow task; a step signalled before that would look already done.
	for i := 0; i < 25; i++ {
		time.Sleep(200 * time.Millisecond)
		d, err := s.tc.DescribeWorkflowExecution(ctx, wfID, "")
		if err == nil && d.GetPendingWorkflowTask() == nil && d.GetWorkflowExecutionInfo().GetHistoryLength() > 3 {
			break
		}
	}
	s.logActivity(ctx, tenant, caseID, "WORKFLOW_RESUMED",
		fmt.Sprintf("The case's workflow had stopped while the case was open; it was started again from %s (%d signals) so the case can move on", source, len(signals)))
	return nil
}

// sigCase is signalCaseWorkflow for callers that only need to know whether the step went through.
func (s *server) sigCase(ctx context.Context, tenant, caseID, wfID, signal string, data any) error {
	_, err := s.signalCaseWorkflow(ctx, tenant, caseID, wfID, signal, data)
	return err
}
