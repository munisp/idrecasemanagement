package main

// verifications.go — the checks beyond the NPI registry (npi_registry.go) on
// a TPA's client or a health plan's party, each one able to say "not
// available" without getting in anyone's way:
//
//   EXCLUSION  the HHS-OIG List of Excluded Individuals/Entities (LEIE):
//              people and businesses barred from federal health programs.
//              Downloaded (free, no key) and refreshed weekly into
//              public.oig_exclusions. An NPI match, or the same business or
//              person name, holds the client for staff review; filing waits
//              until staff clear it (not the excluded party) or confirm it.
//              No list loaded yet: UNAVAILABLE, never blocking, re-checked
//              once the list loads (and after every refresh).
//   TIN        the number's form: an EIN prefix the IRS issues, or a valid
//              Social Security number pattern for a sole provider. A TIN
//              failing both is refused at entry. Matching it against IRS
//              records needs IRS TIN Matching or a paid vendor; until one is
//              set up the check reads NOT_CONFIGURED and blocks nothing.
//
// One row per (subject, check) in public.verifications.
//
//   GET  /onboarding/verification-reviews              staff: exclusion-list matches
//   POST /onboarding/verification-reviews/{id}         staff: {decision: CLEAR|EXCLUDED, note}

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

var (
	oigListURL      = envOr("OIG_LEIE_URL", "https://oig.hhs.gov/exclusions/downloadables/UPDATED.csv")
	oigHTTP         = &http.Client{Timeout: 3 * time.Minute}
	oigRefreshEvery = 7 * 24 * time.Hour
	oigRetryEvery   = time.Hour // after a failed download
	oigLastTry      time.Time
)

// --- TIN / EIN form -------------------------------------------------------------

// IRS campus prefixes that are never issued for an EIN.
var einBadPrefix = map[string]bool{"00": true, "07": true, "08": true, "09": true, "17": true, "18": true, "19": true,
	"28": true, "29": true, "49": true, "69": true, "70": true, "78": true, "79": true, "89": true, "96": true, "97": true}

func einFormOK(t string) bool { return isDigits(t, 9) && !einBadPrefix[t[:2]] }

// ssnFormOK: a sole provider may bill under a Social Security number.
func ssnFormOK(t string) bool {
	if !isDigits(t, 9) {
		return false
	}
	area := t[:3]
	return area != "000" && area != "666" && area[0] != '9' && t[3:5] != "00" && t[5:] != "0000"
}

// normTIN is a TIN as its 9 digits: 12-3456789 and 123 45 6789 are written either way.
func normTIN(t string) string {
	return strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(t))
}

var (
	einWritten = regexp.MustCompile(`^\d{2}-\d{7}$`)             // 12-3456789: an EIN
	ssnWritten = regexp.MustCompile(`^\d{3}[- ]\d{2}[- ]\d{4}$`) // 123-45-6789: a Social Security number
)

// tinCheck is a TIN as taken: its 9 digits, and why it cannot be real ("" when it can). How it is written says
// which kind it is: 12-3456789 must be a possible EIN, 123-45-6789 a possible SSN; bare digits may be either.
func tinCheck(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	t := normTIN(raw)
	if msg := tinProblem(t); msg != "" {
		return t, msg
	}
	switch {
	case einWritten.MatchString(raw) && !einFormOK(t):
		return t, "TIN " + raw + " is written as an EIN, but no EIN starts with " + t[:2]
	case ssnWritten.MatchString(raw) && !ssnFormOK(t):
		return t, "TIN " + raw + " is written as a Social Security number, but it isn't a valid one"
	}
	return t, ""
}

// tinProblem: why this TIN (9 digits, see normTIN) cannot be real, or "". The one rule for every TIN taken.
func tinProblem(t string) string {
	switch {
	case !isDigits(t, 9):
		return "TIN must be exactly 9 digits"
	case strings.Count(t, t[:1]) == 9:
		return "TIN " + t + " is not a real number (all the same digit)"
	case !einFormOK(t) && !ssnFormOK(t):
		return "TIN " + t + " cannot be an IRS-issued number: its first two digits are never used for an EIN, and it isn't a valid Social Security number either"
	}
	return ""
}

// feinProblem: a health plan's FEIN is always an EIN.
func feinProblem(t string) string {
	switch {
	case !isDigits(t, 9):
		return "FEIN must be exactly 9 digits"
	case !einFormOK(t) || strings.Count(t, t[:1]) == 9:
		return "FEIN " + t + " cannot be an IRS-issued EIN: check the number on the plan's IRS letter"
	}
	return ""
}

// --- the exclusion list -------------------------------------------------------------

type oigRow struct{ last, first, mid, bus, general, specialty, npi, state, excltype, excldate string }

// parseLEIE reads the OIG download (header row, then one row per exclusion).
func parseLEIE(r io.Reader) ([]oigRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	head, err := cr.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	for _, need := range []string{"LASTNAME", "FIRSTNAME", "BUSNAME", "NPI", "STATE", "EXCLTYPE", "EXCLDATE"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("the exclusion list has no %s column", need)
		}
	}
	get := func(rec []string, k string) string {
		if i, ok := col[k]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var out []oigRow
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		npi := get(rec, "NPI")
		if strings.Trim(npi, "0") == "" {
			npi = ""
		}
		out = append(out, oigRow{get(rec, "LASTNAME"), get(rec, "FIRSTNAME"), get(rec, "MIDNAME"), get(rec, "BUSNAME"),
			get(rec, "GENERAL"), get(rec, "SPECIALTY"), npi, get(rec, "STATE"), get(rec, "EXCLTYPE"), get(rec, "EXCLDATE")})
	}
	if len(out) < 1000 { // the real list holds tens of thousands
		return nil, fmt.Errorf("the exclusion list download looks incomplete (%d rows)", len(out))
	}
	return out, nil
}

// oigNameKey: the comparable form of a business or person name.
func oigNameKey(name string) string {
	t := npiNameTokens(name)
	if len(t) == 0 {
		return ""
	}
	words := make([]string, 0, len(t))
	for w := range t {
		words = append(words, w)
	}
	// order-free: "SMITH JOHN" and "JOHN SMITH" are the same key
	for i := 1; i < len(words); i++ {
		for j := i; j > 0 && words[j] < words[j-1]; j-- {
			words[j], words[j-1] = words[j-1], words[j]
		}
	}
	return strings.Join(words, " ")
}

// oigLoaded: when the list was last loaded, and how many rows it holds.
func (s *server) oigLoaded(ctx context.Context) (time.Time, int) {
	var at time.Time
	var n int
	_ = s.db.QueryRow(ctx, `SELECT loaded_at, row_count FROM public.oig_meta WHERE id=1`).Scan(&at, &n)
	return at, n
}

// refreshOIGList downloads the list when it is missing or a week old. Only
// one replica does it (advisory lock); a failed download is retried hourly
// and leaves the old list in place.
func (s *server) refreshOIGList(ctx context.Context) {
	at, n := s.oigLoaded(ctx)
	if n > 0 && time.Since(at) < oigRefreshEvery {
		return
	}
	if time.Since(oigLastTry) < oigRetryEvery {
		return
	}
	oigLastTry = time.Now()
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	var got bool
	if conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('oig_leie_refresh'))`).Scan(&got) != nil || !got {
		return
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('oig_leie_refresh'))`)

	dctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(dctx, http.MethodGet, oigListURL, nil)
	resp, err := oigHTTP.Do(req)
	if err != nil {
		log.Printf("oig: download failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("oig: download failed: HTTP %d", resp.StatusCode)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		log.Printf("oig: download cut off: %v", err)
		return
	}
	rows, err := parseLEIE(bytes.NewReader(body))
	if err != nil {
		log.Printf("oig: %v", err)
		return
	}
	tx, err := conn.Begin(dctx)
	if err != nil {
		return
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(dctx, `TRUNCATE public.oig_exclusions`); err != nil {
		return
	}
	src := make([][]any, 0, len(rows))
	for _, x := range rows {
		person := strings.TrimSpace(x.first + " " + x.last)
		key := oigNameKey(x.bus)
		if key == "" {
			key = oigNameKey(person)
		}
		src = append(src, []any{x.last, x.first, x.mid, x.bus, x.general, x.specialty, x.npi, x.state, x.excltype, x.excldate, key})
	}
	if _, err := tx.CopyFrom(dctx, pgx.Identifier{"public", "oig_exclusions"},
		[]string{"last_name", "first_name", "mid_name", "business_name", "general", "specialty", "npi", "state", "excl_type", "excl_date", "name_key"},
		pgx.CopyFromRows(src)); err != nil {
		log.Printf("oig: load failed: %v", err)
		return
	}
	if _, err := tx.Exec(dctx, `
		INSERT INTO public.oig_meta (id, loaded_at, row_count, source_modified) VALUES (1, now(), $1, $2)
		ON CONFLICT (id) DO UPDATE SET loaded_at=now(), row_count=excluded.row_count, source_modified=excluded.source_modified`,
		len(rows), resp.Header.Get("Last-Modified")); err != nil {
		return
	}
	if tx.Commit(dctx) == nil {
		log.Printf("oig: exclusion list loaded, %d rows", len(rows))
	}
}

type oigMatch struct {
	Name      string `json:"name"`
	NPI       string `json:"npi,omitempty"`
	General   string `json:"general,omitempty"`
	Specialty string `json:"specialty,omitempty"`
	State     string `json:"state,omitempty"`
	Type      string `json:"exclusion_type"`
	Date      string `json:"excluded_since"`
	By        string `json:"matched_on"` // npi | name
}

var errOIGUnavailable = errors.New("the exclusion list is not loaded yet")

// oigCheck: the exclusion-list entries this NPI or name matches.
func (s *server) oigCheck(ctx context.Context, npi, name string) ([]oigMatch, error) {
	if _, n := s.oigLoaded(ctx); n == 0 {
		return nil, errOIGUnavailable
	}
	key := oigNameKey(name)
	rows, err := s.db.Query(ctx, `
		SELECT coalesce(nullif(business_name,''), trim(first_name||' '||last_name)), npi, general, specialty, state, excl_type, excl_date,
		       CASE WHEN $1<>'' AND npi=$1 THEN 'npi' ELSE 'name' END
		FROM public.oig_exclusions
		WHERE ($1<>'' AND npi=$1) OR ($2<>'' AND name_key=$2 AND position(' ' in $2) > 0)
		LIMIT 20`, npi, key)
	if err != nil {
		return nil, errOIGUnavailable
	}
	defer rows.Close()
	var out []oigMatch
	for rows.Next() {
		var m oigMatch
		if rows.Scan(&m.Name, &m.NPI, &m.General, &m.Specialty, &m.State, &m.Type, &m.Date, &m.By) == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// --- running the checks on a client or party --------------------------------------

// runVerifications records the exclusion-list and TIN checks for a TPA
// client (kind "tpa") or a plan party (kind "hp"). A check a person already
// decided keeps its decision unless what it was decided on changed.
func (s *server) runVerifications(ctx context.Context, kind, tenant, id, npi, name, tin string) {
	subject := npi + "|" + strings.ToUpper(strings.TrimSpace(name))
	var prevStatus, prevSubject string
	_ = s.db.QueryRow(ctx, `SELECT status, subject_key FROM public.verifications WHERE tenant=$1 AND kind=$2 AND subject_id=$3 AND check_name='EXCLUSION'`,
		tenant, kind, id).Scan(&prevStatus, &prevSubject)
	if !(prevSubject == subject && (prevStatus == "CLEARED" || prevStatus == "EXCLUDED")) {
		status, detail := "CLEAR", map[string]any{}
		if m, err := s.oigCheck(ctx, npi, name); err != nil {
			status, detail = "UNAVAILABLE", map[string]any{"reason": err.Error()}
		} else if len(m) > 0 {
			status, detail = "NEEDS_REVIEW", map[string]any{"matches": m}
		}
		s.saveVerification(ctx, kind, tenant, id, "EXCLUSION", status, subject, detail)
		if status == "NEEDS_REVIEW" && prevStatus != "NEEDS_REVIEW" {
			s.logAudit(ctx, tenant, "", "EXCLUSION_LIST_MATCH", map[string]any{"kind": kind, "id": id, "by": "system"})
		}
	}
	if tin != "" {
		s.saveVerification(ctx, kind, tenant, id, "TIN", "NOT_CONFIGURED", tin,
			map[string]any{"form": "ok", "reason": "the TIN's form is checked; matching it against IRS records needs IRS TIN Matching or a vendor, not set up"})
	}
}

func (s *server) saveVerification(ctx context.Context, kind, tenant, id, check, status, subject string, detail map[string]any) {
	d, _ := json.Marshal(detail)
	_, _ = s.db.Exec(ctx, `
		INSERT INTO public.verifications (tenant, kind, subject_id, check_name, status, subject_key, detail, checked_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,now())
		ON CONFLICT (tenant, kind, subject_id, check_name) DO UPDATE SET status=excluded.status, subject_key=excluded.subject_key,
		  detail=excluded.detail, checked_at=now(), reviewed_by=NULL, review_note=NULL`,
		tenant, kind, id, check, status, subject, d)
}

// verificationBlock: why a client or party cannot file because of a check, or "".
func (s *server) verificationBlock(ctx context.Context, kind, tenant, id string) string {
	var st, note string
	_ = s.db.QueryRow(ctx, `SELECT status, coalesce(review_note,'') FROM public.verifications
		WHERE tenant=$1 AND kind=$2 AND subject_id=$3 AND check_name='EXCLUSION'`, tenant, kind, id).Scan(&st, &note)
	noun := npiOwners[kind].noun
	switch st {
	case "NEEDS_REVIEW":
		return "the " + noun + " matches an entry on the federal exclusion list (HHS-OIG) and is waiting for the review team to check it"
	case "EXCLUDED":
		if note != "" {
			return "the review team confirmed the " + noun + " is on the federal exclusion list (HHS-OIG): " + note
		}
		return "the review team confirmed the " + noun + " is on the federal exclusion list (HHS-OIG)"
	}
	return ""
}

// exclusionOnboardingStatus: the onboarding status an exclusion-list check
// holds a client or party at, or "".
func (s *server) exclusionOnboardingStatus(ctx context.Context, kind, tenant, id string) string {
	var st string
	_ = s.db.QueryRow(ctx, `SELECT status FROM public.verifications WHERE tenant=$1 AND kind=$2 AND subject_id=$3 AND check_name='EXCLUSION'`,
		tenant, kind, id).Scan(&st)
	return map[string]string{"NEEDS_REVIEW": "EXCLUSION_REVIEW", "EXCLUDED": "EXCLUDED"}[st]
}

var verifyRunning atomic.Bool

// startVerificationSweep runs recheckVerifications off the filer's loop: the
// weekly list download can take a minute and must not hold up filing.
func (s *server) startVerificationSweep() {
	if !verifyRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer verifyRunning.Store(false)
		s.recheckVerifications(context.Background())
	}()
}

// recheckVerifications: once the list loads, check what couldn't be checked;
// after each refresh, check the clear ones again. A small batch per run.
func (s *server) recheckVerifications(ctx context.Context) {
	s.refreshOIGList(ctx)
	at, n := s.oigLoaded(ctx)
	if n == 0 {
		return
	}
	type item struct{ kind, tenant, id string }
	var todo []item
	rows, err := s.db.Query(ctx, `
		SELECT kind, tenant, subject_id FROM public.verifications
		WHERE check_name='EXCLUSION' AND (status='UNAVAILABLE' OR (status='CLEAR' AND checked_at < $1))
		ORDER BY checked_at LIMIT 25`, at)
	if err != nil {
		return
	}
	for rows.Next() {
		var x item
		if rows.Scan(&x.kind, &x.tenant, &x.id) == nil {
			todo = append(todo, x)
		}
	}
	rows.Close()
	for _, x := range todo {
		o := npiOwners[x.kind]
		var npi, name, tin string
		if s.db.QueryRow(ctx, `SELECT coalesce(npi,''), party_name, coalesce(tin,'') FROM public.`+o.table+` WHERE tenant=$1 AND id::text=$2`,
			x.tenant, x.id).Scan(&npi, &name, &tin) != nil {
			continue
		}
		s.runVerifications(ctx, x.kind, x.tenant, x.id, npi, name, tin)
		s.recomputeNPIOwner(ctx, x.kind, x.tenant, x.id)
	}
}

// verificationsSummary: check name -> status for the client lists.
func (s *server) verificationsSummary(ctx context.Context, kind, tenant string) map[string]map[string]any {
	out := map[string]map[string]any{}
	rows, err := s.db.Query(ctx, `SELECT subject_id, check_name, status, coalesce(review_note,'') FROM public.verifications WHERE tenant=$1 AND kind=$2`, tenant, kind)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, check, st, note string
		if rows.Scan(&id, &check, &st, &note) == nil {
			if out[id] == nil {
				out[id] = map[string]any{}
			}
			out[id][check] = map[string]any{"status": st, "note": note}
		}
	}
	return out
}

// --- staff review -------------------------------------------------------------------

func (s *server) verificationReviews(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !hasAnyRole(r.Context().Value(ctxPrincipal{}).(principal), onboardingReviewRoles...) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	out := []map[string]any{}
	for _, kind := range []string{"tpa", "hp"} {
		o := npiOwners[kind]
		rows, err := s.db.Query(r.Context(), `
			SELECT v.id::text, c.id::text, t.name, c.party_name, coalesce(c.npi,''), v.detail, v.checked_at
			FROM public.verifications v JOIN public.`+o.table+` c ON c.tenant=v.tenant AND c.id::text=v.subject_id
			JOIN public.`+o.orgTable+` t ON t.id=c.`+o.orgCol+`
			WHERE v.tenant=$1 AND v.kind=$2 AND v.check_name='EXCLUSION' AND v.status='NEEDS_REVIEW'
			ORDER BY v.checked_at`, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var vid, id, org, party, npi string
			var detail []byte
			var checked time.Time
			if rows.Scan(&vid, &id, &org, &party, &npi, &detail, &checked) != nil {
				continue
			}
			var d map[string]any
			_ = json.Unmarshal(detail, &d)
			out = append(out, map[string]any{"review_id": vid, "kind": kind, "id": id, "organization": org, "party_name": party,
				"npi": npi, "matches": d["matches"], "checked_at": checked})
		}
		rows.Close()
	}
	at, n := s.oigLoaded(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"reviews": out, "list_loaded_at": at, "list_rows": n})
}

func (s *server) verificationReviewDecide(w http.ResponseWriter, r *http.Request) {
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
	status := map[string]string{"CLEAR": "CLEARED", "EXCLUDED": "EXCLUDED"}[strings.ToUpper(in.Decision)]
	switch {
	case status == "":
		http.Error(w, `{"error":"decision must be CLEAR (not the excluded party) or EXCLUDED"}`, http.StatusBadRequest)
		return
	case in.Note == "":
		http.Error(w, `{"error":"say how you decided (for example: different date of birth, different address, confirmed with the provider)"}`, http.StatusBadRequest)
		return
	case len(in.Note) > 500:
		http.Error(w, `{"error":"the note is too long (500 characters at most)"}`, http.StatusBadRequest)
		return
	}
	var kind, id string
	err := s.db.QueryRow(r.Context(), `
		UPDATE public.verifications SET status=$3, reviewed_by=$4, review_note=$5, checked_at=now()
		WHERE tenant=$1 AND id::text=$2 AND check_name='EXCLUSION' AND status='NEEDS_REVIEW'
		RETURNING kind, subject_id`, tenant, chi.URLParam(r, "reviewId"), status, p.Subject, in.Note).Scan(&kind, &id)
	if err != nil {
		http.Error(w, `{"error":"no exclusion-list match waiting for review here"}`, http.StatusNotFound)
		return
	}
	st := s.recomputeNPIOwner(r.Context(), kind, tenant, id)
	s.logAudit(r.Context(), tenant, "", "EXCLUSION_LIST_REVIEWED", map[string]any{"kind": kind, "id": id, "decision": status, "note": in.Note, "by": p.Subject})
	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "id": id, "status": status, "onboarding_status": st})
}
