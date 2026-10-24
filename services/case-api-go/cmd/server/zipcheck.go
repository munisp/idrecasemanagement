// Author: Patrick Munis at newwave
package main

// zipcheck.go — what every uploaded ZIP (bulk filings, document packages)
// passes before any file in it is used:
//
//   - its size (zipMaxBytes), and that it is a ZIP we can read;
//   - the virus scan of the whole archive (ClamAV unpacks archives itself);
//   - each entry: no unsafe path (../, absolute, drive letters), no password
//     protection, a compression method we can read (stored or deflate), no
//     archive inside the archive, no link, within its type's size limit;
//   - the archive as a whole: at most zipMaxFiles files and zipMaxTotal bytes
//     unpacked, and no entry that expands far beyond its packed size (a zip
//     bomb).
//
// Each file taken out of it is then held to docPolicy and scanned on its own
// like any other upload.

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	zipMaxBytes = 512 << 20 // the ZIP as uploaded
	zipMaxFiles = 2000
	zipMaxTotal = 4 << 30 // unpacked, all entries together
	zipMaxRatio = 200     // unpacked / packed, per entry (above 1 MB)

	clamStreamMax = 100 << 20 // ClamAV's StreamMaxLength on this platform
)

var nestedArchiveExts = map[string]bool{".zip": true, ".rar": true, ".7z": true, ".gz": true, ".tgz": true, ".tar": true, ".bz2": true, ".xz": true, ".cab": true, ".iso": true}

// zipJunk: entries operating systems add on their own; skipped, not refused.
func zipJunk(name string) bool {
	base := filepath.Base(name)
	return strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(base, "._") || base == ".DS_Store" || base == "Thumbs.db" || base == "desktop.ini"
}

// checkZipEntries lists every reason the archive's contents are refused.
func checkZipEntries(zr *zip.Reader) []string {
	var problems []string
	files := 0
	var total uint64
	for _, f := range zr.File {
		name := f.Name
		if f.FileInfo().IsDir() || zipJunk(name) {
			continue
		}
		files++
		switch {
		case strings.Contains(name, "\x00"), strings.HasPrefix(name, "/"), strings.HasPrefix(name, "\\"),
			strings.Contains(name, ":"), strings.Contains("/"+strings.ReplaceAll(name, "\\", "/")+"/", "/../"):
			problems = append(problems, fmt.Sprintf("%s: unsafe file path", name))
			continue
		case f.Flags&0x1 != 0:
			problems = append(problems, fmt.Sprintf("%s: password-protected — save the ZIP without a password", name))
			continue
		case f.Method != zip.Store && f.Method != zip.Deflate:
			problems = append(problems, fmt.Sprintf("%s: packed with a compression method we cannot read — re-save it as a standard ZIP", name))
			continue
		case f.Mode()&os.ModeSymlink != 0:
			problems = append(problems, fmt.Sprintf("%s: links are not accepted", name))
			continue
		case nestedArchiveExts[strings.ToLower(filepath.Ext(name))]:
			problems = append(problems, fmt.Sprintf("%s: archives inside the ZIP are not accepted — put the documents in the ZIP itself", name))
			continue
		}
		if lim := docSizeLimitFor(name); lim > 0 && f.UncompressedSize64 > uint64(lim) {
			problems = append(problems, fmt.Sprintf("%s: %s — the limit for this type is %s", name, humanBytes(int64(f.UncompressedSize64)), humanBytes(lim)))
		}
		if f.CompressedSize64 > 0 && f.UncompressedSize64 > 1<<20 && f.UncompressedSize64/f.CompressedSize64 > zipMaxRatio {
			problems = append(problems, fmt.Sprintf("%s: expands %d times its packed size — refused as a possible zip bomb", name, f.UncompressedSize64/f.CompressedSize64))
		}
		total += f.UncompressedSize64
	}
	if files > zipMaxFiles {
		problems = append(problems, fmt.Sprintf("%d files — a ZIP may hold at most %d", files, zipMaxFiles))
	}
	if total > zipMaxTotal {
		problems = append(problems, fmt.Sprintf("unpacks to %s — at most %s", humanBytes(int64(total)), humanBytes(zipMaxTotal)))
	}
	if files == 0 {
		problems = append(problems, "the ZIP has no files in it")
	}
	return problems
}

// openUploadedZip runs every check on an uploaded ZIP and writes the refusal
// itself; ok=false means the response is written.
func (s *server) openUploadedZip(w http.ResponseWriter, raw []byte) (*zip.Reader, bool) {
	if len(raw) > zipMaxBytes {
		http.Error(w, fmt.Sprintf(`{"error":"the ZIP is over %s — split it into smaller ZIPs"}`, humanBytes(zipMaxBytes)), http.StatusRequestEntityTooLarge)
		return nil, false
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		http.Error(w, `{"error":"this is not a ZIP we can read — save it as a standard .zip (RAR and 7-Zip archives are not accepted)"}`, http.StatusBadRequest)
		return nil, false
	}
	if problems := checkZipEntries(zr); len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "the ZIP was refused: " + strings.Join(problems[:min(len(problems), 5)], "; "), "problems": problems})
		return nil, false
	}
	// The scanner takes a stream of up to clamStreamMax (its StreamMaxLength).
	// A larger ZIP is not scanned whole; every file taken out of it is
	// scanned on its own before it is stored, as any upload is.
	if len(raw) <= clamStreamMax {
		if _, ok := s.scanOrRefuse(w, bytes.NewReader(raw), "ZIP"); !ok {
			return nil, false
		}
	}
	return zr, true
}
