package main

import (
	"errors"
	"testing"
	"time"
)

func TestMailRetryAfter(t *testing.T) {
	cases := []struct {
		msg       string
		transient bool
		wait      time.Duration
	}{
		{"550 You have reached your daily email sending quota.", true, time.Hour},
		{"550 Too many requests. You can only make 10 requests per second.", true, time.Minute},
		{"421 Service not available", true, 2 * time.Minute},
		{"dial tcp 1.2.3.4:465: i/o timeout", true, 2 * time.Minute},
		{"550 5.1.1 The email account that you tried to reach does not exist", false, 0},
		{"553 Sender address rejected", false, 0},
	}
	for _, c := range cases {
		w, tr := mailRetryAfter(errors.New(c.msg))
		if tr != c.transient || w != c.wait {
			t.Errorf("%q -> (%v, %v), want (%v, %v)", c.msg, w, tr, c.wait, c.transient)
		}
	}
	if _, tr := mailRetryAfter(nil); tr {
		t.Error("no error is not a retry")
	}
}

func TestMailQueuedErrorIsRecognised(t *testing.T) {
	var err error = &mailQueuedError{reason: "550 quota"}
	if !errors.Is(err, errMailQueued) {
		t.Error("callers recognise a queued message with errors.Is")
	}
}

func TestCleanAddrs(t *testing.T) {
	got, err := cleanAddrs([]string{" ifegbesan6+plan1@gmail.com. ", "a@b.co,", ""})
	if err != nil || len(got) != 2 || got[0] != "ifegbesan6+plan1@gmail.com" || got[1] != "a@b.co" {
		t.Errorf("got %v %v", got, err)
	}
	for _, bad := range []string{"not-an-email", "x@nodot", "two words@x.com"} {
		if _, err := cleanAddrs([]string{bad}); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
