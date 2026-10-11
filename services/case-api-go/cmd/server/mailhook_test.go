package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

func svixSign(key []byte, id, ts string, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func TestVerifySvix(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(key)
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"type":"email.bounced"}`)
	good := svixSign(key, "msg_1", ts, body)
	if !verifySvix(secret, "msg_1", ts, good, body, now) {
		t.Fatal("a correctly signed report must verify")
	}
	if !verifySvix(secret, "msg_1", ts, "v1,AAAA "+good, body, now) {
		t.Fatal("any one valid signature in the header is enough (key rotation)")
	}
	if verifySvix(secret, "msg_1", ts, good, []byte(`{"type":"email.delivered"}`), now) {
		t.Fatal("a changed body must not verify")
	}
	if verifySvix(secret, "msg_2", ts, good, body, now) {
		t.Fatal("a changed message id must not verify")
	}
	if verifySvix("whsec_"+base64.StdEncoding.EncodeToString([]byte("another-key-another-key")), "msg_1", ts, good, body, now) {
		t.Fatal("the wrong secret must not verify")
	}
	if verifySvix(secret, "msg_1", ts, good, body, now.Add(6*time.Minute)) {
		t.Fatal("a report older than five minutes must be refused (replay)")
	}
	if verifySvix(secret, "msg_1", ts, "", body, now) || verifySvix("", "msg_1", ts, good, body, now) {
		t.Fatal("missing signature or secret must not verify")
	}
}

func TestMailEventStatusAndPrecedence(t *testing.T) {
	for ev, want := range map[string]string{"email.bounced": "BOUNCED", "email.delivered": "DELIVERED",
		"email.complained": "COMPLAINED", "email.failed": "FAILED", "email.sent": "", "email.opened": "", "email.clicked": ""} {
		if got := mailEventStatus(ev); got != want {
			t.Errorf("%s: got %q, want %q", ev, got, want)
		}
	}
	cases := []struct {
		cur, next string
		want      bool
	}{
		{"SENT", "DELIVERED", true}, {"SENT", "BOUNCED", true}, {"DELIVERED", "COMPLAINED", true},
		{"DELIVERED", "BOUNCED", true},
		{"BOUNCED", "DELIVERED", false}, {"COMPLAINED", "DELIVERED", false}, {"FAILED", "DELIVERED", false},
		{"BOUNCED", "BOUNCED", false},
		{"PENDING", "BOUNCED", false}, {"REJECTED", "DELIVERED", false}, // never sent: no report applies
	}
	for _, c := range cases {
		if got := mailStatusMayReplace(c.cur, c.next); got != c.want {
			t.Errorf("%s -> %s: got %v, want %v", c.cur, c.next, got, c.want)
		}
	}
	if _, ok := parseProviderTime("2026-10-11T19:22:18.123Z"); !ok {
		t.Error("RFC3339 provider time must parse")
	}
	if _, ok := parseProviderTime("2026-10-11 19:22:18.123456+00"); !ok {
		t.Error("space-separated provider time must parse")
	}
}
