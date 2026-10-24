package main

// npi_registry.go — an NPI is checked against the national registry (NPPES,
// run by CMS), not only for its check digit: a TPA client's, a health plan
// party's (networks and facilities), and the provider a plan files against.
//
//   not in NPPES (never issued, or deactivated)  refused at entry
//   found, and the name matches                   VERIFIED
//   found under another name                      NEEDS_REVIEW: saved, but the
//                                                 client cannot file until staff
//                                                 confirm or reject it
//   NPPES unreachable                             UNVERIFIED: saved and usable;
//                                                 re-checked in the background
//
// Every answer is kept in public.npi_cache. A VERIFIED NPI is checked again
// every 30 days, so one deactivated later lands in NEEDS_REVIEW.
//
// The provider a plan files against has no review: an NPI not in NPPES is
// refused, and any name is accepted (the filer names the other side).
//
//   GET  /onboarding/npi-reviews                     staff: NPIs waiting for a person
//   POST /onboarding/npi-reviews/{kind}/{id}         staff: {decision: CONFIRM|REJECT, note}
//                                                    kind: tpa (client) | hp (party)
//
// NPI_TEST_NUMBERS (dev only, comma-separated) lists made-up NPIs the test
// suites use; they pass without a lookup. Never set it in production.
//
// Only the NPI is sent to NPPES: it is public data, and nothing about a case
// leaves the platform.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var (
	nppesURL       = envOr("NPPES_URL", "https://npiregistry.cms.hhs.gov/api/")
	nppesHTTP      = &http.Client{Timeout: 6 * time.Second}
	npiCacheFresh  = 24 * time.Hour      // a save reuses an answer this recent
	npiRecheckAge  = 30 * 24 * time.Hour // a VERIFIED NPI is checked again after this
	errNPPESDown   = errors.New("the NPI registry (NPPES) could not be reached")
	npiNameTokenRe = regexp.MustCompile(`[A-Z0-9]+`)
	npiTestNumbers = func() map[string]bool {
		m := map[string]bool{}
		for _, n := range strings.Split(envOr("NPI_TEST_NUMBERS", ""), ",") {
			if n = strings.TrimSpace(n); n != "" {
				m[n] = true
			}
		}
		return m
	}()
)

// npiRecord is what NPPES holds for one NPI.
type npiRecord struct {
	Found    bool     `json:"found"`
	Name     string   `json:"name"`             // organization name, or "FIRST LAST"
	Others   []string `json:"other_names"`      // former and doing-business-as names
	Type     string   `json:"enumeration_type"` // NPI-1 person, NPI-2 organization
	Taxonomy string   `json:"taxonomy"`
	State    string   `json:"state"`
}

// nppesLookup asks the registry. A deactivated NPI is not returned, so it
// reads as not found.
func nppesLookup(ctx context.Context, npi string) (npiRecord, error) {
	q := url.Values{"version": {"2.1"}, "number": {npi}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nppesURL+"?"+q.Encode(), nil)
	if err != nil {
		return npiRecord{}, err
	}
	resp, err := nppesHTTP.Do(req)
	if err != nil {
		return npiRecord{}, errNPPESDown
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return npiRecord{}, errNPPESDown
	}
	var body struct {
		ResultCount int `json:"result_count"`
		Errors      []struct {
			Description string `json:"description"`
		} `json:"Errors"`
		Results []struct {
			EnumerationType string `json:"enumeration_type"`
			Basic           struct {
				OrganizationName string `json:"organization_name"`
				FirstName        string `json:"first_name"`
				LastName         string `json:"last_name"`
			} `json:"basic"`
			OtherNames []struct {
				OrganizationName string `json:"organization_name"`
				FirstName        string `json:"first_name"`
				LastName         string `json:"last_name"`
			} `json:"other_names"`
			Addresses []struct {
				Purpose string `json:"address_purpose"`
				State   string `json:"state"`
			} `json:"addresses"`
			Taxonomies []struct {
				Desc    string `json:"desc"`
				Primary bool   `json:"primary"`
			} `json:"taxonomies"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return npiRecord{}, errNPPESDown
	}
	if len(body.Errors) > 0 || body.ResultCount == 0 || len(body.Results) == 0 {
		return npiRecord{Found: false}, nil
	}
	x := body.Results[0]
	rec := npiRecord{Found: true, Type: x.EnumerationType}
	person := func(first, last string) string { return strings.TrimSpace(first + " " + last) }
	if x.Basic.OrganizationName != "" {
		rec.Name = x.Basic.OrganizationName
	} else {
		rec.Name = person(x.Basic.FirstName, x.Basic.LastName)
	}
	for _, o := range x.OtherNames {
		if n := o.OrganizationName; n != "" {
			rec.Others = append(rec.Others, n)
		} else if n := person(o.FirstName, o.LastName); n != "" {
			rec.Others = append(rec.Others, n)
		}
	}
	for _, a := range x.Addresses {
		if a.Purpose == "LOCATION" || rec.State == "" {
			rec.State = a.State
		}
	}
	for _, t := range x.Taxonomies {
		if t.Primary || rec.Taxonomy == "" {
			rec.Taxonomy = t.Desc
		}
	}
	return rec, nil
}

// npiLookup answers from the cache when it is fresh enough, otherwise asks
// NPPES and keeps the answer. When NPPES is down, a cached answer of any age
// is used; without one the error is returned.
func (s *server) npiLookup(ctx context.Context, npi string, maxAge time.Duration) (npiRecord, error) {
	var rec npiRecord
	var valid bool
	var others []byte
	var checked time.Time
	cached := s.db.QueryRow(ctx, `
		SELECT valid, coalesce(registry_name,''), coalesce(other_names,'[]'::jsonb), coalesce(enumeration_type,''),
		       coalesce(taxonomy,''), coalesce(state,''), checked_at
		FROM public.npi_cache WHERE npi=$1`, npi).
		Scan(&valid, &rec.Name, &others, &rec.Type, &rec.Taxonomy, &rec.State, &checked) == nil
	if cached {
		rec.Found = valid
		_ = json.Unmarshal(others, &rec.Others)
		if time.Since(checked) < maxAge {
			return rec, nil
		}
	}
	live, err := nppesLookup(ctx, npi)
	if err != nil {
		if cached {
			return rec, nil
		}
		return npiRecord{}, err
	}
	o, _ := json.Marshal(live.Others)
	_, _ = s.db.Exec(ctx, `
		INSERT INTO public.npi_cache (npi, valid, source, checked_at, registry_name, other_names, enumeration_type, taxonomy, state)
		VALUES ($1,$2,'live',now(),$3,$4,$5,$6,$7)
		ON CONFLICT (npi) DO UPDATE SET valid=excluded.valid, source='live', checked_at=now(),
		  registry_name=excluded.registry_name, other_names=excluded.other_names,
		  enumeration_type=excluded.enumeration_type, taxonomy=excluded.taxonomy, state=excluded.state`,
		npi, live.Found, live.Name, o, live.Type, live.Taxonomy, live.State)
	return live, nil
}

// Words that say what kind of entity a name is, not which one.
var npiNameNoise = map[string]bool{
	"THE": true, "OF": true, "AND": true, "DBA": true, "A": true,
	"LLC": true, "L": true, "C": true, "INC": true, "INCORPORATED": true, "CORP": true, "CORPORATION": true,
	"CO": true, "COMPANY": true, "LTD": true, "LIMITED": true, "LP": true, "LLP": true, "PLLC": true,
	"PA": true, "PC": true, "P": true, "MD": true, "DO": true, "PLC": true,
}

func npiNameTokens(name string) map[string]bool {
	out := map[string]bool{}
	for _, t := range npiNameTokenRe.FindAllString(strings.ToUpper(strings.ReplaceAll(name, "&", " AND ")), -1) {
		if !npiNameNoise[t] {
			out[t] = true
		}
	}
	return out
}

// npiNamesMatch: the same words once legal-form words are dropped, or one
// name's words (at least two of them) all within the other's. "Gulf Coast
// Emergency Physicians, P.A." matches "GULF COAST EMERGENCY PHYSICIANS PA"
// and "Gulf Coast Emergency"; "Gulf Coast ER" does not, and goes to a person.
func npiNamesMatch(entered string, rec npiRecord) bool {
	a := npiNameTokens(entered)
	if len(a) == 0 {
		return false
	}
	for _, n := range append([]string{rec.Name}, rec.Others...) {
		b := npiNameTokens(n)
		if len(b) == 0 {
			continue
		}
		small, big := a, b
		if len(small) > len(big) {
			small, big = big, small
		}
		all := true
		for t := range small {
			if !big[t] {
				all = false
				break
			}
		}
		if all && (len(small) == len(big) || len(small) >= 2) {
			return true
		}
	}
	return false
}

// npiCheck decides a client's NPI. refuse is set when it must not be saved.
type npiCheck struct {
	Status   string // VERIFIED | NEEDS_REVIEW | UNVERIFIED
	Registry string // the name NPPES holds
	Refuse   string
}

func (s *server) checkClientNPI(ctx context.Context, npi, partyName string, maxAge time.Duration) npiCheck {
	if npiTestNumbers[npi] {
		return npiCheck{Status: "VERIFIED", Registry: "(test NPI)"}
	}
	rec, err := s.npiLookup(ctx, npi, maxAge)
	switch {
	case err != nil:
		return npiCheck{Status: "UNVERIFIED"}
	case !rec.Found:
		return npiCheck{Refuse: "NPI " + npi + " is not in the national NPI registry (NPPES): it was never issued or has been deactivated. Check the number with the provider."}
	case npiNamesMatch(partyName, rec):
		return npiCheck{Status: "VERIFIED", Registry: rec.Name}
	default:
		return npiCheck{Status: "NEEDS_REVIEW", Registry: rec.Name}
	}
}

// npiOwner is a table whose rows carry an NPI decision.
type npiOwner struct {
	table, orgTable, orgCol, noun string
}

var npiOwners = map[string]npiOwner{
	"tpa": {"tpa_clients", "tpas", "tpa_id", "client"},
	"hp":  {"hp_parties", "health_plans", "plan_id", "party"},
}

// saveNPICheck records the outcome on a client (tpa) or party (hp).
func (s *server) saveNPICheck(ctx context.Context, kind, tenant, id string, c npiCheck) {
	o := npiOwners[kind]
	_, _ = s.db.Exec(ctx, `
		UPDATE public.`+o.table+` SET npi_status=$3, npi_registry_name=NULLIF($4,''), npi_checked_at=now(),
		  npi_reviewed_by=NULL, npi_review_note=NULL
		WHERE tenant=$1 AND id::text=$2`, tenant, id, c.Status, c.Registry)
}

func (s *server) saveClientNPICheck(ctx context.Context, tenant, clientID string, c npiCheck) {
	s.saveNPICheck(ctx, "tpa", tenant, clientID, c)
}

// npiBlock: why this client (tpa) or party (hp) cannot file because of its NPI, or "".
func (s *server) npiBlock(ctx context.Context, kind, tenant, id string) string {
	o := npiOwners[kind]
	var st, note string
	_ = s.db.QueryRow(ctx, `SELECT coalesce(npi_status,''), coalesce(npi_review_note,'') FROM public.`+o.table+` WHERE tenant=$1 AND id::text=$2`,
		tenant, id).Scan(&st, &note)
	switch st {
	case "NEEDS_REVIEW":
		return "the " + o.noun + "'s NPI is registered under another name and is waiting for the review team to confirm it"
	case "REJECTED":
		if note != "" {
			return "the review team rejected the " + o.noun + "'s NPI: " + note + " — enter the correct NPI in the " + o.noun + "'s profile"
		}
		return "the review team rejected the " + o.noun + "'s NPI — enter the correct NPI in the " + o.noun + "'s profile"
	}
	return s.verificationBlock(ctx, kind, tenant, id)
}

func (s *server) clientNPIBlock(ctx context.Context, tenant, clientID string) string {
	return s.npiBlock(ctx, "tpa", tenant, clientID)
}

func (s *server) recomputeNPIOwner(ctx context.Context, kind, tenant, id string) string {
	if kind == "hp" {
		return s.recomputeHpPartyStatusCtx(ctx, tenant, id)
	}
	return s.recomputeClientStatusCtx(ctx, tenant, id)
}

// respondentNPIProblem: the provider a plan files against must hold a real
// NPI. An outage lets it through; a name that differs is accepted.
func (s *server) respondentNPIProblem(ctx context.Context, npi string) string {
	if npi == "" || npiTestNumbers[npi] {
		return ""
	}
	if rec, err := s.npiLookup(ctx, npi, npiCacheFresh); err == nil && !rec.Found {
		return "the provider's NPI " + npi + " is not in the national NPI registry (NPPES): it was never issued or has been deactivated"
	}
	return ""
}

// recheckClientNPIs: UNVERIFIED NPIs once NPPES answers again, and VERIFIED
// ones every 30 days, for clients and parties alike. A small batch per run,
// to stay gentle with NPPES.
func (s *server) recheckClientNPIs(ctx context.Context) {
	age := fmt.Sprintf("%d seconds", int(npiRecheckAge.Seconds()))
	type item struct{ kind, tenant, id, npi, name, status string }
	var todo []item
	for _, kind := range []string{"tpa", "hp"} {
		o := npiOwners[kind]
		rows, err := s.db.Query(ctx, `
			SELECT tenant, id::text, npi, party_name, coalesce(npi_status,'') FROM public.`+o.table+`
			WHERE coalesce(npi,'') <> '' AND (
			  npi_status='UNVERIFIED' OR
			  (npi_status='VERIFIED' AND npi_reviewed_by IS NULL AND npi_checked_at < now() - $1::interval))
			ORDER BY npi_checked_at NULLS FIRST LIMIT 10`, age)
		if err != nil {
			continue
		}
		for rows.Next() {
			x := item{kind: kind}
			if rows.Scan(&x.tenant, &x.id, &x.npi, &x.name, &x.status) == nil {
				todo = append(todo, x)
			}
		}
		rows.Close()
	}
	for _, x := range todo {
		o := npiOwners[x.kind]
		c := s.checkClientNPI(ctx, x.npi, x.name, 0)
		if c.Status == "UNVERIFIED" {
			if x.status == "VERIFIED" { // NPPES down: try again next time, keep the date moving
				_, _ = s.db.Exec(ctx, `UPDATE public.`+o.table+` SET npi_checked_at=now() WHERE tenant=$1 AND id::text=$2`, x.tenant, x.id)
			}
			return // NPPES is down for everyone; stop this run
		}
		if c.Refuse != "" { // deactivated (or never issued) since it was saved
			c = npiCheck{Status: "NEEDS_REVIEW"}
		}
		s.saveNPICheck(ctx, x.kind, x.tenant, x.id, c)
		if c.Status == "NEEDS_REVIEW" {
			s.logAudit(ctx, x.tenant, "", "NPI_FLAGGED", map[string]any{"kind": x.kind, "id": x.id, "by": "system", "registry_name": c.Registry})
		}
		if x.status != c.Status {
			s.recomputeNPIOwner(ctx, x.kind, x.tenant, x.id)
		}
	}
}

// --- staff review ---------------------------------------------------------------

func (s *server) npiReviews(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !hasAnyRole(r.Context().Value(ctxPrincipal{}).(principal), onboardingReviewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	out := []map[string]any{}
	for _, kind := range []string{"tpa", "hp"} {
		o := npiOwners[kind]
		rows, err := s.db.Query(r.Context(), `
			SELECT c.id::text, t.name, c.party_name, c.npi, coalesce(c.npi_registry_name,''),
			       coalesce(k.enumeration_type,''), coalesce(k.taxonomy,''), coalesce(k.state,''),
			       coalesce(k.other_names,'[]'::jsonb), c.npi_checked_at
			FROM public.`+o.table+` c JOIN public.`+o.orgTable+` t ON t.id=c.`+o.orgCol+`
			LEFT JOIN public.npi_cache k ON k.npi=c.npi
			WHERE c.tenant=$1 AND c.npi_status='NEEDS_REVIEW'
			ORDER BY c.npi_checked_at`, tenant)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var id, org, party, npi, reg, typ, tax, state string
			var others []byte
			var checked *time.Time
			if rows.Scan(&id, &org, &party, &npi, &reg, &typ, &tax, &state, &others, &checked) != nil {
				continue
			}
			var on []string
			_ = json.Unmarshal(others, &on)
			out = append(out, map[string]any{"kind": kind, "id": id, "client_id": id, "organization": org, "party_name": party, "npi": npi,
				"registry_name": reg, "registry_found": reg != "", "enumeration_type": typ, "taxonomy": tax, "state": state,
				"other_names": on, "checked_at": checked})
		}
		rows.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": out})
}

func (s *server) npiReviewDecide(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, onboardingReviewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	var in struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&in) != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	status := map[string]string{"CONFIRM": "VERIFIED", "REJECT": "REJECTED"}[strings.ToUpper(in.Decision)]
	switch {
	case status == "":
		http.Error(w, `{"error":"decision must be CONFIRM or REJECT"}`, http.StatusBadRequest)
		return
	case status == "REJECTED" && in.Note == "":
		http.Error(w, `{"error":"say why the NPI is rejected — the TPA sees this note"}`, http.StatusBadRequest)
		return
	case len(in.Note) > 500:
		http.Error(w, `{"error":"the note is too long (500 characters at most)"}`, http.StatusBadRequest)
		return
	}
	kind := chi.URLParam(r, "kind")
	o, ok := npiOwners[kind]
	if !ok {
		http.Error(w, `{"error":"kind must be tpa or hp"}`, http.StatusNotFound)
		return
	}
	id := chi.URLParam(r, "id")
	tag, err := s.db.Exec(r.Context(), `
		UPDATE public.`+o.table+` SET npi_status=$3, npi_reviewed_by=$4, npi_review_note=NULLIF($5,''), npi_checked_at=now()
		WHERE tenant=$1 AND id::text=$2 AND npi_status='NEEDS_REVIEW'`, tenant, id, status, p.Subject, in.Note)
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, `{"error":"no NPI waiting for review on this `+o.noun+`"}`, http.StatusNotFound)
		return
	}
	st := s.recomputeNPIOwner(r.Context(), kind, tenant, id)
	action := map[string]string{"tpa": "TPA_CLIENT_NPI_REVIEWED", "hp": "HP_PARTY_NPI_REVIEWED"}[kind]
	s.logAudit(r.Context(), tenant, "", action, map[string]any{"kind": kind, "id": id, "client_id": id, "decision": in.Decision, "note": in.Note, "by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "id": id, "client_id": id, "npi_status": status, "onboarding_status": st})
}
