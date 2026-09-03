package reproxy

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestCaptureEmptyBody(t *testing.T) {
	cb, err := Capture(strings.NewReader(""), 10)
	if err != nil {
		t.Fatalf("Capture error: %v", err)
	}
	if cb.Oversized {
		t.Errorf("empty body should not be oversized")
	}
	if cb.Len != 0 {
		t.Errorf("Len = %d, want 0", cb.Len)
	}
	if len(cb.Data) != 0 {
		t.Errorf("Data = %q, want empty", cb.Data)
	}
	if cb.Reader() != nil {
		t.Errorf("Reader() on empty capture should be nil")
	}
}

func TestCaptureUnderCap(t *testing.T) {
	cb, err := Capture(strings.NewReader("hello"), 10)
	if err != nil {
		t.Fatalf("Capture error: %v", err)
	}
	if cb.Oversized {
		t.Errorf("5-byte body within cap 10 should not be oversized")
	}
	if string(cb.Data) != "hello" {
		t.Errorf("Data = %q, want %q", cb.Data, "hello")
	}
	if cb.Len != 5 {
		t.Errorf("Len = %d, want 5", cb.Len)
	}
}

func TestCaptureExactlyAtCap(t *testing.T) {
	// A body of exactly cap bytes fits: NOT oversized. The probe reads
	// cap+1 and gets io.ErrUnexpectedEOF with n == cap.
	body := bytes.Repeat([]byte("x"), 1024)
	cb, err := Capture(bytes.NewReader(body), 1024)
	if err != nil {
		t.Fatalf("Capture error: %v", err)
	}
	if cb.Oversized {
		t.Errorf("body == cap should not be oversized")
	}
	if cb.Len != 1024 {
		t.Errorf("Len = %d, want 1024", cb.Len)
	}
	if !bytes.Equal(cb.Data, body) {
		t.Errorf("Data mismatch: got %d bytes, want %d", len(cb.Data), len(body))
	}
}

func TestCaptureOneByteOverCap(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 1025)
	cb, err := Capture(bytes.NewReader(body), 1024)
	if err != nil {
		t.Fatalf("Capture error: %v", err)
	}
	if !cb.Oversized {
		t.Errorf("body == cap+1 should be oversized")
	}
	if cb.Data != nil {
		t.Errorf("oversized body should not be buffered, got %d bytes", len(cb.Data))
	}
	if cb.Len != 1025 {
		t.Errorf("Len = %d, want 1025 (probe size)", cb.Len)
	}
	if cb.Reader() != nil {
		t.Errorf("Reader() on oversized capture should be nil")
	}
}

func TestCaptureLargeOversizedBody(t *testing.T) {
	// A body far beyond the cap: the probe stops at cap+1 (best-effort Len).
	body := bytes.Repeat([]byte("y"), 5<<20) // 5 MiB, cap 1 MiB
	cb, err := Capture(bytes.NewReader(body), 1<<20)
	if err != nil {
		t.Fatalf("Capture error: %v", err)
	}
	if !cb.Oversized {
		t.Errorf("5 MiB body with 1 MiB cap should be oversized")
	}
	if cb.Len != (1<<20)+1 {
		t.Errorf("Len = %d, want %d (probe reads at most cap+1)", cb.Len, (1<<20)+1)
	}
}

func TestCaptureZeroCap(t *testing.T) {
	// cap == 0: any non-empty body is oversized; an empty body is fine.
	cb, err := Capture(strings.NewReader(""), 0)
	if err != nil {
		t.Fatalf("Capture error: %v", err)
	}
	if cb.Oversized || cb.Len != 0 {
		t.Errorf("empty body with cap 0: Oversized=%v Len=%d, want false/0", cb.Oversized, cb.Len)
	}

	cb, err = Capture(strings.NewReader("a"), 0)
	if err != nil {
		t.Fatalf("Capture error: %v", err)
	}
	if !cb.Oversized {
		t.Errorf("1-byte body with cap 0 should be oversized")
	}
	if cb.Len != 1 {
		t.Errorf("Len = %d, want 1", cb.Len)
	}
}

func TestCaptureReaderReplayIndependence(t *testing.T) {
	// Two sequential readers must each see the full content from the start.
	cb, err := Capture(strings.NewReader("replayable body"), 64)
	if err != nil {
		t.Fatalf("Capture error: %v", err)
	}
	r1 := cb.Reader()
	if r1 == nil {
		t.Fatalf("Reader() should be non-nil for non-empty capture")
	}
	first, rerr := io.ReadAll(r1)
	if rerr != nil {
		t.Fatalf("ReadAll(r1) error: %v", rerr)
	}
	if string(first) != "replayable body" {
		t.Errorf("r1 content = %q", first)
	}
	// Exhausting r1 must not affect a fresh reader.
	r2 := cb.Reader()
	second, rerr := io.ReadAll(r2)
	if rerr != nil {
		t.Fatalf("ReadAll(r2) error: %v", rerr)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("r2 content = %q, want identical to r1", second)
	}
	// And the first reader, still drained, stays drained.
	if _, err := r1.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("drained r1 should return EOF, got %v", err)
	}
}

func TestCaptureReadError(t *testing.T) {
	_, err := Capture(errReader{}, 10)
	if err == nil {
		t.Fatalf("Capture should surface read errors")
	}
	if err.Code != 400 {
		t.Errorf("code = %d, want 400", err.Code)
	}
}

// errReader always fails with a non-EOF error.
type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}
