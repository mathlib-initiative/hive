package main

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// upstream records the last request it received.
type upstream struct {
	*httptest.Server
	last *http.Request
	body string
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.last, u.body = r, string(b)
		io.WriteString(w, "ok")
	}))
	t.Cleanup(u.Close)
	return u
}

func newTestGateway(t *testing.T, c *Config) *httptest.Server {
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(newGateway(c))
	t.Cleanup(s.Close)
	return s
}

func do(t *testing.T, req *http.Request) (*http.Response, string) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestForwardsBelowPrefix(t *testing.T) {
	up := newUpstream(t)
	gw := newTestGateway(t, &Config{Routes: map[string]Route{
		"/anthropic": {Upstream: up.URL + "/v1/acct/gw/anthropic"},
	}})

	req, _ := http.NewRequest("POST", gw.URL+"/anthropic/v1/messages?beta=true", strings.NewReader(`{"model":"m"}`))
	resp, _ := do(t, req)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := up.last.URL.Path; got != "/v1/acct/gw/anthropic/v1/messages" {
		t.Errorf("path %q", got)
	}
	if got := up.last.URL.RawQuery; got != "beta=true" {
		t.Errorf("query %q", got)
	}
	if up.body != `{"model":"m"}` {
		t.Errorf("body %q", up.body)
	}
}

func TestReplacesClientCredentials(t *testing.T) {
	up := newUpstream(t)
	gw := newTestGateway(t, &Config{Routes: map[string]Route{
		"/p": {Upstream: up.URL, Headers: map[string]string{"X-Api-Key": "real-key", "X-Extra": "set"}},
	}})

	req, _ := http.NewRequest("POST", gw.URL+"/p/v1", nil)
	req.Header.Set("Authorization", "Bearer client")
	req.Header.Set("X-Api-Key", "client")
	req.Header.Set("Cookie", "c=1")
	req.Header.Set("Proxy-Authorization", "Basic client")
	req.Header.Set("Forwarded", "for=10.0.0.1")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("X-Extra", "client")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	do(t, req)

	h := up.last.Header
	for name, want := range map[string]string{
		"Authorization":       "",
		"Proxy-Authorization": "",
		"Cookie":              "",
		"Forwarded":           "",
		"X-Forwarded-For":     "",
		"X-Api-Key":           "real-key",
		"X-Extra":             "set",
		"Anthropic-Version":   "2023-06-01",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestClients(t *testing.T) {
	nextLine := captureLog(t)
	up := newUpstream(t)
	gw := newTestGateway(t, &Config{
		Clients: map[string]string{"alice": "alice-token", "bob": "bob-token"},
		Routes:  map[string]Route{"/p": {Upstream: up.URL}},
	})

	for _, tc := range []struct {
		name, header, value string
		want                int
		client              string
	}{
		{"none", "", "", 401, "-"},
		{"wrong", "X-Api-Key", "nope", 401, "-"},
		{"part of a token", "X-Api-Key", "alice", 401, "-"},
		{"x-api-key", "X-Api-Key", "alice-token", 200, "alice"},
		{"bearer", "Authorization", "Bearer bob-token", 200, "bob"},
		{"bearer lower case", "Authorization", "bearer alice-token", 200, "alice"},
		{"basic", "Authorization", "Basic alice-token", 401, "-"},
	} {
		up.last = nil
		req, _ := http.NewRequest("GET", gw.URL+"/p/x", nil)
		if tc.header != "" {
			req.Header.Set(tc.header, tc.value)
		}
		resp, _ := do(t, req)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
		if tc.want == 200 && (up.last.Header.Get("X-Api-Key") != "" || up.last.Header.Get("Authorization") != "") {
			t.Errorf("%s: token forwarded upstream", tc.name)
		}
		if line := nextLine(); !strings.Contains(line, " "+tc.client+" GET /p/x ") {
			t.Errorf("%s: log %q, want client %s", tc.name, line, tc.client)
		}
	}

	resp, _ := do(t, mustRequest(t, "GET", gw.URL+"/healthz"))
	if resp.StatusCode != 200 {
		t.Errorf("healthz without token: %d", resp.StatusCode)
	}
}

func TestPrefixMatching(t *testing.T) {
	short, long := newUpstream(t), newUpstream(t)
	gw := newTestGateway(t, &Config{Routes: map[string]Route{
		"/a":   {Upstream: short.URL},
		"/a/b": {Upstream: long.URL},
	}})

	for path, want := range map[string]*upstream{"/a/x": short, "/a/b/x": long, "/a": short, "/ab": nil, "/b": nil} {
		short.last, long.last = nil, nil
		resp, _ := do(t, mustRequest(t, "GET", gw.URL+path))
		switch {
		case want == nil && resp.StatusCode != 404:
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		case want != nil && want.last == nil:
			t.Errorf("%s: went to the wrong upstream", path)
		}
	}
}

func TestRejectsDotDot(t *testing.T) {
	up := newUpstream(t)
	gw := newTestGateway(t, &Config{Routes: map[string]Route{"/p": {Upstream: up.URL + "/base"}}})

	for _, path := range []string{"/p/../x", "/p/a/..", "/p/%2e%2e/x", "/p/..%5Cx"} {
		up.last = nil
		req := mustRequest(t, "GET", gw.URL)
		req.URL.Opaque = path // send the path exactly as written
		resp, _ := do(t, req)
		if resp.StatusCode != 400 || up.last != nil {
			t.Errorf("%s: status %d, forwarded %v", path, resp.StatusCode, up.last != nil)
		}
	}
}

func TestLogEscapesPath(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	gw := newTestGateway(t, &Config{Clients: map[string]string{"a": "secret"}, Routes: map[string]Route{"/p": {Upstream: "https://example.com"}}})

	do(t, mustRequest(t, "GET", gw.URL+"/x%0A2026/11/10%2000:00:00%20GET%20/p%20200"))
	if lines := strings.Count(buf.String(), "\n"); lines != 1 || strings.HasSuffix(buf.String(), ")\n") {
		t.Errorf("log %q, want one line without a note", buf.String())
	}
}

func TestStreams(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: second\n\n")
	}))
	defer up.Close()
	defer close(release)
	gw := newTestGateway(t, &Config{Routes: map[string]Route{"/p": {Upstream: up.URL}}})

	resp, err := http.Get(gw.URL + "/p/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line := make(chan string)
	go func() {
		s, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if s != "data: first\n" {
			t.Errorf("got %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first event not passed on before the stream ended")
	}
}

// TestLogsCutOffStreams checks that a streamed response that ends early is
// logged, with a note on which side ended it.
func TestLogsCutOffStreams(t *testing.T) {
	defer log.SetOutput(os.Stderr)
	for _, tc := range []struct {
		upstreamBreaks bool
		note           string
	}{
		{false, " (client went away)"},
		{true, " (aborted)"},
	} {
		var buf bytes.Buffer
		log.SetOutput(&buf)
		release := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			if tc.upstreamBreaks {
				panic(http.ErrAbortHandler)
			}
			<-release
		}))
		gw := newTestGateway(t, &Config{Clients: map[string]string{"a": "t"}, Routes: map[string]Route{"/p": {Upstream: up.URL}}})

		req := mustRequest(t, "GET", gw.URL+"/p/stream")
		req.Header.Set("X-Api-Key", "t")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if tc.upstreamBreaks {
			// The client must see the stream break off, not end.
			if _, err := io.ReadAll(resp.Body); err == nil {
				t.Error("cut-off stream ended cleanly")
			}
		} else {
			bufio.NewReader(resp.Body).ReadString('\n')
		}
		resp.Body.Close()
		gw.Close() // waits until the request is logged
		close(release)
		up.Close()
		if !strings.HasSuffix(buf.String(), tc.note+"\n") || !strings.Contains(buf.String(), " a GET /p/stream 200 ") {
			t.Errorf("upstream breaks %v: log %q", tc.upstreamBreaks, buf.String())
		}
	}
}

func TestUpstreamDown(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	up.Close()
	gw := newTestGateway(t, &Config{Routes: map[string]Route{"/p": {Upstream: up.URL + "/key-in-path"}}})

	resp, body := do(t, mustRequest(t, "GET", gw.URL+"/p/x"))
	if resp.StatusCode != 502 || strings.Contains(body, "key-in-path") {
		t.Errorf("status %d body %q", resp.StatusCode, body)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("TEST_TOKEN", "sekrit")
	t.Setenv("TEST_EMPTY", "")
	t.Setenv("TEST_NEWLINE", "sekrit\n")
	write := func(s string) string {
		p := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}

	c, err := loadConfig(write(`{"clients": {"a.b_c-1": "${TEST_TOKEN}"}, "routes": {"/p": {"upstream": "https://example.com/${TEST_TOKEN}", "headers": {"A": "Bearer ${TEST_TOKEN}", "B": "a$TEST_TOKEN${}b${c"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	h := c.Routes["/p"].Headers
	if c.Listen != ":8080" || c.Clients["a.b_c-1"] != "sekrit" || c.Routes["/p"].Upstream != "https://example.com/sekrit" || h["A"] != "Bearer sekrit" || h["B"] != "a$TEST_TOKEN${}b${c" {
		t.Errorf("got %+v", c)
	}

	const routes = `"routes": {"/p": {"upstream": "https://example.com"}}`
	for name, tc := range map[string]struct{ config, wantErr string }{
		"unset":         {`{"routes": {"/p": {"upstream": "https://example.com", "headers": {"A": "${TEST_UNSET}"}}}}`, "TEST_UNSET"},
		"empty":         {`{"clients": {"a": "${TEST_EMPTY}"}, ` + routes + `}`, "TEST_EMPTY"},
		"no routes":     {`{}`, "no routes"},
		"prefix":        {`{"routes": {"/p/": {"upstream": "https://example.com"}}}`, "prefix"},
		"upstream":      {`{"routes": {"/p": {"upstream": "example.com/${TEST_TOKEN}"}}}`, "upstream"},
		"unknown":       {`{"route": {}}`, "unknown field"},
		"newline":       {`{"routes": {"/p": {"upstream": "https://example.com", "headers": {"A": "${TEST_NEWLINE}"}}}}`, "control character"},
		"name":          {`{"routes": {"/p": {"upstream": "https://example.com", "headers": {"X(Key)": "x"}}}}`, "header name"},
		"client slash":  {`{"clients": {"a/b": "x"}, ` + routes + `}`, "a name must"},
		"client dots":   {`{"clients": {"..": "x"}, ` + routes + `}`, "a name must"},
		"client dash":   {`{"clients": {"-": "x"}, ` + routes + `}`, "a name must"},
		"no token":      {`{"clients": {"a": ""}, ` + routes + `}`, "empty token"},
		"token newline": {`{"clients": {"a": "${TEST_NEWLINE}"}, ` + routes + `}`, "control character"},
		"same token":    {`{"clients": {"a": "${TEST_TOKEN}", "b": "${TEST_TOKEN}"}, ` + routes + `}`, "same token"},
	} {
		_, err := loadConfig(write(tc.config))
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err %v, want %q", name, err, tc.wantErr)
		}
		if err != nil && strings.Contains(err.Error(), "sekrit") {
			t.Errorf("%s: error shows a secret: %v", name, err)
		}
	}
}

// captureLog captures the standard logger's output and returns a function
// that waits for its next line. The gateway logs a request after its
// response may have reached the client, so tests wait for the line rather
// than read a buffer.
func captureLog(t *testing.T) func() string {
	lines := make(chan string, 100)
	log.SetOutput(lineWriter(lines))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return func() string {
		t.Helper()
		select {
		case line := <-lines:
			return line
		case <-time.After(5 * time.Second):
			t.Fatal("no log line")
			return ""
		}
	}
}

type lineWriter chan string

func (w lineWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

func mustRequest(t *testing.T, method, url string) *http.Request {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
