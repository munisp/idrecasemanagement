package main

import (
	"errors"
	"fmt"
	"testing"

	"go.temporal.io/api/serviceerror"
)

func TestWorkflowEnded(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{serviceerror.NewNotFound("workflow execution already completed"), true},
		{fmt.Errorf("signal: %w", serviceerror.NewNotFound("sql: no rows in result set")), true},
		{errors.New("workflow execution already completed"), true},
		{serviceerror.NewUnavailable("connection refused"), false},
		{errors.New("context deadline exceeded"), false},
	} {
		if got := workflowEnded(c.err); got != c.want {
			t.Errorf("workflowEnded(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
