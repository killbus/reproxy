package reproxy

import (
	"bytes"
	"io"
)

// CapturedBody is the request body captured (buffered) for replay across
// retry attempts. A body can be read from the wire only once, so retrying a
// request requires holding a copy; Capture bounds that copy with a cap.
type CapturedBody struct {
	// Data holds the full body bytes when it fits within the cap.
	// Oversized bodies are not buffered (Data is nil).
	Data []byte
	// Oversized reports that the body exceeded the cap: the caller either
	// degrades to a single pass-through attempt or, in strict mode, rejects
	// the request with 413. Capture itself never rejects.
	Oversized bool
	// Len is the number of bytes read while probing. For oversized bodies
	// it is best-effort: the probe reads at most cap+1 bytes, so Len is the
	// probe size, not the full body length.
	Len int64
}

// Capture reads at most cap+1 bytes from src (the Traefik-style probe
// pattern): if the body fits within cap, it is fully buffered and replayable;
// if cap+1 bytes could be read, the body is oversized and NOT buffered.
//
// A cap of 0 captures only a truly empty body; any byte makes it oversized.
// Strict-mode rejection is the caller's concern (proxy layer); Capture just
// reports the condition so behavior stays testable in isolation.
func Capture(src io.Reader, cap int64) (*CapturedBody, *RequestError) {
	buf := make([]byte, cap+1)
	n, err := io.ReadFull(src, buf)
	switch {
	case err == nil:
		// ReadFull filled cap+1 bytes: there is at least one byte over the
		// cap. The body is oversized; do not keep the copy.
		return &CapturedBody{Data: nil, Oversized: true, Len: int64(n)}, nil
	case err == io.EOF, err == io.ErrUnexpectedEOF:
		// n bytes then end-of-stream: fits within the cap (n <= cap).
		if cap == 0 && n > 0 {
			// A zero cap cannot buffer anything; any body is oversized.
			return &CapturedBody{Data: nil, Oversized: true, Len: int64(n)}, nil
		}
		return &CapturedBody{Data: buf[:n], Oversized: false, Len: int64(n)}, nil
	default:
		return nil, &RequestError{
			Code:   400,
			Reason: "failed reading request body",
			Hint:   "check the client connection and resend the request",
		}
	}
}

// Reader returns a fresh reader over the captured bytes, one per retry
// attempt. Each call yields an independent reader positioned at the start,
// so sequential attempts each see the full body. It returns nil for
// oversized or empty captures (there is nothing to replay).
func (b *CapturedBody) Reader() *bytes.Reader {
	if b == nil || len(b.Data) == 0 {
		return nil
	}
	return bytes.NewReader(b.Data)
}
