package main

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// The grouped chain must be exactly the chain the old one-entry-at-a-time writer made
// (scripts/verify-audit-chain.py recomputes it).
func TestChainHashesMatchOneAtATime(t *testing.T) {
	bodies := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}
	prev, want := "abc123", []string{}
	for _, b := range bodies {
		sum := sha256.Sum256([]byte(prev + b))
		prev = hex.EncodeToString(sum[:])
		want = append(want, prev)
	}
	got := chainHashes("abc123", bodies)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: got %s want %s", i, got[i], want[i])
		}
	}
	if len(chainHashes("GENESIS", nil)) != 0 {
		t.Error("no entries, no hashes")
	}
}
