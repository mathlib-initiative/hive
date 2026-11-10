package main

import (
	"bufio"
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"time"
)

// clientCredentials are removed from every request before it is forwarded:
// the gateway owns the credentials, and whatever a client sends (a dummy
// key, the gateway token) must not reach the upstream. ReverseProxy itself
// removes hop-by-hop headers such as Proxy-Authorization, and Forwarded and
// X-Forwarded-*.
var clientCredentials = []string{"Authorization", "X-Api-Key", "Cookie"}

type gateway struct {
	clients []client
	routes  []route     // longest prefix first
	log     *requestLog // nil without a request log
}

type client struct {
	name  string
	token []byte
}

type route struct {
	prefix string
	proxy  *httputil.ReverseProxy
}

func newGateway(c *Config) *gateway {
	g := &gateway{}
	for name, token := range c.Clients {
		g.clients = append(g.clients, client{name: name, token: []byte(token)})
	}
	if c.RequestLog != nil {
		g.log = &requestLog{bucket: newBucket(c.RequestLog), gzip: c.RequestLog.Gzip}
	}
	for prefix, r := range c.Routes {
		g.routes = append(g.routes, route{prefix: prefix, proxy: newProxy(prefix, r, g.log != nil)})
	}
	sort.Slice(g.routes, func(i, j int) bool { return len(g.routes[i].prefix) > len(g.routes[j].prefix) })
	return g
}

func newProxy(prefix string, r Route, logged bool) *httputil.ReverseProxy {
	upstream, _ := url.Parse(r.Upstream) // checked by Config.validate
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
			pr.Out.URL.RawPath = ""
			pr.SetURL(upstream)
			for _, h := range clientCredentials {
				pr.Out.Header.Del(h)
			}
			for k, v := range r.Headers {
				pr.Out.Header.Set(k, v)
			}
			if logged {
				// The request log keeps response bodies readable. Without
				// the client's Accept-Encoding, the transport asks for gzip
				// itself and passes the response on decompressed.
				pr.Out.Header.Del("Accept-Encoding")
			}
		},
		// Inference responses are often streamed; pass every chunk on at once.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			// Transport errors name the upstream host but not the path, where
			// a secret may be. Clients only learn that the request failed.
			log.Printf("%s %s: upstream request failed: %v", req.Method, prefix, err)
			http.Error(w, "upstream request failed", http.StatusBadGateway)
		},
	}
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	lw := &loggingWriter{ResponseWriter: w, status: http.StatusOK}
	finished := false
	// Deferred, so that a request is logged even when serve panics. The
	// proxy does that, with http.ErrAbortHandler, when a streamed response
	// breaks off.
	defer func() {
		note, client, suffix := "", lw.client, ""
		switch {
		case r.Context().Err() != nil:
			note = "client went away"
		case !finished:
			note = "aborted"
		}
		if client == "" {
			client = "-"
		}
		if note != "" {
			suffix = " (" + note + ")"
		}
		log.Printf("%s %s %s %d %s%s", client, r.Method, r.URL.EscapedPath(), lw.status, time.Since(start).Round(time.Millisecond), suffix)
		if lw.capture != nil && !lw.hijacked {
			g.log.add(start, r, lw, note)
		}
	}()
	g.serve(lw, r)
	finished = true
}

func (g *gateway) serve(w *loggingWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if len(g.clients) > 0 {
		name, ok := g.authenticate(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.client = name
	}
	// A ".." segment could make the upstream resolve the request outside the
	// route's base path, with the route's credentials attached. Some servers
	// treat a backslash as a slash.
	if strings.Contains(r.URL.Path+"/", "/../") || strings.Contains(r.URL.Path, `\`) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	for _, rt := range g.routes {
		if r.URL.Path == rt.prefix || strings.HasPrefix(r.URL.Path, rt.prefix+"/") {
			if g.log != nil {
				w.capture, r = newCapture(r)
			}
			rt.proxy.ServeHTTP(w, r)
			return
		}
	}
	http.NotFound(w, r)
}

// authenticate returns the name of the client whose token r carries, and
// false if r carries none of the tokens. Every token is compared, in
// constant time.
func (g *gateway) authenticate(r *http.Request) (name string, ok bool) {
	got := r.Header.Get("X-Api-Key")
	if auth := r.Header.Get("Authorization"); len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		got = auth[7:]
	}
	token := []byte(got)
	for _, c := range g.clients {
		if subtle.ConstantTimeCompare(token, c.token) == 1 {
			name, ok = c.name, true
		}
	}
	return name, ok
}

// loggingWriter records the client and the response status for the access
// log, and with a capture the response body for the request log. Unwrap
// lets the proxy flush streamed responses through it.
type loggingWriter struct {
	http.ResponseWriter
	client   string
	status   int
	capture  *capture
	hijacked bool
}

func (w *loggingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingWriter) Write(p []byte) (int, error) {
	if w.capture != nil {
		w.capture.response.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *loggingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack lets the proxy hand an upgraded connection, such as a WebSocket,
// to the client and the upstream. Its traffic is not recorded.
func (w *loggingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	w.hijacked = err == nil
	return c, rw, err
}
