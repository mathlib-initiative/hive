package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// reuseFor is how long a cached token must still be valid to be handed out
// again. GitHub's tokens last an hour, so a client gets a token that lasts
// between 10 and 60 minutes.
const reuseFor = 10 * time.Minute

type broker struct {
	clients []client
	github  *githubApp
	access  map[string]GitHubAccess

	mu    sync.Mutex
	cache map[string]*installationToken
}

type client struct {
	name  string
	token []byte
}

func newBroker(c *Config) (*broker, error) {
	app, err := newGitHubApp(c.GitHub)
	if err != nil {
		return nil, err
	}
	b := &broker{github: app, access: c.GitHub.Access, cache: map[string]*installationToken{}}
	for name, token := range c.Clients {
		b.clients = append(b.clients, client{name: name, token: []byte(token)})
	}
	return b, nil
}

func (b *broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	lw := &loggingWriter{ResponseWriter: w, client: "-", status: http.StatusOK}
	defer func() {
		log.Printf("%s %s %s %d %s", lw.client, r.Method, r.URL.EscapedPath(), lw.status, time.Since(start).Round(time.Millisecond))
	}()
	b.serve(lw, r)
}

func (b *broker) serve(w *loggingWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	name, ok := b.authenticate(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.client = name
	if r.URL.Path != "/github/token" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	b.githubToken(w, r, name)
}

// githubToken answers POST /github/token. The optional JSON body asks for
// less than the client's access allows.
func (b *broker) githubToken(w http.ResponseWriter, r *http.Request, name string) {
	access, ok := b.access[name]
	if !ok {
		http.Error(w, "no GitHub access for this client", http.StatusForbidden)
		return
	}
	var req struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid request body: data after the JSON object", http.StatusBadRequest)
		return
	}
	if err := checkRequest(req.Repositories, req.Permissions); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	repos, perms, err := narrow(access, req.Repositories, req.Permissions)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	key := cacheKey(name, repos, perms)
	b.mu.Lock()
	t := b.cache[key]
	b.mu.Unlock()
	if t == nil || time.Until(t.ExpiresAt) < reuseFor {
		t, err = b.github.token(r.Context(), access.InstallationID, repos, perms)
		if err != nil {
			log.Printf("%s: github: %v", name, err)
			http.Error(w, "github: "+err.Error(), http.StatusBadGateway)
			return
		}
		b.store(key, t)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(t)
}

// narrow returns what a token is created for: what the client asked for,
// if its access allows that, or else its whole access.
func narrow(a GitHubAccess, repos []string, perms map[string]string) ([]string, map[string]string, error) {
	if len(repos) == 0 {
		repos = a.Repositories
	} else if len(a.Repositories) > 0 {
		for _, repo := range repos {
			if !slices.ContainsFunc(a.Repositories, func(s string) bool { return strings.EqualFold(s, repo) }) {
				return nil, nil, fmt.Errorf("repository %q is not allowed", repo)
			}
		}
	}
	if len(perms) == 0 {
		perms = a.Permissions
	} else if len(a.Permissions) > 0 {
		for p, level := range perms {
			if levels[level] > levels[a.Permissions[p]] {
				return nil, nil, fmt.Errorf("permission %q at level %q is not allowed", p, level)
			}
		}
	}
	return repos, perms, nil
}

// cacheKey names a client's request. The client's name fixes the
// installation.
func cacheKey(name string, repos []string, perms map[string]string) string {
	r := make([]string, len(repos))
	for i, repo := range repos {
		r[i] = strings.ToLower(repo)
	}
	slices.Sort(r)
	p := make([]string, 0, len(perms))
	for k, v := range perms {
		p = append(p, k+":"+v)
	}
	slices.Sort(p)
	return name + " " + strings.Join(r, ",") + " " + strings.Join(p, ",")
}

// store caches a token and drops the ones that can no longer be reused.
func (b *broker) store(key string, t *installationToken) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, old := range b.cache {
		if time.Until(old.ExpiresAt) < reuseFor {
			delete(b.cache, k)
		}
	}
	b.cache[key] = t
}

// authenticate returns the name of the client whose token r carries, and
// false if r carries none of the tokens. Every token is compared, in
// constant time.
func (b *broker) authenticate(r *http.Request) (name string, ok bool) {
	got := r.Header.Get("X-Api-Key")
	if auth := r.Header.Get("Authorization"); len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		got = auth[7:]
	}
	token := []byte(got)
	for _, c := range b.clients {
		if subtle.ConstantTimeCompare(token, c.token) == 1 {
			name, ok = c.name, true
		}
	}
	return name, ok
}

// loggingWriter records the client and the response status for the access
// log.
type loggingWriter struct {
	http.ResponseWriter
	client string
	status int
}

func (w *loggingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
