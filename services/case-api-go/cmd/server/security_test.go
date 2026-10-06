package main

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
)

// fakeClamd starts a minimal clamd INSTREAM listener on 127.0.0.1:0 that
// drains one zINSTREAM session (length-prefixed chunks terminated by a
// zero-length chunk) and replies with reply, exactly as real clamd does:
// NUL-terminated, never newline-terminated. TCP is a byte stream, not a
// message stream, so the terminator can arrive coalesced with the data in
// the same read() rather than alone -- framing must be parsed by length,
// not by assuming any one chunk lands in its own read.
func fakeClamd(t *testing.T, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		// "zINSTREAM\0" handshake.
		if _, err := r.ReadString(0); err != nil {
			return
		}
		var sz [4]byte
		for {
			if _, err := io.ReadFull(r, sz[:]); err != nil {
				return
			}
			n := binary.BigEndian.Uint32(sz[:])
			if n == 0 {
				conn.Write([]byte(reply + "\x00"))
				return
			}
			if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
				return
			}
		}
	}()
	return ln.Addr().String()
}

func TestClamScanCleanFile(t *testing.T) {
	addr := fakeClamd(t, "stream: OK")
	s := &server{cfg: Config{ClamdAddr: addr}}
	sig, err := s.clamScan(strings.NewReader("harmless content"))
	if err != nil {
		t.Fatalf("expected no error on clean reply, got %v", err)
	}
	if sig != "" {
		t.Fatalf("expected empty signature for clean file, got %q", sig)
	}
}

func TestClamScanInfectedFile(t *testing.T) {
	addr := fakeClamd(t, "stream: Eicar-Test-Signature FOUND")
	s := &server{cfg: Config{ClamdAddr: addr}}
	sig, err := s.clamScan(strings.NewReader("fake eicar"))
	if err != nil {
		t.Fatalf("expected no error on a detected-signature reply, got %v", err)
	}
	if sig != "Eicar-Test-Signature" {
		t.Fatalf("expected signature %q, got %q", "Eicar-Test-Signature", sig)
	}
}

func TestClamScanUnreachable(t *testing.T) {
	// A closed listener on a real port that nothing answers on.
	s := &server{cfg: Config{ClamdAddr: "127.0.0.1:1"}}
	if _, err := s.clamScan(strings.NewReader("anything")); err == nil {
		t.Fatal("expected an error when the scanner is unreachable")
	}
}
