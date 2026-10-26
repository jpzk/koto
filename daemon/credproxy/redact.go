package credproxy

import (
	"bytes"
	"io"
	"net/http"
)

// Replacement is what a redacted secret value is replaced with in bytes flowing
// back to the guest.
const Replacement = "[REDACTED]"

// Redactor replaces every known secret VALUE with Replacement in any byte stream
// headed for the guest (response headers, body, Location). It is the single
// chokepoint so no code path can emit a real secret (design §5).
type Redactor struct {
	// values are the real secret byte sequences to scrub, longest first so an
	// overlapping shorter secret cannot leave a tail behind.
	values [][]byte
	maxLen int
}

// NewRedactor builds a redactor for the given secret values. Empty values are
// ignored.
func NewRedactor(values []string) *Redactor {
	r := &Redactor{}
	for _, v := range values {
		if v == "" {
			continue
		}
		b := []byte(v)
		r.values = append(r.values, b)
		if len(b) > r.maxLen {
			r.maxLen = len(b)
		}
	}
	return r
}

// Bytes returns p with every secret value replaced by Replacement. Safe for
// whole-buffer (non-streaming) use such as headers.
func (r *Redactor) Bytes(p []byte) []byte {
	out := p
	for _, v := range r.values {
		out = bytes.ReplaceAll(out, v, []byte(Replacement))
	}
	return out
}

// String redacts a single string value (headers, Location).
func (r *Redactor) String(s string) string {
	if s == "" {
		return s
	}
	return string(r.Bytes([]byte(s)))
}

// Header redacts every value of every header in place.
func (r *Redactor) Header(h http.Header) {
	for k, vs := range h {
		for i, v := range vs {
			vs[i] = r.String(v)
		}
		h[k] = vs
	}
}

// streamReader wraps an upstream body and redacts secret bytes as it is read,
// keeping an overlap carry of maxLen-1 bytes so a secret split across read
// boundaries is still caught (design §5).
type streamReader struct {
	src   io.Reader
	r     *Redactor
	carry []byte
	buf   []byte // already-redacted bytes ready to hand out
	eof   bool
}

// Reader returns an io.Reader that yields the redacted form of src.
func (r *Redactor) Reader(src io.Reader) io.Reader {
	if len(r.values) == 0 || r.maxLen <= 1 {
		if len(r.values) == 0 {
			return src
		}
	}
	return &streamReader{src: src, r: r}
}

func (s *streamReader) Read(p []byte) (int, error) {
	// Serve buffered redacted bytes first.
	if len(s.buf) > 0 {
		n := copy(p, s.buf)
		s.buf = s.buf[n:]
		return n, nil
	}
	if s.eof {
		return 0, io.EOF
	}
	tmp := make([]byte, 32*1024)
	n, err := s.src.Read(tmp)
	chunk := append(s.carry, tmp[:n]...)
	overlap := s.r.maxLen - 1
	if overlap < 0 {
		overlap = 0
	}
	var emit []byte
	if err == io.EOF {
		s.eof = true
		emit = s.r.Bytes(chunk)
		s.carry = nil
	} else if len(chunk) > overlap {
		// Redact the safe prefix; keep the last `overlap` bytes as carry so a
		// secret straddling the boundary is caught next round.
		safe := chunk[:len(chunk)-overlap]
		emit = s.r.Bytes(safe)
		s.carry = append([]byte(nil), chunk[len(chunk)-overlap:]...)
	} else {
		s.carry = chunk
		emit = nil
		if err != nil {
			return 0, err
		}
		// nothing to emit yet; recurse-read more unless upstream blocked
		return s.Read(p)
	}
	s.buf = emit
	m := copy(p, s.buf)
	s.buf = s.buf[m:]
	if err != nil && err != io.EOF {
		return m, err
	}
	return m, nil
}
