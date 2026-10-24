// Author: Patrick Munis at newwave
package main

import (
	"archive/zip"
	"bytes"
	"testing"
)

func TestParseDollars(t *testing.T) {
	for in, want := range map[string]int64{"1980": 198000, "1,980.00": 198000, "$612.40": 61240, " 0.5 ": 50} {
		if got, ok := parseDollars(in); !ok || got != want {
			t.Errorf("parseDollars(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "abc", "0", "-5"} {
		if _, ok := parseDollars(bad); ok {
			t.Errorf("parseDollars(%q) accepted", bad)
		}
	}
}

func testZip(t *testing.T, names ...string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, _ := zw.Create(n)
		_, _ = w.Write([]byte("x"))
	}
	_ = zw.Close()
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

func TestZipFoldersPerDispute(t *testing.T) {
	zr := testZip(t, "disputes.csv", "CLAIM-1/eob.pdf", "CLAIM-1/itemized_bill.pdf",
		"claim-2/merged.pdf", "__MACOSX/CLAIM-1/._eob.pdf", "CLAIM-1/._eob.pdf")
	folders, root := zipFolders(zr)
	if _, ok := root["disputes.csv"]; !ok {
		t.Fatal("disputes.csv not found at the root")
	}
	if len(folders["CLAIM-1"]) != 2 {
		t.Fatalf("CLAIM-1 has %d files, want 2 (macOS metadata ignored)", len(folders["CLAIM-1"]))
	}
	if _, ok := lookupFolder(folders, "CLAIM-2"); !ok {
		t.Fatal("folder lookup should ignore case")
	}
	if m, d := splitFolder(folders["claim-2"]); m == nil || len(d) != 0 {
		t.Fatal("a folder with merged.pdf is a merged package")
	}
	if m, d := splitFolder(folders["CLAIM-1"]); m != nil || len(d) != 2 {
		t.Fatal("a folder of slot files is a discrete package")
	}
}
