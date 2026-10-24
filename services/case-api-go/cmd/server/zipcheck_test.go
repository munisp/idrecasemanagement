// Author: Patrick Munis at newwave
package main

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func zipOf(t *testing.T, files map[string][]byte, method uint16) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
	_ = zw.Close()
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

func TestZipChecks(t *testing.T) {
	pdf := []byte("%PDF-1.4 a small document")
	if p := checkZipEntries(zipOf(t, map[string][]byte{"disputes.csv": []byte("a,b"), "D-1/eob.pdf": pdf, "__MACOSX/._eob.pdf": {1}}, zip.Deflate)); len(p) != 0 {
		t.Errorf("a good ZIP was refused: %v", p)
	}
	cases := map[string]map[string][]byte{
		"unsafe file path":           {"../../etc/passwd.pdf": pdf},
		"archives inside the ZIP":    {"inner.zip": []byte("PK\x03\x04")},
		"possible zip bomb":          {"bomb.pdf": append([]byte("%PDF-"), bytes.Repeat([]byte{0}, 30<<20)...)},
		"the limit for this type is": {"huge.txt": bytes.Repeat([]byte("a"), 11<<20)},
	}
	for want, files := range cases {
		p := strings.Join(checkZipEntries(zipOf(t, files, zip.Deflate)), "; ")
		if !strings.Contains(p, want) {
			t.Errorf("expected %q, got %q", want, p)
		}
	}
	if p := checkZipEntries(zipOf(t, map[string][]byte{"__MACOSX/x": {1}}, zip.Store)); len(p) == 0 {
		t.Error("an empty ZIP must be refused")
	}
}

func TestDocPolicyLimits(t *testing.T) {
	if err := docPolicy("a.pdf", []byte("%PDF-1.4")); err != nil {
		t.Errorf("a small PDF: %v", err)
	}
	if err := docPolicy("a.pdf", nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty file must be refused: %v", err)
	}
	if err := docPolicy("big.png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 31<<20)...)); err == nil || !strings.Contains(err.Error(), "30.0 MB") {
		t.Errorf("an image over 30 MB must be refused: %v", err)
	}
	if err := docPolicy("page.html", []byte("<html><script>alert(1)</script>")); err == nil {
		t.Error("HTML must no longer be accepted")
	}
	if docSizeLimitFor("x.zip") != zipMaxBytes || docSizeLimitFor("x.exe") != 0 {
		t.Error("limits by name")
	}
}

func TestZipSafeNames(t *testing.T) {
	for in, want := range map[string]string{"FL26-1/../x": "FL26-1---x", "a:b": "a-b", "": "document"} {
		if got := zipSafe(in); got != want {
			t.Errorf("zipSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
