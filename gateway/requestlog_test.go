package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBucket accepts signed PUTs and passes each object on.
type fakeBucket struct {
	*httptest.Server
	objects chan object
	fail    atomic.Int32  // number of PUTs to refuse
	block   chan struct{} // if set, PUTs wait for it to close
}

type object struct {
	path, contentType string
	data              []byte
}

func newFakeBucket(t *testing.T) *fakeBucket {
	b := &fakeBucket{objects: make(chan object, 10)}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.block != nil {
			<-b.block
		}
		data, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(data)
		if r.Method != "PUT" || r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) ||
			!strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=key-id/") {
			http.Error(w, "bad upload", http.StatusBadRequest)
			return
		}
		if b.fail.Add(-1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "<Error><Code>SlowDown</Code><Message>Reduce your request rate.</Message></Error>")
			return
		}
		b.objects <- object{r.URL.EscapedPath(), r.Header.Get("Content-Type"), data}
	}))
	t.Cleanup(b.Close)
	return b
}

// next returns the next object, decoded.
func (b *fakeBucket) next(t *testing.T) (object, record) {
	t.Helper()
	select {
	case o := <-b.objects:
		data := o.data
		if strings.HasSuffix(o.path, ".gz") {
			zr, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			data, _ = io.ReadAll(zr)
		}
		for _, secret := range []string{"alice-token", "provider-key"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s holds %s", o.path, secret)
			}
		}
		var rec record
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("%s: %v", o.path, err)
		}
		return o, rec
	case <-time.After(5 * time.Second):
		t.Fatal("no object uploaded")
		return object{}, record{}
	}
}

// newLoggedGateway starts a gateway that logs to b. Its prefix needs
// escaping, so that the signed path is the one sent.
func newLoggedGateway(t *testing.T, b *fakeBucket, gzip bool, upstream string, change ...func(*Config)) (*gateway, *httptest.Server) {
	c := &Config{
		Clients: map[string]string{"alice": "alice-token"},
		Routes:  map[string]Route{"/p": {Upstream: upstream, Headers: map[string]string{"X-Api-Key": "provider-key"}}},
		RequestLog: &RequestLog{
			URL: b.URL + "/bucket/pre+fix/", Region: "auto", AccessKeyID: "key-id", SecretAccessKey: "secret", Gzip: gzip,
		},
	}
	for _, f := range change {
		f(c)
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	g := newGateway(c)
	s := httptest.NewServer(g)
	t.Cleanup(s.Close)
	return g, s
}

func TestRequestLog(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Request-Id", "req-1")
		w.WriteHeader(201)
		io.WriteString(w, `{"reply":"hi"}`)
	}))
	defer up.Close()

	for _, gz := range []bool{false, true} {
		b := newFakeBucket(t)
		_, gw := newLoggedGateway(t, b, gz, up.URL)
		req, _ := http.NewRequest("POST", gw.URL+"/p/v1/messages?beta=true", strings.NewReader(`{"prompt":"hello"}`))
		req.Header.Set("X-Api-Key", "alice-token")
		req.Header.Set("Cookie", "c=1")
		req.Header.Set("Proxy-Authorization", "Basic alice-token")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		before := time.Now()
		do(t, req)

		o, rec := b.next(t)
		key := regexp.MustCompile(`^/bucket/pre%2Bfix/alice/(\d{4}-\d\d-\d\d)/(\d{6}\.\d{3})-([0-9a-f]{16})\.json$`)
		contentType := "application/json"
		if gz {
			key = regexp.MustCompile(strings.Replace(key.String(), `\.json$`, `\.json\.gz$`, 1))
			contentType = "application/gzip"
		}
		m := key.FindStringSubmatch(o.path)
		if m == nil || o.contentType != contentType {
			t.Fatalf("gzip %v: key %s, content type %s", gz, o.path, o.contentType)
		}
		if m[1] != rec.Time.Format("2006-01-02") || m[2] != rec.Time.Format("150405.000") || m[3] != rec.ID ||
			rec.Time.Location() != time.UTC || rec.Time.Before(before.Truncate(time.Millisecond)) || time.Since(rec.Time) > 5*time.Second {
			t.Errorf("gzip %v: key %s for time %s, id %s", gz, o.path, rec.Time, rec.ID)
		}
		if rec.Client != "alice" || rec.Method != "POST" || rec.URL != "/p/v1/messages?beta=true" || rec.Status != 201 || rec.Note != "" {
			t.Errorf("gzip %v: record %+v", gz, rec)
		}
		h := rec.Request.Header
		if rec.Request.Body != `{"prompt":"hello"}` || h.Get("Anthropic-Version") != "2023-06-01" || h.Get("X-Api-Key") != "" || h.Get("Cookie") != "" {
			t.Errorf("gzip %v: request %+v", gz, rec.Request)
		}
		if rec.Response.Body != `{"reply":"hi"}` || rec.Response.Header.Get("Request-Id") != "req-1" {
			t.Errorf("gzip %v: response %+v", gz, rec.Response)
		}
	}
}

// TestRequestLogStream checks that a streamed response is recorded whole,
// and that the record and the client get it uncompressed even when the
// client asks for gzip.
func TestRequestLogStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out io.Writer = w
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			defer zw.Close()
			out = zw
		}
		io.WriteString(out, "data: first\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(out, "data: second\n\n")
	}))
	defer up.Close()
	b := newFakeBucket(t)
	_, gw := newLoggedGateway(t, b, false, up.URL)

	req := mustRequest(t, "GET", gw.URL+"/p/stream")
	req.Header.Set("Authorization", "Bearer alice-token")
	req.Header.Set("Accept-Encoding", "gzip, br")
	resp, body := do(t, req)
	const want = "data: first\n\ndata: second\n\n"
	if body != want || resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("client got %q, Content-Encoding %q", body, resp.Header.Get("Content-Encoding"))
	}
	if _, rec := b.next(t); rec.Response.Body != want || rec.Request.Header.Get("Accept-Encoding") != "gzip, br" {
		t.Errorf("record %+v", rec)
	}
}

func TestRequestLogBinaryBody(t *testing.T) {
	up := newUpstream(t)
	b := newFakeBucket(t)
	_, gw := newLoggedGateway(t, b, false, up.URL)

	req, _ := http.NewRequest("POST", gw.URL+"/p/x", bytes.NewReader([]byte{0x28, 0xb5, 0x2f, 0xfd, 0}))
	req.Header.Set("X-Api-Key", "alice-token")
	do(t, req)
	if _, rec := b.next(t); rec.Request.Body != "" || !bytes.Equal(rec.Request.BodyBase64, []byte{0x28, 0xb5, 0x2f, 0xfd, 0}) || rec.Response.Body != "ok" {
		t.Errorf("record %+v", rec)
	}
}

// TestRequestLogSkips checks that only forwarded requests are recorded.
func TestRequestLogSkips(t *testing.T) {
	up := newUpstream(t)
	b := newFakeBucket(t)
	_, gw := newLoggedGateway(t, b, false, up.URL)

	for _, path := range []string{"/healthz", "/p/no-token", "/elsewhere", "/p/../x", "/p/ok"} {
		req := mustRequest(t, "GET", gw.URL)
		req.URL.Opaque = path
		if path != "/p/no-token" {
			req.Header.Set("X-Api-Key", "alice-token")
		}
		do(t, req)
	}
	if _, rec := b.next(t); rec.URL != "/p/ok" {
		t.Errorf("recorded %s", rec.URL)
	}
}

// TestRequestLogAborted checks the record of a stream that the upstream
// broke off, from a gateway without clients.
func TestRequestLogAborted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer up.Close()
	b := newFakeBucket(t)
	_, gw := newLoggedGateway(t, b, false, up.URL, func(c *Config) { c.Clients = nil })

	resp, err := http.Get(gw.URL + "/p/stream")
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	o, rec := b.next(t)
	if !regexp.MustCompile(`^/bucket/pre%2Bfix/\d{4}-\d\d-\d\d/\d{6}\.\d{3}-[0-9a-f]{16}\.json$`).MatchString(o.path) ||
		rec.Client != "" || rec.Note != "aborted" || rec.Response.Body != "data: first\n\n" {
		t.Errorf("key %s, record %+v", o.path, rec)
	}
}

// TestRequestLogUpgrade checks that a connection the upstream upgraded is
// not recorded, while a request that only asks for an upgrade is.
func TestRequestLogUpgrade(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			io.WriteString(w, "ok")
			return
		}
		c, brw, _ := http.NewResponseController(w).Hijack()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\nhello")
		brw.Flush()
		c.Close()
	}))
	defer up.Close()
	b := newFakeBucket(t)
	_, gw := newLoggedGateway(t, b, false, up.URL)

	conn, err := net.Dial("tcp", strings.TrimPrefix(gw.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "GET /p/ws HTTP/1.1\r\nHost: x\r\nX-Api-Key: alice-token\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
	got, _ := io.ReadAll(conn)
	conn.Close()
	if !strings.HasSuffix(string(got), "\r\n\r\nhello") {
		t.Errorf("client got %q", got)
	}
	select {
	case o := <-b.objects:
		t.Errorf("upgraded connection recorded: %s", o.path)
	case <-time.After(500 * time.Millisecond):
	}

	req := mustRequest(t, "POST", gw.URL+"/p/x")
	req.Header.Set("X-Api-Key", "alice-token")
	req.Header.Set("Upgrade", "test")
	do(t, req)
	if _, rec := b.next(t); rec.URL != "/p/x" || rec.Response.Body != "ok" {
		t.Errorf("record %+v", rec)
	}
}

func TestRequestLogRetries(t *testing.T) {
	defer func(d []time.Duration) { uploadDelays = d }(uploadDelays)
	uploadDelays = []time.Duration{time.Millisecond, time.Millisecond}
	up := newUpstream(t)
	b := newFakeBucket(t)
	_, gw := newLoggedGateway(t, b, false, up.URL)
	nextLine := captureLog(t)
	request := func() {
		req := mustRequest(t, "GET", gw.URL+"/p/x")
		req.Header.Set("X-Api-Key", "alice-token")
		if resp, _ := do(t, req); resp.StatusCode != 200 {
			t.Errorf("status %d", resp.StatusCode)
		}
		nextLine() // the access log line
	}

	b.fail.Store(2)
	request()
	b.next(t)

	b.fail.Store(3)
	request()
	line := nextLine()
	if !regexp.MustCompile(`request log: alice/\S+\.json: upload failed 3 times, dropped; last error: 503 Service Unavailable \(SlowDown\)\n$`).MatchString(line) {
		t.Errorf("log %q", line)
	}
	select {
	case o := <-b.objects:
		t.Errorf("uploaded %s", o.path)
	default:
	}
}

func TestRequestLogWait(t *testing.T) {
	up := newUpstream(t)
	b := newFakeBucket(t)
	b.block = make(chan struct{})
	g, gw := newLoggedGateway(t, b, false, up.URL)

	req := mustRequest(t, "GET", gw.URL+"/p/x")
	req.Header.Set("X-Api-Key", "alice-token")
	do(t, req)
	gw.Close() // waits for the handler, as Shutdown does in main
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.log.wait(ctx); err == nil {
		t.Error("wait returned before the upload finished")
	}
	close(b.block)
	if err := g.log.wait(context.Background()); err != nil {
		t.Error(err)
	}
	select {
	case <-b.objects:
	default:
		t.Error("wait returned before the object was stored")
	}
}
