package main

import "testing"

// Every rule of the boundary, without a database: what a client's login, a Finance login and both may reach.
func TestTPAScopedDecision(t *testing.T) {
	const c, id = "c1", "11111111-1111-1111-1111-111111111111"
	client := tpaLogin{TPAID: "t", ClientID: c, Role: "STAFF"}
	clientAdmin := tpaLogin{TPAID: "t", ClientID: c, Role: "ADMIN"}
	finance := tpaLogin{TPAID: "t", Role: "FINANCE"}
	clientFinance := tpaLogin{TPAID: "t", ClientID: c, Role: "FINANCE"}
	for _, x := range []struct {
		name         string
		l            tpaLogin
		method, path string
		allowed      bool
		check        string
	}{
		{"shell", client, "GET", "/notifications", true, ""},
		{"client list narrowed by handler", client, "GET", "/tpa/clients", true, ""},
		{"own dispute checked", client, "GET", "/cases/" + id, true, "case"},
		{"case documents checked", client, "GET", "/cases/" + id + "/documents", true, "case"},
		{"case timeline stays closed", client, "GET", "/cases/" + id + "/timeline", false, ""},
		{"client path checked", client, "PUT", "/tpa/clients/" + id + "/profile", true, "client"},
		{"package checked", client, "POST", "/tpa/packages/" + id + "/docs", true, "package"},
		{"request checked", client, "POST", "/tpa/document-requests/" + id + "/submit", true, "request"},
		{"pay checked", client, "POST", "/tpa/disputes/" + id + "/pay", true, "case"},
		{"adding a document checked", client, "POST", "/tpa/disputes/" + id + "/documents", true, "case"},
		{"finance cannot add a document", finance, "POST", "/tpa/disputes/" + id + "/documents", false, ""},
		{"bulk filing closed", client, "POST", "/tpa/disputes/bulk", false, ""},
		{"bulk report closed", client, "GET", "/tpa/disputes/bulk/" + id, false, ""},
		{"old bulk closed", client, "POST", "/intake/bulk", false, ""},
		{"claim closed", client, "POST", "/tpa/claim", false, ""},
		{"invite code closed", clientAdmin, "POST", "/tpa/invite-code", false, ""},
		{"add client closed", clientAdmin, "POST", "/tpa/clients", false, ""},
		{"import closed", clientAdmin, "POST", "/tpa/clients/import", false, ""},
		{"status closed", clientAdmin, "POST", "/tpa/clients/" + id + "/status", false, ""},
		{"groups write closed", clientAdmin, "POST", "/tpa/groups", false, ""},
		{"senders closed", clientAdmin, "GET", "/tpa/authorized-senders", false, ""},
		{"tasks closed", client, "POST", "/tpa/tasks", false, ""},
		{"own team open", clientAdmin, "POST", "/tpa/team", true, ""},
		{"unknown route closed", client, "GET", "/tpa/something-new", false, ""},
		{"finance page", finance, "GET", "/tpa/finance", true, ""},
		{"finance pays", finance, "POST", "/tpa/disputes/" + id + "/pay", true, ""},
		{"finance bulk payment", finance, "POST", "/tpa/disputes/bulk/" + id + "/checkout", true, ""},
		{"finance cannot file", finance, "POST", "/tpa/intake", false, ""},
		{"finance cannot bulk file", finance, "POST", "/tpa/disputes/bulk", false, ""},
		{"finance cannot edit a dispute", finance, "PATCH", "/tpa/disputes/" + id, false, ""},
		{"finance cannot change a profile", finance, "PUT", "/tpa/clients/" + id + "/profile", false, ""},
		{"finance cannot see the team", finance, "GET", "/tpa/team", false, ""},
		{"finance cannot upload", finance, "POST", "/tpa/packages/" + id + "/docs", false, ""},
		{"client finance checked", clientFinance, "POST", "/tpa/clients/" + id + "/fees/checkout", true, "client"},
		{"client finance no batches", clientFinance, "GET", "/tpa/disputes/bulk", false, ""},
		{"client finance cannot file", clientFinance, "POST", "/tpa/intake", false, ""},
		{"wrong method closed", client, "DELETE", "/tpa/clients", false, ""},
	} {
		ok, check, _ := tpaScopedDecision(x.l, x.method, x.path)
		if ok != x.allowed || check != x.check {
			t.Errorf("%s: %s %s = %v %q; want %v %q", x.name, x.method, x.path, ok, check, x.allowed, x.check)
		}
	}
	if (tpaLogin{Role: "STAFF"}).restricted() || (tpaLogin{Role: "ADMIN"}).restricted() {
		t.Error("whole-TPA admins and staff must not be restricted")
	}
}

func TestTPAFinanceSummary(t *testing.T) {
	rows := []map[string]any{
		{"client_id": "a", "client_name": "A", "amount_cents": int64(100), "status": "OPEN", "due_date": "2026-01-01", "payment_status": "FAILED"},
		{"client_id": "a", "client_name": "A", "amount_cents": int64(50), "status": "PAID", "due_date": "2026-01-01", "payment_status": "PAID"},
		{"client_id": "b", "client_name": "B", "amount_cents": int64(30), "status": "OPEN", "due_date": "2026-12-01", "payment_status": "PENDING", "clearing": true},
		{"client_id": "b", "client_name": "B", "amount_cents": int64(20), "status": "OPEN", "due_date": "2026-12-01", "payment_status": "PENDING", "clearing": false},
	}
	s := tpaFinanceSummary(rows, "2026-06-01")
	if s["outstanding_cents"] != int64(150) || s["overdue_cents"] != int64(100) || s["paid_cents"] != int64(50) || s["failed"] != 1 || s["pending"] != 1 {
		t.Errorf("summary wrong: %v", s)
	}
}

func TestCSVKeepsFormulasOut(t *testing.T) {
	if csvNoFormula("=HYPERLINK(1)") != "'=HYPERLINK(1)" || csvNoFormula("Clinic") != "Clinic" || csvNoFormula("") != "" {
		t.Error("csvNoFormula")
	}
}
