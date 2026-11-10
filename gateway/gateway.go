package main

import (
	"crypto/subtle"
	"log"
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
	token  string
	routes []route // longest prefix first
}

type route struct {
	prefix string
	proxy  *httputil.ReverseProxy
}

func newGateway(c *Config) *gateway {
	g := &gateway{token: c.Token}
	for prefix, r := range c.Routes {
		g.routes = append(g.routes, route{prefix: prefix, proxy: newProxy(prefix, r)})
	}
	sort.Slice(g.routes, func(i, j int) bool { return len(g.routes[i].prefix) > len(g.routes[j].prefix) })
	return g
}

func newProxy(prefix string, r Route) *httputil.ReverseProxy {
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
		note := ""
		switch {
		case r.Context().Err() != nil:
			note = " (client went away)"
		case !finished:
			note = " (aborted)"
		}
		log.Printf("%s %s %d %s%s", r.Method, r.URL.EscapedPath(), lw.status, time.Since(start).Round(time.Millisecond), note)
	}()
	g.serve(lw, r)
	finished = true
}

func (g *gateway) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if g.token != "" && !g.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
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
			rt.proxy.ServeHTTP(w, r)
			return
		}
	}
	http.NotFound(w, r)
}

func (g *gateway) authorized(r *http.Request) bool {
	got := r.Header.Get("X-Api-Key")
	if auth := r.Header.Get("Authorization"); len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		got = auth[7:]
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(g.token)) == 1
}

// loggingWriter records the response status for the access log. Unwrap lets
// the proxy flush streamed responses through it.
type loggingWriter struct {
	http.ResponseWriter
	status int
}

func (w *loggingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
