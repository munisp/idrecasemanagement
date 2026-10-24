package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Sample ZIPs for "File many disputes at once" (tpa_bulk_disputes.go): a ready-made disputes.zip a TPA downloads,
// opens and copies the shape of -- disputes.csv (pre-filled with its own ready clients when it has any) plus one
// folder per dispute, either with the six documents as separate PDFs or with one merged.pdf. Every sample PDF says
// what the real document must show, and carries sampleDocMarker so the upload refuses a sample filed by mistake.

const sampleDocMarker = "IDRE-SAMPLE-DOCUMENT"

type sampleDoc struct {
	file, title string
	lines       []string
}

var sampleDocs = []sampleDoc{
	{"eob.pdf", "Explanation of Benefits (EOB)", []string{
		"The health plan's EOB for the claim in dispute.",
		"It must show: claim number, date(s) of service, billed amount,",
		"allowed / paid amount, denial or adjustment codes, and the plan's name.",
		"The EOB date is the final determination date the 12-month filing window runs from."}},
	{"itemized_bill.pdf", "Itemized bill", []string{
		"The provider's itemized bill (UB-04 or CMS-1500, or an itemized statement).",
		"It must show every service line in dispute: CPT/HCPCS code, date of service,",
		"units and billed charge, with the provider's NPI."}},
	{"proof_of_coverage.pdf", "Proof of coverage", []string{
		"Evidence the patient was covered by the health plan on the date of service:",
		"a member ID card (front and back), an eligibility response, or a plan letter."}},
	{"open_negotiation_notice.pdf", "Open negotiation notice", []string{
		"The notice that started open negotiation with the health plan, and the date it was sent.",
		"Attach the plan's reply, if any."}},
	{"offer_substantiation.pdf", "Offer substantiation", []string{
		"What supports the amount you are asking for: contracts, prior payments for the same",
		"service, market data, or the provider's written rationale."}},
	{"idre_attestation.pdf", "Attestation", []string{
		"The signed attestation that the dispute is eligible and the information is accurate,",
		"on the program's attestation form, signed and dated by the provider or the TPA."}},
}

// simplePDF builds a small, valid PDF (one page per entry, Helvetica text). No dependencies: the documents are only
// words, and the xref offsets are computed exactly.
func simplePDF(pages [][]string) []byte {
	esc := func(s string) string {
		return strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(s)
	}
	var objs []string
	n := len(pages)
	// 1 catalog, 2 pages, 3 font, then per page: page object, content stream
	kids := []string{}
	for i := 0; i < n; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", 4+2*i))
	}
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>")
	objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, lines := range pages {
		var cs bytes.Buffer
		cs.WriteString("BT /F1 11 Tf 15 TL 56 780 Td\n")
		for j, l := range lines {
			if j == 0 {
				cs.WriteString("/F1 16 Tf (" + esc(l) + ") Tj T* T* /F1 11 Tf\n")
				continue
			}
			cs.WriteString("(" + esc(l) + ") Tj T*\n")
		}
		cs.WriteString("ET\n")
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 842] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i))
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", cs.Len(), cs.String()))
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func samplePage(d sampleDoc, ref, party string) []string {
	out := []string{"SAMPLE - " + d.title, "Dispute " + ref + "  -  " + party, ""}
	out = append(out, d.lines...)
	return append(out, "",
		"This is a sample showing what goes in this file. Replace it with the real document",
		"before uploading: a ZIP that still holds sample documents is refused.",
		sampleDocMarker)
}

// tpaBulkSample: GET /tpa/disputes/bulk/sample.zip?layout=separate|merged
func (s *server) tpaBulkSample(w http.ResponseWriter, r *http.Request) {
	tenant := r.Context().Value(ctxTenant{}).(string)
	if !s.featureGate(w, r, tenant, "tpa_onboarding") {
		return
	}
	p := r.Context().Value(ctxPrincipal{}).(principal)
	if !hasAnyRole(p, "TPA", "PM", "TENANT_ADMIN", "FEDERAL_ADMIN", "PLATFORM_ADMIN") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	merged := r.URL.Query().Get("layout") == "merged"
	// Pre-fill with the TPA's own ready clients, so party_name already matches its roster; staff get example names.
	// Each with its provider group, so provider_group shows how a provider is placed.
	parties, groups := []string{}, []string{}
	if org, err := s.tpaForPrincipal(r, tenant); err == nil {
		// Ready to file, the same rule as the disputes.csv template: active, profile and payment done, no hold.
		rows, err := s.db.Query(r.Context(), `SELECT c.party_name, coalesce(g.name,'') FROM public.tpa_clients c
			LEFT JOIN public.tpa_provider_groups g ON g.id=c.group_id
			WHERE c.tenant=$1 AND c.tpa_id=$2 AND c.status='ACTIVE' AND c.onboarding_status IN ('READY','DOCS_PENDING')
			ORDER BY (c.onboarding_status='READY') DESC, c.party_name LIMIT 2`, tenant, org.ID)
		if err == nil {
			for rows.Next() {
				var n, g string
				if rows.Scan(&n, &g) == nil {
					parties, groups = append(parties, n), append(groups, g)
				}
			}
			rows.Close()
		}
	}
	for i, ex := range []string{"Gulf Coast Clinic", "Bay Imaging Center"} {
		if len(parties) < 2 {
			parties, groups = append(parties, ex), append(groups, []string{"Gulf Coast Medical Group", ""}[i])
		}
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	put := func(name string, data []byte) {
		f, _ := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
		_, _ = f.Write(data)
	}
	var csvBuf bytes.Buffer
	cw := csv.NewWriter(&csvBuf)
	_ = cw.Write([]string{"dispute_ref", "provider_group", "party_name", "disputed_amount", "other_party_name", "other_party_email", "other_party_naic", "contact_name", "notes"})
	examples := [][]string{
		{"Sunshine Health", "disputes@sunshine-health.example", "12345", "Dr Lee", "ER visit 08/14 - replace with your dispute"},
		{"Bay Health Plan", "claims@bayhealth.example", "", "", "MRI 07/02 - replace with your dispute"},
	}
	amounts := []string{"1980.00", "2450.00"}
	for i, party := range parties {
		ref := fmt.Sprintf("CLAIM-%03d", i+1)
		_ = cw.Write(append([]string{ref, groups[i], party, amounts[i]}, examples[i]...))
		who := party
		if groups[i] != "" {
			who = party + " (" + groups[i] + ")"
		}
		if merged {
			pages := [][]string{}
			for _, d := range sampleDocs {
				pages = append(pages, samplePage(d, ref, who))
			}
			put(ref+"/merged.pdf", simplePDF(pages))
		} else {
			for _, d := range sampleDocs {
				put(ref+"/"+d.file, simplePDF([][]string{samplePage(d, ref, who)}))
			}
		}
	}
	cw.Flush()
	put("disputes.csv", csvBuf.Bytes())
	layout := "Each dispute folder holds its documents as separate files. The six named here are the usual ones, not a\r\n" +
		"   limit: add every other document the dispute relies on (claim forms, denials, license, contract,\r\n" +
		"   medical records...) under any name, and leave out any that don't apply."
	if merged {
		layout = "Each dispute folder holds ONE merged.pdf with the dispute's documents, one after another, plus any\r\n" +
			"   other files the dispute relies on."
	}
	put("README.txt", []byte(strings.Join([]string{
		"Sample bulk filing - how to use it",
		"",
		"1. disputes.csv lists the disputes, one row each. party_name must be one of your clients that is ready",
		"   (profile and payment method done). other_party_name / other_party_email are the health plan the",
		"   dispute is against and where its notices go. Replace the example rows with your own.",
		"   provider_group is the provider group the client is in on your Clients tab (optional). Fill it in",
		"   when two of your providers share a name; a provider in a different group than the one given is",
		"   refused, so a dispute is never filed for the wrong provider.",
		"2. One folder per dispute, named exactly like its dispute_ref (CLAIM-001, ...).",
		"   " + layout,
		"3. Replace every sample PDF with the real document. The samples say what each one usually shows.",
		"   Every document is read and flagged for the reviewer if it doesn't look like its name; none is refused.",
		"   A ZIP that still contains a sample document is refused, so nothing is filed by mistake.",
		"4. Zip the CSV and the folders together (disputes.csv at the top level) and upload it on",
		"   'File many disputes at once'. Up to 500 disputes per ZIP, 512 MB at most.",
		"",
	}, "\r\n")))
	if err := zw.Close(); err != nil {
		http.Error(w, `{"error":"could not build the sample"}`, http.StatusInternalServerError)
		return
	}
	name := "sample-disputes-separate.zip"
	if merged {
		name = "sample-disputes-merged.zip"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(buf.Bytes())
}

// zipHasSampleDocs: a folder still holding one of the sample documents.
func zipHasSampleDocs(entries []zipEntry) bool {
	for _, e := range entries {
		rc, err := e.zf.Open()
		if err != nil {
			continue
		}
		var b bytes.Buffer
		_, _ = b.ReadFrom(rc)
		rc.Close()
		if bytes.Contains(b.Bytes(), []byte(sampleDocMarker)) {
			return true
		}
	}
	return false
}
