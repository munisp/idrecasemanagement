// Author: Patrick Munis at newwave
package main

import (
	"crypto/sha256"
	"fmt"
	"testing"

	tb_types "github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

func oldID(s string) tb_types.Uint128 {
	h := sha256.Sum256([]byte(s))
	var b [16]byte
	copy(b[:], h[:16])
	return tb_types.BytesToUint128(b)
}

// With no namespace (idre) every id is what it was before namespacing, so the
// ledger idre already posted to keeps lining up.
func TestLedgerIDsUnchangedWithoutNamespace(t *testing.T) {
	ledgerNamespace = ""
	if acctID("fl", acctEscrowTrustHeld, "p1") != oldID(fmt.Sprintf("%s:%d:%s", "fl", acctEscrowTrustHeld, "p1")) {
		t.Fatal("account id changed")
	}
	if hash16("mojaloop", "inv1", "p1") != oldID("mojaloop:inv1:p1") {
		t.Fatal("hash16 changed")
	}
	if ledgerTransferID("svc:k") != oldID("svc:k") {
		t.Fatal("transfer id changed")
	}
}

// With a namespace (idre-prod) none of the ids collide with idre's.
func TestLedgerIDsSeparatedByNamespace(t *testing.T) {
	defer func() { ledgerNamespace = "" }()
	ledgerNamespace = ""
	a, h, x := acctID("fl", acctEscrowTrustHeld, "p1"), hash16("a", "b"), ledgerTransferID("ap:k")
	ledgerNamespace = "idre-prod"
	if acctID("fl", acctEscrowTrustHeld, "p1") == a || hash16("a", "b") == h || ledgerTransferID("ap:k") == x {
		t.Fatal("namespace did not change the ids")
	}
}
