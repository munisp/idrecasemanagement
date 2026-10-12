package main

import "testing"

func TestTheReviewerHasNoPaymentScreens(t *testing.T) {
	for _, who := range [][]string{{"CASE_MANAGER"}, {"ARBITRATOR"}, {"CODER"}, {"TPA"}} {
		if hasAnyRole(principal{Roles: who}, paymentReadRoles...) {
			t.Errorf("%v must not read payment accounts, files or screening", who)
		}
	}
	for _, who := range [][]string{{"PM"}, {"FINANCE"}, {"TENANT_ADMIN"}, {"FEDERAL_ADMIN"}} {
		if !hasAnyRole(principal{Roles: who}, paymentReadRoles...) {
			t.Errorf("%v reads the Payments screen", who)
		}
	}
}
