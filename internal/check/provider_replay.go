package check

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Provider request/response buffering limits. The wire body and the decoded
// body are both bounded; only the first megabyte stays in memory.
const (
	providerSpoolMemory     int64 = 1 << 20
	providerModelValueLimit       = 512
	providerKeyValueLimit         = 128
	providerMaxJSONDepth          = 128
	providerReadChunkSize         = 8 << 10
	providerUnknownPath           = "\x00"
)

// Prefix bounds are variables so tests can exercise the handoff boundary.
var (
	providerPrefixLimit = 64 << 10
	providerPrefixWait  = 2 * time.Second
)

var (
	errProviderTooLarge            = errors.New("请求体超过 512 MiB 上限")
	errProviderReadTimeout         = errors.New("读取请求体超时")
	errProviderBadJSON             = errors.New("请求体不是有效 JSON")
	errProviderUnsupportedEncoding = errors.New("不支持的 Content-Encoding，仅支持 gzip 与 zstd")
)

// providerSpool holds a request or response body for replay. Small bodies stay
// in memory; larger ones move to a 0600 temp file in the service data dir.
type providerSpool struct {
	dir   string
	limit int64
	mem   []byte
	name  string
	file  *os.File
	size  int64
}

func newProviderSpool(dir string, limit int64) *providerSpool {
	if limit <= 0 {
		limit = providerBodyLimit
	}
	return &providerSpool{dir: dir, limit: limit}
}

func (s *providerSpool) Write(p []byte) (int, error) {
	if s.size+int64(len(p)) > s.limit {
		return 0, errProviderTooLarge
	}
	if s.file == nil && s.size+int64(len(p)) > providerSpoolMemory {
		f, err := os.CreateTemp(s.dir, ".model-guard-body-*")
		if err != nil {
			return 0, err
		}
		if len(s.mem) > 0 {
			if _, err := f.Write(s.mem); err != nil {
				name := f.Name()
				_ = f.Close()
				_ = os.Remove(name)
				return 0, err
			}
		}
		s.file, s.name, s.mem = f, f.Name(), nil
	}
	if s.file != nil {
		n, err := s.file.Write(p)
		s.size += int64(n)
		return n, err
	}
	s.mem = append(s.mem, p...)
	s.size += int64(len(p))
	return len(p), nil
}

// ReadFrom streams r into the spool, enforcing the size limit.
func (s *providerSpool) ReadFrom(r io.Reader) (int64, error) {
	buf := make([]byte, 64<<10)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := s.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
}

func (s *providerSpool) Size() int64 { return s.size }

// Open returns an independent reader over the spooled bytes.
func (s *providerSpool) Open() (io.ReadCloser, error) {
	if s.file != nil {
		f, err := os.Open(s.name)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	return io.NopCloser(bytes.NewReader(s.mem)), nil
}

func (s *providerSpool) Close() error {
	var err error
	if s.file != nil {
		err = s.file.Close()
		s.file = nil
		if s.name != "" {
			if rerr := os.Remove(s.name); err == nil {
				err = rerr
			}
			s.name = ""
		}
	}
	s.mem = nil
	return err
}

// providerSpoolReader closes its spool when the body is released.
type providerSpoolReader struct {
	io.ReadCloser
	spool *providerSpool
}

func (r *providerSpoolReader) Close() error {
	err := r.ReadCloser.Close()
	if cerr := r.spool.Close(); err == nil {
		err = cerr
	}
	return err
}

// providerByteReader is a buffered byte reader with a hard byte budget.
type providerByteReader struct {
	r        io.Reader
	buf      []byte
	back     []byte
	pos      int
	n        int
	total    int64
	limit    int64
	terminal error
}

func (b *providerByteReader) fill() error {
	if b.pos < b.n {
		return nil
	}
	if b.terminal != nil {
		return b.terminal
	}
	if b.limit > 0 && b.total >= b.limit {
		b.terminal = errProviderTooLarge
		return b.terminal
	}
	if b.buf == nil {
		b.buf = make([]byte, 32<<10)
	}
	n, err := b.r.Read(b.buf)
	if n > 0 {
		b.pos, b.n = 0, n
		b.total += int64(n)
		if b.limit > 0 && b.total > b.limit {
			b.terminal = errProviderTooLarge
			return b.terminal
		}
		if err != nil {
			// Readers may return the final bytes together with EOF.
			b.terminal = err
		}
		return nil
	}
	if err == nil {
		err = io.ErrNoProgress
	}
	b.terminal = err
	return err
}

// UnreadByte pushes one lookahead byte back. It survives a buffer refill.
func (b *providerByteReader) unreadByte(c byte) {
	b.unreadBytes([]byte{c})
}

// UnreadBytes pushes lookahead back in the order it was read.
func (b *providerByteReader) unreadBytes(data []byte) {
	if len(data) == 0 {
		return
	}
	b.back = append(append([]byte(nil), data...), b.back...)
}

func (b *providerByteReader) ReadByte() (byte, error) {
	if len(b.back) > 0 {
		c := b.back[0]
		b.back = b.back[1:]
		return c, nil
	}
	if b.pos >= b.n {
		if err := b.fill(); err != nil {
			return 0, err
		}
	}
	c := b.buf[b.pos]
	b.pos++
	return c, nil
}

// providerScanValues is the small set of fields the guard needs from a JSON
// document. Everything else is skipped without materializing values.
type providerScanValues struct {
	Model              string
	Type               string
	ResponseID         string
	responseIDRank     int
	PreviousResponseID string
	Compaction         bool
	Generate           bool
	HasGenerate        bool
}

// scanProviderJSON validates a complete JSON document and collects only model,
// compaction_trigger and generate=false. Unknown values (tools, signatures,
// base64 images) are skipped in place instead of being allocated.
func scanProviderJSON(r io.Reader, limit int64) (providerScanValues, error) {
	return scanProviderJSONMode(r, limit, false)
}

// scanProviderJSONRequest extracts the request's top-level model only. Response
// shapes (message.model, response.model) must not select the monitored target.
func scanProviderJSONRequest(r io.Reader, limit int64) (providerScanValues, error) {
	return scanProviderJSONMode(r, limit, true)
}

func scanProviderJSONMode(r io.Reader, limit int64, request bool) (providerScanValues, error) {
	var out providerScanValues
	sc := &providerJSONScanner{r: &providerByteReader{r: r, limit: limit}, request: request}
	if err := sc.scanValue("", &out, 0); err != nil {
		return out, err
	}
	if err := sc.skipSpace(); err != nil {
		if err == io.EOF {
			return out, nil
		}
		return out, err
	}
	return out, errProviderBadJSON
}

type providerJSONScanner struct {
	r       *providerByteReader
	request bool
}

func (sc *providerJSONScanner) skipSpace() error {
	for {
		c, err := sc.r.ReadByte()
		if err != nil {
			return err
		}
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		}
		sc.r.unreadByte(c)
		return nil
	}
}

func (sc *providerJSONScanner) scanValue(path string, out *providerScanValues, depth int) error {
	if depth > providerMaxJSONDepth {
		return errProviderBadJSON
	}
	if err := sc.skipSpace(); err != nil {
		return errProviderBadJSON
	}
	c, err := sc.r.ReadByte()
	if err != nil {
		return errProviderBadJSON
	}
	if path == "compaction_trigger" {
		out.Compaction = true
	}
	switch c {
	case '{':
		return sc.scanObject(path, out, depth+1)
	case '[':
		return sc.scanArray(path, out, depth+1)
	case '"':
		value, ok, err := sc.scanString(providerModelValueLimit)
		if err != nil {
			return err
		}
		if ok {
			observeProviderJSONString(path, value, out, sc.request)
		}
		return nil
	case 't':
		if err := sc.scanLiteral("rue"); err != nil {
			return err
		}
		if path == "generate" {
			out.HasGenerate, out.Generate = true, true
		}
		return nil
	case 'f':
		if err := sc.scanLiteral("alse"); err != nil {
			return err
		}
		if path == "generate" {
			out.HasGenerate, out.Generate = true, false
		}
		return nil
	case 'n':
		return sc.scanLiteral("ull")
	default:
		return sc.scanNumber(c)
	}
}

func (sc *providerJSONScanner) scanObject(path string, out *providerScanValues, depth int) error {
	if err := sc.skipSpace(); err != nil {
		return errProviderBadJSON
	}
	c, err := sc.r.ReadByte()
	if err != nil {
		return errProviderBadJSON
	}
	if c == '}' {
		return nil
	}
	sc.r.unreadByte(c)
	for {
		if err := sc.skipSpace(); err != nil {
			return errProviderBadJSON
		}
		quote, err := sc.r.ReadByte()
		if err != nil || quote != '"' {
			return errProviderBadJSON
		}
		key, ok, err := sc.scanString(providerKeyValueLimit)
		if err != nil {
			return err
		}
		childPath := providerUnknownPath
		if ok && key != "" && path != providerUnknownPath {
			if path == "" {
				childPath = key
			} else {
				childPath = path + "." + key
			}
		}
		if err := sc.skipSpace(); err != nil {
			return errProviderBadJSON
		}
		colon, err := sc.r.ReadByte()
		if err != nil || colon != ':' {
			return errProviderBadJSON
		}
		if err := sc.scanValue(childPath, out, depth); err != nil {
			return err
		}
		if err := sc.skipSpace(); err != nil {
			return errProviderBadJSON
		}
		sep, err := sc.r.ReadByte()
		if err != nil {
			return errProviderBadJSON
		}
		switch sep {
		case ',':
			continue
		case '}':
			return nil
		default:
			return errProviderBadJSON
		}
	}
}

func (sc *providerJSONScanner) scanArray(path string, out *providerScanValues, depth int) error {
	if err := sc.skipSpace(); err != nil {
		return errProviderBadJSON
	}
	c, err := sc.r.ReadByte()
	if err != nil {
		return errProviderBadJSON
	}
	if c == ']' {
		return nil
	}
	sc.r.unreadByte(c)
	index := 0
	for {
		childPath := providerUnknownPath
		if index == 0 && path != providerUnknownPath {
			childPath = path + ".0"
		}
		if err := sc.scanValue(childPath, out, depth); err != nil {
			return err
		}
		index++
		if err := sc.skipSpace(); err != nil {
			return errProviderBadJSON
		}
		sep, err := sc.r.ReadByte()
		if err != nil {
			return errProviderBadJSON
		}
		switch sep {
		case ',':
			continue
		case ']':
			return nil
		default:
			return errProviderBadJSON
		}
	}
}

// scanString consumes a JSON string (opening quote already consumed) and
// returns it when it fits max; longer values are skipped without allocation.
func (sc *providerJSONScanner) scanString(max int) (string, bool, error) {
	var raw []byte
	keep := true
	for {
		c, err := sc.r.ReadByte()
		if err != nil {
			return "", false, errProviderBadJSON
		}
		switch c {
		case '"':
			if !keep {
				return "", false, nil
			}
			return string(raw), true, nil
		case '\\':
			esc, err := sc.r.ReadByte()
			if err != nil {
				return "", false, errProviderBadJSON
			}
			var decoded []byte
			switch esc {
			case '"', '\\', '/':
				decoded = []byte{esc}
			case 'b':
				decoded = []byte{'\b'}
			case 'f':
				decoded = []byte{'\f'}
			case 'n':
				decoded = []byte{'\n'}
			case 'r':
				decoded = []byte{'\r'}
			case 't':
				decoded = []byte{'\t'}
			case 'u':
				r, err := sc.scanUnicodeEscape()
				if err != nil {
					return "", false, err
				}
				decoded = []byte(string(r))
			default:
				return "", false, errProviderBadJSON
			}
			if keep {
				raw = append(raw, decoded...)
				if len(raw) > max {
					keep, raw = false, nil
				}
			}
		default:
			if c < 0x20 {
				return "", false, errProviderBadJSON
			}
			if keep {
				raw = append(raw, c)
				if len(raw) > max {
					keep, raw = false, nil
				}
			}
		}
	}
}

func (sc *providerJSONScanner) scanUnicodeEscape() (rune, error) {
	hex4 := func() (rune, error) {
		var v rune
		for range 4 {
			c, err := sc.r.ReadByte()
			if err != nil {
				return 0, errProviderBadJSON
			}
			var d rune
			switch {
			case c >= '0' && c <= '9':
				d = rune(c - '0')
			case c >= 'a' && c <= 'f':
				d = rune(c-'a') + 10
			case c >= 'A' && c <= 'F':
				d = rune(c-'A') + 10
			default:
				return 0, errProviderBadJSON
			}
			v = v<<4 | d
		}
		return v, nil
	}
	first, err := hex4()
	if err != nil {
		return 0, err
	}
	if first >= 0xD800 && first <= 0xDBFF {
		// Try to combine a surrogate pair; an unpaired half becomes U+FFFD.
		c1, err := sc.r.ReadByte()
		if err != nil {
			return 0, errProviderBadJSON
		}
		c2, err := sc.r.ReadByte()
		if err != nil {
			return 0, errProviderBadJSON
		}
		if c1 == '\\' && c2 == 'u' {
			second, err := hex4()
			if err != nil {
				return 0, err
			}
			if second >= 0xDC00 && second <= 0xDFFF {
				return 0x10000 + (first-0xD800)<<10 + (second - 0xDC00), nil
			}
			sc.r.unreadBytes(providerUnicodeEscape(second))
			return 0xFFFD, nil
		}
		sc.r.unreadBytes([]byte{c1, c2})
		return 0xFFFD, nil
	}
	if first >= 0xDC00 && first <= 0xDFFF {
		return 0xFFFD, nil
	}
	return first, nil
}

// providerUnicodeEscape writes one \uXXXX escape for a lookahead that must be
// scanned again after an unpaired surrogate.
func providerUnicodeEscape(v rune) []byte {
	const digits = "0123456789ABCDEF"
	return []byte{'\\', 'u', digits[(v>>12)&0xF], digits[(v>>8)&0xF], digits[(v>>4)&0xF], digits[v&0xF]}
}

func (sc *providerJSONScanner) scanLiteral(rest string) error {
	for i := range len(rest) {
		c, err := sc.r.ReadByte()
		if err != nil || c != rest[i] {
			return errProviderBadJSON
		}
	}
	return nil
}

func (sc *providerJSONScanner) scanNumber(first byte) error {
	if first == '-' {
		c, err := sc.r.ReadByte()
		if err != nil {
			return errProviderBadJSON
		}
		first = c
	}
	if first == '0' {
		next, err := sc.r.ReadByte()
		if err != nil && err != io.EOF {
			return err
		}
		if err == nil {
			sc.r.unreadByte(next)
			if next >= '0' && next <= '9' {
				return errProviderBadJSON
			}
		}
	} else if first >= '1' && first <= '9' {
		for {
			next, err := sc.r.ReadByte()
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			if next < '0' || next > '9' {
				sc.r.unreadByte(next)
				break
			}
		}
	} else {
		return errProviderBadJSON
	}
	next, err := sc.r.ReadByte()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	sc.r.unreadByte(next)
	if next == '.' {
		if _, err := sc.r.ReadByte(); err != nil {
			return err
		}
		count := 0
		for {
			d, err := sc.r.ReadByte()
			if err != nil {
				if err == io.EOF {
					if count == 0 {
						return errProviderBadJSON
					}
					return nil
				}
				return err
			}
			if d < '0' || d > '9' {
				sc.r.unreadByte(d)
				break
			}
			count++
		}
		if count == 0 {
			return errProviderBadJSON
		}
		next, err = sc.r.ReadByte()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		sc.r.unreadByte(next)
	}
	if next == 'e' || next == 'E' {
		if _, err := sc.r.ReadByte(); err != nil {
			return errProviderBadJSON
		}
		c, err := sc.r.ReadByte()
		if err != nil {
			return errProviderBadJSON
		}
		if c == '+' || c == '-' {
			c, err = sc.r.ReadByte()
			if err != nil {
				return errProviderBadJSON
			}
		}
		if c < '0' || c > '9' {
			return errProviderBadJSON
		}
		for {
			d, err := sc.r.ReadByte()
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			if d < '0' || d > '9' {
				sc.r.unreadByte(d)
				next = d
				break
			}
		}
	}
	last, err := sc.r.ReadByte()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	sc.r.unreadByte(last)
	if !isJSONDelimiter(last) {
		return errProviderBadJSON
	}
	return nil
}

func isJSONDelimiter(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ',', '}', ']':
		return true
	}
	return false
}

func observeProviderJSONString(path, value string, out *providerScanValues, request bool) {
	if path == providerUnknownPath {
		return
	}
	switch path {
	case "type":
		if out.Type == "" {
			out.Type = strings.TrimSpace(value)
		}
		return
	case "response_id":
		considerProviderResponseID(out, value, 3)
		return
	case "response.id":
		considerProviderResponseID(out, value, 3)
		return
	case "responseId", "response.responseId":
		// Gemini and Code Assist name the reply's id responseId.
		considerProviderResponseID(out, value, 3)
		return
	case "previous_response_id":
		if out.PreviousResponseID == "" {
			out.PreviousResponseID = strings.TrimSpace(value)
		}
		return
	case "message.id":
		// Anthropic stream events put the message id under message. A content
		// block's top-level id must not outrank it.
		if !request {
			considerProviderResponseID(out, value, 2)
		}
		return
	case "id":
		// Chat and Anthropic whole replies carry the id at the top level.
		// Responses stream events also have item ids there, hence the lower
		// rank than response.id/response_id.
		if !request {
			considerProviderResponseID(out, value, 1)
		}
		return
	}
	if request {
		// Only the request's own top-level model names the target. Response
		// shapes such as message.model must not select it, and a repeated
		// model field follows the decoder's last-value-wins rule.
		if path == "model" {
			out.Model = strings.TrimSpace(value)
		}
		return
	}
	if out.Model != "" {
		return
	}
	switch path {
	case "model", "modelVersion", "model_name",
		"response.model", "response.modelVersion",
		"message.model",
		"candidates.0.model", "candidates.0.modelVersion":
		out.Model = strings.TrimSpace(value)
	}
}

// considerProviderResponseID keeps the most authoritative reply id seen in a
// document: an explicit response id outranks message.id, which outranks a bare
// top-level id that can also name a Responses output item.
func considerProviderResponseID(out *providerScanValues, value string, rank int) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > providerModelValueLimit || rank < out.responseIDRank {
		return
	}
	if rank == out.responseIDRank && out.ResponseID != "" {
		return
	}
	out.ResponseID, out.responseIDRank = value, rank
}

// providerDecodeReader wraps body with a gzip or zstd decoder. The returned
// reader closes both the decoder and the source body.
func providerDecodeReader(encoding string, body io.ReadCloser) (io.ReadCloser, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		gz, err := gzip.NewReader(body)
		if err != nil {
			return nil, err
		}
		return &providerDecodedBody{Reader: gz, decoders: []io.Closer{gz}, source: body}, nil
	case "zstd":
		dec, err := zstd.NewReader(body, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		return &providerDecodedBody{Reader: dec, decoders: []io.Closer{dec.IOReadCloser()}, source: body}, nil
	}
	return nil, errProviderUnsupportedEncoding
}

type providerDecodedBody struct {
	io.Reader
	decoders []io.Closer
	source   io.ReadCloser
}

func (p *providerDecodedBody) Close() error {
	var err error
	for _, c := range p.decoders {
		if cerr := c.Close(); err == nil {
			err = cerr
		}
	}
	if cerr := p.source.Close(); err == nil {
		err = cerr
	}
	return err
}

// providerContentEncoding reports a single Content-Encoding token, "" when the
// body is unencoded, and "other" for unsupported or stacked encodings.
func providerContentEncoding(header string) string {
	enc := strings.ToLower(strings.TrimSpace(header))
	if enc == "" || enc == "identity" {
		return ""
	}
	if strings.Contains(enc, ",") {
		return "other"
	}
	switch enc {
	case "gzip", "zstd":
		return enc
	}
	return "other"
}

// providerStreamFormat selects the prefix parser; the caller decides from the
// response Content-Type, never from the client's stream flag.
type providerStreamFormat int

const (
	providerStreamSSE providerStreamFormat = iota + 1
	providerStreamNDJSON
)

// providerHandoff owns the single in-flight Read on an upstream body and hands
// chunks to a consumer one at a time. Ownership can move from the prefix
// inspector to the response proxy without losing bytes or leaking a goroutine.
type providerHandoff struct {
	body       io.ReadCloser
	ch         chan providerReadChunk
	stop       chan struct{}
	stopOnce   sync.Once
	pending    []byte
	pendingErr error
}

type providerReadChunk struct {
	data []byte
	err  error
}

func newProviderHandoff(body io.ReadCloser) *providerHandoff {
	h := &providerHandoff{body: body, ch: make(chan providerReadChunk, 1), stop: make(chan struct{})}
	go h.loop()
	return h
}

func (h *providerHandoff) loop() {
	buf := make([]byte, providerReadChunkSize)
	for {
		n, err := h.body.Read(buf)
		var data []byte
		if n > 0 {
			data = append([]byte(nil), buf[:n]...)
		}
		if n > 0 || err != nil {
			select {
			case h.ch <- providerReadChunk{data: data, err: err}:
			case <-h.stop:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (h *providerHandoff) Read(p []byte) (int, error) {
	for {
		if len(h.pending) > 0 {
			// Keep a trailing error for the next call: a reader may return the
			// final bytes and EOF together, and io.Copy expects EOF separately.
			n := copy(p, h.pending)
			h.pending = h.pending[n:]
			return n, nil
		}
		if h.pendingErr != nil {
			err := h.pendingErr
			h.pendingErr = nil
			if err == io.EOF {
				return 0, io.EOF
			}
			return 0, err
		}
		select {
		case chunk, ok := <-h.ch:
			if !ok {
				return 0, io.EOF
			}
			if len(chunk.data) > 0 {
				h.pending, h.pendingErr = chunk.data, chunk.err
				continue
			}
			if chunk.err != nil {
				if chunk.err == io.EOF {
					return 0, io.EOF
				}
				return 0, chunk.err
			}
		case <-h.stop:
			return 0, io.EOF
		}
	}
}

// noteTerminal records a trailing error that arrived with the last accepted
// chunk so the next Read reports it instead of blocking on a finished reader.
func (h *providerHandoff) noteTerminal(err error) {
	if err == nil || h.pendingErr != nil {
		return
	}
	h.pendingErr = err
}

func (h *providerHandoff) Close() error {
	h.stopOnce.Do(func() {
		close(h.stop)
		_ = h.body.Close()
	})
	return nil
}

// inspectProviderPrefix buffers at most providerPrefixLimit bytes and waits at
// most providerPrefixWait for the first model value. On any boundary it returns
// the captured prefix and a reader continuing with the same upstream body.
func inspectProviderPrefix(ctx context.Context, body io.ReadCloser, format providerStreamFormat) ([]byte, io.ReadCloser, string, error) {
	handoff := newProviderHandoff(body)
	var prefix []byte
	timer := time.NewTimer(providerPrefixWait)
	defer timer.Stop()
	for {
		if len(prefix) >= providerPrefixLimit {
			return prefix, handoff, "", nil
		}
		select {
		case <-ctx.Done():
			_ = handoff.Close()
			return nil, nil, "", ctx.Err()
		case <-timer.C:
			return prefix, handoff, "", nil
		case chunk, ok := <-handoff.ch:
			if !ok {
				return prefix, handoff, "", nil
			}
			if len(chunk.data) > 0 {
				prefix = append(prefix, chunk.data...)
				if model := providerStreamModel(prefix, format); model != "" {
					handoff.noteTerminal(chunk.err)
					return prefix, handoff, model, nil
				}
			}
			if chunk.err != nil {
				if chunk.err == io.EOF {
					handoff.noteTerminal(io.EOF)
					return prefix, handoff, "", nil
				}
				return prefix, handoff, "", chunk.err
			}
		}
	}
}

// providerStreamModel scans complete SSE events or NDJSON lines for a model.
// Incomplete trailing data is left for the forwarding path.
func providerStreamModel(buf []byte, format providerStreamFormat) string {
	switch format {
	case providerStreamSSE:
		return providerSSEModel(buf)
	case providerStreamNDJSON:
		return providerNDJSONModel(buf)
	}
	return ""
}

// supplierStreamReplyID returns the reply id found in complete SSE events or
// NDJSON lines of a captured supplier response prefix. It is used only to
// correlate that response with Magpie's async hook; an empty id means the
// vendor (or the prefix window) did not expose one.
func supplierStreamReplyID(prefix []byte, format providerStreamFormat) string {
	return providerStreamScanValues(prefix, format).ResponseID
}

// providerStreamScanValues scans the complete SSE events or NDJSON lines in a
// captured prefix and returns the first model plus the most authoritative
// reply id. Callers use the id to correlate a synchronously judged supplier
// response with Magpie's later async hook; incomplete trailing data is skipped.
func providerStreamScanValues(buf []byte, format providerStreamFormat) providerScanValues {
	switch format {
	case providerStreamSSE:
		return providerSSEScanValues(buf)
	case providerStreamNDJSON:
		return providerNDJSONScanValues(buf)
	}
	return providerScanValues{}
}

func providerSSEModel(buf []byte) string {
	return providerSSEScanValues(buf).Model
}

func providerSSEScanValues(buf []byte) providerScanValues {
	var out providerScanValues
	offset := 0
	for {
		end := -1
		start := offset
		for i := offset; i <= len(buf); i++ {
			if i == len(buf) {
				break
			}
			if buf[i] != '\n' {
				continue
			}
			line := buf[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) == 0 {
				end = i + 1
				break
			}
			start = i + 1
		}
		if end < 0 {
			return out
		}
		mergeProviderScanValues(&out, providerSSEEventValues(buf[offset:end]))
		offset = end
		if offset >= len(buf) {
			return out
		}
	}
}

func providerSSEEventModel(event []byte) string {
	return providerSSEEventValues(event).Model
}

func providerSSEEventValues(event []byte) providerScanValues {
	var data []string
	for _, raw := range bytes.Split(event, []byte("\n")) {
		line := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			data = append(data, value)
		}
	}
	if len(data) == 0 {
		return providerScanValues{}
	}
	payload := strings.TrimSpace(strings.Join(data, "\n"))
	if payload == "" || payload == "[DONE]" {
		return providerScanValues{}
	}
	values, err := scanProviderJSON(strings.NewReader(payload), int64(len(payload))+1)
	if err != nil {
		return providerScanValues{}
	}
	return values
}

func providerNDJSONModel(buf []byte) string {
	return providerNDJSONScanValues(buf).Model
}

func providerNDJSONScanValues(buf []byte) providerScanValues {
	var out providerScanValues
	offset := 0
	for {
		idx := bytes.IndexByte(buf[offset:], '\n')
		if idx < 0 {
			return out
		}
		line := bytes.TrimSuffix(buf[offset:offset+idx], []byte("\r"))
		offset += idx + 1
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		values, err := scanProviderJSON(bytes.NewReader(line), int64(len(line))+1)
		if err != nil {
			continue
		}
		mergeProviderScanValues(&out, values)
	}
}

func mergeProviderScanValues(dst *providerScanValues, src providerScanValues) {
	if dst.Model == "" {
		dst.Model = src.Model
	}
	if dst.Type == "" {
		dst.Type = src.Type
	}
	if src.ResponseID != "" && (dst.ResponseID == "" || src.responseIDRank > dst.responseIDRank) {
		dst.ResponseID, dst.responseIDRank = src.ResponseID, src.responseIDRank
	}
	if dst.PreviousResponseID == "" {
		dst.PreviousResponseID = src.PreviousResponseID
	}
	dst.Compaction = dst.Compaction || src.Compaction
	if !dst.HasGenerate && src.HasGenerate {
		dst.Generate, dst.HasGenerate = src.Generate, true
	}
}

// providerPrependReader replays a captured prefix before the live body.
type providerPrependReader struct {
	prefix []byte
	rest   io.Reader
	closer io.Closer
}

func (p *providerPrependReader) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.rest.Read(b)
}

func (p *providerPrependReader) Close() error {
	if p.closer != nil {
		return p.closer.Close()
	}
	return nil
}
