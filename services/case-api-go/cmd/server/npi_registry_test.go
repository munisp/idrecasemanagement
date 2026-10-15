package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNPINamesMatch(t *testing.T) {
	org := npiRecord{Found: true, Name: "GULF COAST EMERGENCY PHYSICIANS, P.A.", Others: []string{"GCEP TAMPA"}}
	person := npiRecord{Found: true, Name: "MARIA LOPEZ"}
	cases := []struct {
		entered string
		rec     npiRecord
		want    bool
	}{
		{"Gulf Coast Emergency Physicians PA", org, true},
		{"gulf coast emergency physicians, llc", org, true}, // legal form ignored
		{"Gulf Coast Emergency", org, true},                 // all its words, at least two
		{"GCEP Tampa", org, true},                           // a doing-business-as name
		{"Gulf Coast ER", org, false},                       // ER is not in the registry name
		{"Gulf", org, false},                                // one word is not enough
		{"Sunshine Surgical Center", org, false},
		{"Maria Lopez MD", person, true},
		{"Lopez", person, false},
		{"", org, false},
		{"A & B Clinic", npiRecord{Found: true, Name: "A AND B CLINIC"}, true},
	}
	for _, c := range cases {
		if got := npiNamesMatch(c.entered, c.rec); got != c.want {
			t.Errorf("npiNamesMatch(%q, %q) = %v, want %v", c.entered, c.rec.Name, got, c.want)
		}
	}
}

func TestNPPESLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("version") != "2.1" {
			http.Error(w, "version", http.StatusBadRequest)
			return
		}
		switch r.URL.Query().Get("number") {
		case "1234567893":
			w.Write([]byte(`{"result_count":1,"results":[{"enumeration_type":"NPI-2",
				"basic":{"organization_name":"GULF COAST EMERGENCY PHYSICIANS PA"},
				"other_names":[{"organization_name":"GCEP TAMPA"}],
				"addresses":[{"address_purpose":"MAILING","state":"GA"},{"address_purpose":"LOCATION","state":"FL"}],
				"taxonomies":[{"desc":"Clinic/Center","primary":false},{"desc":"Emergency Medicine","primary":true}]}]}`))
		case "1245319599":
			w.Write([]byte(`{"result_count":1,"results":[{"enumeration_type":"NPI-1","basic":{"first_name":"MARIA","last_name":"LOPEZ"}}]}`))
		case "5000000000":
			http.Error(w, "down", http.StatusServiceUnavailable)
		default:
			w.Write([]byte(`{"result_count":0,"results":[]}`))
		}
	}))
	defer srv.Close()
	old := nppesURL
	nppesURL = srv.URL + "/"
	defer func() { nppesURL = old }()
	ctx := context.Background()

	rec, err := nppesLookup(ctx, "1234567893")
	if err != nil || !rec.Found || rec.Name != "GULF COAST EMERGENCY PHYSICIANS PA" || rec.Type != "NPI-2" ||
		rec.State != "FL" || rec.Taxonomy != "Emergency Medicine" || len(rec.Others) != 1 || rec.Others[0] != "GCEP TAMPA" {
		t.Fatalf("organization: %+v, %v", rec, err)
	}
	if rec, err = nppesLookup(ctx, "1245319599"); err != nil || rec.Name != "MARIA LOPEZ" || rec.Type != "NPI-1" {
		t.Fatalf("person: %+v, %v", rec, err)
	}
	if rec, err = nppesLookup(ctx, "1111111112"); err != nil || rec.Found {
		t.Fatalf("not issued or deactivated must read as not found: %+v, %v", rec, err)
	}
	if _, err = nppesLookup(ctx, "5000000000"); err != errNPPESDown {
		t.Fatalf("a registry outage must be an error, got %v", err)
	}
}
