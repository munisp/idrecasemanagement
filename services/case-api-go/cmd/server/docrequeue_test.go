// Author: Patrick Munis at newwave
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequeueDecision(t *testing.T) {
	m, h := time.Minute, time.Hour
	cases := []struct {
		name              string
		attempts          int
		unread, sinceLast time.Duration
		readerActive      bool
		want              string
	}{
		{"just uploaded", 0, 2 * m, 2 * m, false, "wait"},
		{"still inside the grace period", 0, 29 * m, 29 * m, false, "wait"},
		{"unread past the grace period, reader idle: the event was lost", 0, 31 * m, 31 * m, false, "requeue"},
		{"unread but the reader is busy: it is behind other documents", 0, 90 * m, 90 * m, true, "wait"},
		{"reader busy, but this one has waited longer than any queue should take", 0, 4 * h, 4 * h, true, "requeue"},
		{"announced again a moment ago: give it time", 1, 2 * h, 5 * m, false, "wait"},
		{"announced again half an hour ago and still unread", 1, 2 * h, 31 * m, false, "requeue"},
		{"third try", 3, 3 * h, 31 * m, false, "requeue"},
		{"four tries and nothing: tell people", 4, 4 * h, 31 * m, false, "stall"},
		{"four tries, last one just now: wait before giving up", 4, 4 * h, 5 * m, false, "wait"},
		{"a document from last week that was never read", 0, 7 * 24 * h, 7 * 24 * h, false, "requeue"},
	}
	for _, c := range cases {
		if got := requeueDecision(c.attempts, c.unread, c.sinceLast, c.readerActive); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The bus refusing an event used to pass unnoticed. It is an error now, and
// a refusal that clears on a later try is not.
func TestPublishCheckedSeesARefusalAndRetries(t *testing.T) {
	var calls int32
	fail := int32(2)
	bus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= atomic.LoadInt32(&fail) {
			http.Error(w, "pubsub component not found", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer bus.Close()
	s := &server{cfg: Config{DaprHTTP: bus.URL, TopicPrefix: "idretest"}}
	evt := func() map[string]any { return map[string]any{"type": "doc.uploaded", "doc_id": "d-1"} }

	if err := s.publishChecked(context.Background(), "fl", "documents", evt()); err != nil {
		t.Fatalf("two refusals then success should succeed: %v", err)
	}
	if calls != 3 {
		t.Errorf("want 3 attempts, got %d", calls)
	}
	atomic.StoreInt32(&calls, 0)
	atomic.StoreInt32(&fail, 99)
	err := s.publishChecked(context.Background(), "fl", "documents", evt())
	if err == nil {
		t.Fatal("a bus that refuses every time must be reported")
	}
	if calls != 3 {
		t.Errorf("want exactly 3 attempts before giving up, got %d", calls)
	}
	// A bus that is not there at all is an error too.
	gone := &server{cfg: Config{DaprHTTP: "http://127.0.0.1:1", TopicPrefix: "idretest"}}
	if gone.publishChecked(context.Background(), "fl", "documents", evt()) == nil {
		t.Fatal("an unreachable bus must be reported")
	}
}
