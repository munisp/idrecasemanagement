package main

// org_prefill.go — File a dispute, filled in from the dispute's own documents.
//
// GET /tpa/prefill?package_id=… and /hp/prefill?package_id=…: once a TPA or a health plan has added the dispute's
// documents (its document set), what those documents state of the filing form's fields -- the amount in dispute,
// the other side (name, notices email, NAIC or NPI), the provider type and network status, and for a TPA the client
// whose NPI they carry. Read by doc-intel when each document was checked (package_fields, kept in the set's
// fields); nothing here reads another organization's documents or any case. Each value says which document it
// came from, so the filer sees why before filing; the filer always submits.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

type orgPrefillValue struct {
	Value      string `json:"value"`
	Confidence string `json:"confidence"` // high | medium
	From       string `json:"from"`       // the document it was read from
	Why        string `json:"why"`
}

// orgPrefillDoc: one document of a set and what it states.
type orgPrefillDoc struct {
	Slot, Filename string
	Fields         map[string]any
}

// slotRank: which document to believe first for a field -- the plan's own papers for the plan, the bill for what
// was billed.
var slotRank = map[string]map[string]int{
	"payer":  {"EOB": 0, "PROOF_OF_COVERAGE": 1, "OPEN_NEGOTIATION_NOTICE": 2, "MORE_DENIALS_EOBS": 3},
	"billed": {"ITEMIZED_BILL": 0, "MORE_CLAIM_FORMS": 1, "EOB": 2},
	"paid":   {"EOB": 0, "MORE_DENIALS_EOBS": 1},
}

func rankOf(kind, slot string) int {
	if k := moreDocKind(slot); k != "" {
		slot = "MORE_" + k
	}
	if r, ok := slotRank[kind][slot]; ok {
		return r
	}
	return 50
}

func fieldString(m map[string]any, k string) string {
	if v, ok := m[k]; ok && v != nil {
		switch x := v.(type) {
		case string:
			return strings.TrimSpace(x)
		case float64:
			return fmt.Sprintf("%.0f", x)
		case bool:
			return fmt.Sprint(x)
		}
	}
	return ""
}

// orgPrefillFrom: the suggestions a set's documents make, by field. side: the filer's (tpa: the provider's).
// Pure, so it is tested.
func orgPrefillFrom(docs []orgPrefillDoc, kind string) map[string]orgPrefillValue {
	out := map[string]orgPrefillValue{}
	label := func(d orgPrefillDoc) string {
		if d.Filename != "" {
			return d.Filename
		}
		return strings.ReplaceAll(strings.ToLower(d.Slot), "_", " ")
	}
	best := func(field, rank string, pick func(orgPrefillDoc) string) (string, orgPrefillDoc, bool) {
		cand := []orgPrefillDoc{}
		for _, d := range docs {
			if pick(d) != "" {
				cand = append(cand, d)
			}
		}
		if len(cand) == 0 {
			return "", orgPrefillDoc{}, false
		}
		sort.SliceStable(cand, func(i, j int) bool { return rankOf(rank, cand[i].Slot) < rankOf(rank, cand[j].Slot) })
		return pick(cand[0]), cand[0], true
	}
	agree := func(pick func(orgPrefillDoc) string, v string) string {
		n := 0
		for _, d := range docs {
			if pick(d) == v {
				n++
			}
		}
		if n > 1 {
			return "high"
		}
		return "medium"
	}
	money := func(c string) string {
		var cents int64
		_, _ = fmt.Sscan(c, &cents)
		return fmt.Sprintf("%d.%02d", cents/100, cents%100)
	}
	if kind == "tpa" {
		// The other side is the health plan.
		for _, f := range []struct{ key, field, why string }{
			{"payer_name", "other_party_name", "the health plan named on"},
			{"payer_email", "other_party_email", "the plan's claims/disputes email on"},
			{"payer_naic", "other_party_naic", "the plan's NAIC on"},
		} {
			pick := func(d orgPrefillDoc) string { return fieldString(d.Fields, f.key) }
			if v, d, ok := best(f.field, "payer", pick); ok {
				out[f.field] = orgPrefillValue{v, agree(pick, v), label(d), f.why + " " + label(d)}
			}
		}
		pick := func(d orgPrefillDoc) string { return fieldString(d.Fields, "provider_type") }
		if v, d, ok := best("provider_type", "billed", pick); ok {
			why := fieldString(d.Fields, "provider_type_basis")
			if why == "" {
				why = "the provider type " + label(d) + " states"
			}
			out["provider_type"] = orgPrefillValue{v, agree(pick, v), label(d), why}
		}
		pick = func(d orgPrefillDoc) string {
			switch fieldString(d.Fields, "contracted") {
			case "true":
				return "yes"
			case "false":
				return "no"
			}
			return ""
		}
		if v, d, ok := best("in_network", "payer", pick); ok {
			out["in_network"] = orgPrefillValue{v, agree(pick, v), label(d), "the network status " + label(d) + " states"}
		}
	} else {
		// The other side is the provider.
		pick := func(d orgPrefillDoc) string { return fieldString(d.Fields, "provider_name") }
		if v, d, ok := best("other_party_name", "billed", pick); ok {
			out["other_party_name"] = orgPrefillValue{v, agree(pick, v), label(d), "the provider named on " + label(d)}
		}
		pick = func(d orgPrefillDoc) string { return fieldString(d.Fields, "provider_npi") }
		if v, d, ok := best("other_party_npi", "billed", pick); ok {
			out["other_party_npi"] = orgPrefillValue{v, agree(pick, v), label(d), "the provider's NPI on " + label(d)}
		}
	}
	// The amount in dispute: as a document states it, else billed less paid.
	pick := func(d orgPrefillDoc) string { return fieldString(d.Fields, "disputed_cents") }
	if v, d, ok := best("amount", "billed", pick); ok {
		out["amount"] = orgPrefillValue{money(v), agree(pick, v), label(d), "the amount in dispute " + label(d) + " states"}
	} else {
		pb := func(d orgPrefillDoc) string { return fieldString(d.Fields, "billed_cents") }
		pp := func(d orgPrefillDoc) string { return fieldString(d.Fields, "paid_cents") }
		b, db, okb := best("billed", "billed", pb)
		p, dp, okp := best("paid", "paid", pp)
		var bc, pc int64
		_, _ = fmt.Sscan(b, &bc)
		_, _ = fmt.Sscan(p, &pc)
		switch {
		case okb && okp && bc > pc:
			out["amount"] = orgPrefillValue{money(fmt.Sprint(bc - pc)), "medium", label(db),
				fmt.Sprintf("billed $%s on %s, less $%s paid on %s", money(b), label(db), money(p), label(dp))}
		case okb && !okp:
			out["amount"] = orgPrefillValue{money(b), "medium", label(db), "billed on " + label(db) + " (no payment found in the documents)"}
		}
	}
	return out
}

func (s *server) orgPrefill(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Context().Value(ctxTenant{}).(string)
		py, err := s.payerFor(r, tenant, kind)
		if err != nil {
			http.Error(w, `{"error":"not linked to an organization"}`, http.StatusNotFound)
			return
		}
		pkg := r.URL.Query().Get("package_id")
		if py.clientID != "" && !s.tpaResourceInClient(r.Context(), tenant, py.clientID, "package", pkg) {
			http.Error(w, `{"error":"no such document set of yours"}`, http.StatusNotFound)
			return
		}
		prefix, owner := "tpa", "tpa_id"
		if kind == "hp" {
			prefix, owner = "hp", "plan_id"
		}
		var ok bool
		_ = s.db.QueryRow(r.Context(), fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM public.%s_packages WHERE tenant=$1 AND id::text=$2 AND %s::text=$3)`, prefix, owner),
			tenant, pkg, py.id).Scan(&ok)
		if !ok {
			http.Error(w, `{"error":"no such document set of yours"}`, http.StatusNotFound)
			return
		}
		rows, err := s.db.Query(r.Context(), fmt.Sprintf(`SELECT slot, coalesce(filename,''), coalesce(fields,'{}'::jsonb)::text
			FROM public.%s_package_docs WHERE tenant=$1 AND package_id::text=$2`, prefix), tenant, pkg)
		if err != nil {
			http.Error(w, `{"error":"db"}`, http.StatusInternalServerError)
			return
		}
		var docs []orgPrefillDoc
		reading := 0
		for rows.Next() {
			var d orgPrefillDoc
			var raw string
			if rows.Scan(&d.Slot, &d.Filename, &raw) == nil {
				_ = json.Unmarshal([]byte(raw), &d.Fields)
				if d.Fields == nil {
					reading++
				}
				docs = append(docs, d)
			}
		}
		rows.Close()
		fields := orgPrefillFrom(docs, kind)
		// A TPA's client: the one whose NPI the documents carry.
		if kind == "tpa" {
			npis := map[string]string{}
			for _, d := range docs {
				if n := fieldString(d.Fields, "provider_npi"); n != "" {
					npis[n] = d.Filename
				}
			}
			for npi, from := range npis {
				var id, name string
				if s.db.QueryRow(r.Context(), `SELECT id::text, party_name FROM public.tpa_clients WHERE tenant=$1 AND tpa_id::text=$2 AND npi=$3 AND status='ACTIVE' LIMIT 1`,
					tenant, py.id, npi).Scan(&id, &name) == nil {
					fields["client_id"] = orgPrefillValue{id, "high", from, fmt.Sprintf("NPI %s on %s is %s", npi, from, name)}
					break
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"fields": fields, "documents": len(docs)})
	}
}
