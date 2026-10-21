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
	"net/http"
	"strings"
	"sync"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
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
