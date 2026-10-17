package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// The sample ZIPs must pass the same checks a real upload does -- or a TPA copying them would hit errors the
// samples taught them -- and the upload must recognise their documents as samples.
func TestBulkSampleZipShape(t *testing.T) {
	pdf := simplePDF([][]string{{"Title", "a line (with parens) and \\ a backslash"}, {"Second page"}})
	if !bytes.HasPrefix(pdf, []byte("%PDF-1.4")) || !bytes.HasSuffix(pdf, []byte("%%EOF\n")) {
		t.Fatalf("not a PDF: %q", pdf[:20])
	}
	if err := docPolicy("eob.pdf", pdf); err != nil {
		t.Fatalf("sample PDF refused by the document policy: %v", err)
	}
	// xref offsets point at "N 0 obj"
	xref := bytes.LastIndex(pdf, []byte("xref\n"))
	lines := strings.Split(string(pdf[xref:]), "\n")[3:]
	for i, l := range lines {
		if !strings.HasSuffix(l, " n ") {
			break
		}
		var off int
		if _, err := fmtSscanf(l, &off); err != nil {
			t.Fatal(err)
		}
		want := []byte(strings.TrimSpace(strings.Split(string(pdf[off:off+12]), "\n")[0]))
		if !bytes.HasPrefix(want, []byte(strconv.Itoa(i+1)+" 0 obj")) {
			t.Errorf("xref entry %d points at %q", i+1, want)
		}
	}

	for _, merged := range []bool{false, true} {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		add := func(name string, b []byte) { f, _ := zw.Create(name); _, _ = f.Write(b) }
		var c bytes.Buffer
		cw := csv.NewWriter(&c)
		_ = cw.Write([]string{"dispute_ref", "party_name", "disputed_amount", "other_party_name", "other_party_email", "other_party_naic", "contact_name", "notes"})
		_ = cw.Write([]string{"CLAIM-001", "Gulf Coast Clinic", "1980.00", "Sunshine Health", "a@b.example", "", "", ""})
		cw.Flush()
		add("disputes.csv", c.Bytes())
		if merged {
			pages := [][]string{}
			for _, d := range sampleDocs {
				pages = append(pages, samplePage(d, "CLAIM-001", "Gulf Coast Clinic"))
			}
			add("CLAIM-001/merged.pdf", simplePDF(pages))
		} else {
			for _, d := range sampleDocs {
				add("CLAIM-001/"+d.file, simplePDF([][]string{samplePage(d, "CLAIM-001", "Gulf Coast Clinic")}))
			}
		}
		_ = zw.Close()
		zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
		if err != nil {
			t.Fatal(err)
		}
		if p := checkZipEntries(zr); len(p) != 0 {
			t.Fatalf("sample ZIP refused by the upload checks (merged=%v): %v", merged, p)
		}
		folders, root := zipFolders(zr)
		if root["disputes.csv"] == nil {
			t.Fatal("disputes.csv not at the top")
		}
		entries, ok := lookupFolder(folders, "claim-001")
		if !ok {
			t.Fatal("no CLAIM-001 folder")
		}
		m, discrete := splitFolder(entries)
		if merged && m == nil || !merged && len(discrete) != 6 {
			t.Errorf("merged=%v: folder split wrong (merged=%v, %d files)", merged, m != nil, len(discrete))
		}
		if !zipHasSampleDocs(entries) {
			t.Errorf("merged=%v: the upload must recognise the sample documents", merged)
		}
	}
	// A real document is not mistaken for a sample.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("CLAIM-9/eob.pdf")
	_, _ = f.Write(simplePDF([][]string{{"Real EOB", "claim 123"}}))
	_ = zw.Close()
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	folders, _ := zipFolders(zr)
	if zipHasSampleDocs(folders["CLAIM-9"]) {
		t.Error("a real document was taken for a sample")
	}
}

func fmtSscanf(line string, off *int) (int, error) { return fmt.Sscanf(strings.Fields(line)[0], "%d", off) }

