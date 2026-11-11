package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

func appKey(t *testing.T) *rsa.PrivateKey {
	testKeyOnce.Do(func() {
		var err error
		if testKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			t.Fatal(err)
		}
	})
	return testKey
}

// writeKey writes the test key in PEM, PKCS #1 as GitHub hands it out, or
// PKCS #8.
func writeKey(t *testing.T, pkcs8 bool) string {
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appKey(t))}
	if pkcs8 {
		der, _ := x509.MarshalPKCS8PrivateKey(appKey(t))
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	}
	p := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeGitHub creates installation tokens for requests with a valid JWT.
type fakeGitHub struct {
	*httptest.Server
	mu       sync.Mutex
	requests []tokenRequest
	lifetime time.Duration
	fail     string // if set, the error message of a 422
	status   int    // if set, the status of a success
}

type tokenRequest struct {
	Path, Body   string
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{lifetime: time.Hour}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := checkJWT(appKey(t), r.Header.Get("Authorization")); err != nil || r.Method != "POST" ||
			r.Header.Get("X-GitHub-Api-Version") == "" || r.Header.Get("User-Agent") == "" {
			http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
			return
		}
		var req tokenRequest
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		req.Path, req.Body = r.URL.Path, string(body)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		n, fail, lifetime, status := len(f.requests), f.fail, f.lifetime, f.status
		f.mu.Unlock()
		if fail != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprintf(w, `{"message":%q,"documentation_url":"https://docs.github.com"}`, fail)
			return
		}
		resp := map[string]any{
			"token":                fmt.Sprintf("ghs_token%d", n),
			"expires_at":           time.Now().Add(lifetime).UTC().Format(time.RFC3339),
			"permissions":          req.Permissions,
			"repository_selection": "selected",
		}
		var repos []map[string]any
		for _, name := range req.Repositories {
			repos = append(repos, map[string]any{"name": name, "full_name": "owner/" + name})
		}
		resp["repositories"] = repos
		if status == 0 {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeGitHub) last() tokenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

// checkJWT checks an "Authorization: Bearer <JWT>" header as GitHub would.
func checkJWT(key *rsa.PrivateKey, auth string) error {
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) != 3 {
		return fmt.Errorf("not a JWT")
	}
	enc := base64.RawURLEncoding
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		return err
	}
	header, _ := enc.DecodeString(parts[0])
	var h struct{ Alg string }
	json.Unmarshal(header, &h)
	payload, _ := enc.DecodeString(parts[1])
	var c struct {
		Iat, Exp int64
		Iss      string
	}
	json.Unmarshal(payload, &c)
	now := time.Now().Unix()
	// Dated back for clock drift, and valid for at most 10 minutes.
	if h.Alg != "RS256" || c.Iss != "Iv1.test" || now-c.Iat < 30 || c.Exp <= now || c.Exp-now > 600 {
		return fmt.Errorf("claims %+v, alg %s", c, h.Alg)
	}
	return nil
}

func newTestBroker(t *testing.T, gh *fakeGitHub) *httptest.Server {
	c := &Config{
		Clients: map[string]string{"alice": "alice-token", "bob": "bob-token", "carol": "carol-token", "dave": "dave-token"},
		GitHub: &GitHub{
			AppID:          "Iv1.test",
			PrivateKeyFile: writeKey(t, false),
			API:            gh.URL + "/",
			Access: map[string]GitHubAccess{
				"alice": {
					InstallationID: 42,
					Repositories:   []string{"hive", "Docs"},
					Permissions:    map[string]string{"contents": "write", "pull_requests": "write", "issues": "read"},
				},
				"carol": {InstallationID: 7},
				"dave":  {InstallationID: 42, Repositories: []string{"hive", "Docs"}, Permissions: map[string]string{"contents": "write", "pull_requests": "write", "issues": "read"}},
			},
		},
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	b, err := newBroker(c)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(b)
	t.Cleanup(s.Close)
	return s
}

func post(t *testing.T, s *httptest.Server, token, body string) (int, string) {
	req, _ := http.NewRequest("POST", s.URL+"/github/token", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 200 && resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", resp.Header.Get("Cache-Control"))
	}
	return resp.StatusCode, string(b)
}

func decode(t *testing.T, body string) installationToken {
	var tok installationToken
	if err := json.Unmarshal([]byte(body), &tok); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return tok
}

func TestIssuesToken(t *testing.T) {
	gh := newFakeGitHub(t)
	s := newTestBroker(t, gh)

	status, body := post(t, s, "alice-token", "")
	tok := decode(t, body)
	if status != 200 || tok.Token != "ghs_token1" || time.Until(tok.ExpiresAt) < 59*time.Minute ||
		strings.Join(tok.Repositories, ",") != "hive,Docs" || tok.Permissions["contents"] != "write" {
		t.Fatalf("status %d, %s", status, body)
	}
	req := gh.last()
	if req.Path != "/app/installations/42/access_tokens" || strings.Join(req.Repositories, ",") != "hive,Docs" || len(req.Permissions) != 3 {
		t.Errorf("GitHub got %+v", req)
	}

	// The same request gets the same token, from the cache.
	if _, body := post(t, s, "alice-token", ""); decode(t, body).Token != "ghs_token1" || gh.count() != 1 {
		t.Errorf("not reused: %s, %d requests", body, gh.count())
	}

	// A narrower request gets a token of its own.
	_, body = post(t, s, "alice-token", `{"repositories": ["docs"], "permissions": {"contents": "read"}}`)
	if tok := decode(t, body); tok.Token != "ghs_token2" {
		t.Errorf("got %s", body)
	}
	if req := gh.last(); strings.Join(req.Repositories, ",") != "docs" || len(req.Permissions) != 1 || req.Permissions["contents"] != "read" {
		t.Errorf("GitHub got %+v", req)
	}

	// Each part of a request, and each client, gets a token of its own.
	for i, tc := range []struct{ token, body string }{
		{"alice-token", `{"permissions": {"contents": "read"}}`},
		{"alice-token", `{"repositories": ["docs"]}`},
		{"dave-token", ""},
		{"alice-token", `{"permissions": {"issues": "read"}}`}, // the level the access allows
	} {
		want := fmt.Sprintf("ghs_token%d", i+3)
		if _, body := post(t, s, tc.token, tc.body); decode(t, body).Token != want {
			t.Errorf("%s %s: got %s, want %s", tc.token, tc.body, body, want)
		}
	}

	// A client without limits gets the whole installation, or asks for less.
	_, body = post(t, s, "carol-token", "")
	if req := gh.last(); decode(t, body).Token != "ghs_token7" || req.Path != "/app/installations/7/access_tokens" || req.Body != "{}" {
		t.Errorf("GitHub got %+v", req)
	}
	status, _ = post(t, s, "carol-token", `{"repositories": ["any"], "permissions": {"issues": "write"}}`)
	if req := gh.last(); status != 200 || strings.Join(req.Repositories, ",") != "any" || req.Permissions["issues"] != "write" {
		t.Errorf("status %d, GitHub got %+v", status, req)
	}
}

func TestRefusals(t *testing.T) {
	gh := newFakeGitHub(t)
	s := newTestBroker(t, gh)

	for _, tc := range []struct {
		name, token, body string
		want              int
	}{
		{"no token", "", "", 401},
		{"wrong token", "nope", "", 401},
		{"no access", "bob-token", "", 403},
		{"other repository", "alice-token", `{"repositories": ["secret"]}`, 403},
		{"empty repositories", "alice-token", `{"repositories": []}`, 400},
		{"empty permissions", "carol-token", `{"permissions": {}}`, 400},
		{"too large", "alice-token", `{"repositories": ["` + strings.Repeat("a", 64<<10) + `"]}`, 413},
		{"higher level", "alice-token", `{"permissions": {"issues": "write"}}`, 403},
		{"other permission", "alice-token", `{"permissions": {"administration": "read"}}`, 403},
		{"unknown level", "alice-token", `{"permissions": {"contents": "all"}}`, 400},
		{"bad repository name", "alice-token", `{"repositories": ["owner/hive"]}`, 400},
		{"unknown field", "alice-token", `{"repository": ["hive"]}`, 400},
		{"not JSON", "alice-token", `hive`, 400},
		{"trailing data", "alice-token", `{"repositories": ["hive"]} x`, 400},
		{"trailing brace", "alice-token", `{}}`, 400},
		{"second object", "alice-token", `{} {}`, 400},
		{"listed twice", "alice-token", `{"repositories": ["hive", "HIVE"]}`, 400},
	} {
		if status, body := post(t, s, tc.token, tc.body); status != tc.want {
			t.Errorf("%s: status %d, want %d: %s", tc.name, status, tc.want, body)
		}
	}
	if gh.count() != 0 {
		t.Errorf("%d requests reached GitHub", gh.count())
	}

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/github/token", 405},
		{"POST", "/github", 404},
		{"GET", "/healthz", 200},
	} {
		req, _ := http.NewRequest(tc.method, s.URL+tc.path, nil)
		if tc.path != "/healthz" {
			req.Header.Set("X-Api-Key", "alice-token")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: status %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
}

func TestGitHubError(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.fail = "There is at least one repository that does not exist"
	s := newTestBroker(t, gh)

	status, body := post(t, s, "alice-token", "")
	if status != 502 || !strings.Contains(body, "github: 422 Unprocessable Entity: There is at least one repository") {
		t.Errorf("status %d, %s", status, body)
	}
	gh.fail = ""
	if _, body := post(t, s, "alice-token", ""); decode(t, body).Token != "ghs_token2" {
		t.Errorf("after an error: %s", body)
	}
}

// TestConcurrent checks that parallel requests share the cache safely.
func TestConcurrent(t *testing.T) {
	gh := newFakeGitHub(t)
	s := newTestBroker(t, gh)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			// Not post, which may call t.Fatal outside the test's goroutine.
			req, _ := http.NewRequest("POST", s.URL+"/github/token", nil)
			req.Header.Set("X-Api-Key", []string{"alice-token", "carol-token", "dave-token"}[i%3])
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("status %d", resp.StatusCode)
			}
		})
	}
	wg.Wait()
}

func TestNotCreated(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.status = http.StatusOK
	s := newTestBroker(t, gh)

	if status, body := post(t, s, "alice-token", ""); status != 502 || !strings.Contains(body, "200 OK") {
		t.Errorf("status %d, %s", status, body)
	}
}

// TestStoreDropsExpiring checks that the cache keeps only reusable tokens.
func TestStoreDropsExpiring(t *testing.T) {
	b := &broker{cache: map[string]*installationToken{}}
	b.store("old", &installationToken{ExpiresAt: time.Now().Add(reuseFor - time.Second)})
	b.store("fresh", &installationToken{ExpiresAt: time.Now().Add(time.Hour)})
	b.store("new", &installationToken{ExpiresAt: time.Now().Add(time.Hour)})
	if len(b.cache) != 2 || b.cache["old"] != nil {
		t.Errorf("cache %v", b.cache)
	}
}

// TestShortLivedNotReused checks that a token about to expire is replaced.
func TestShortLivedNotReused(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.lifetime = 5 * time.Minute
	s := newTestBroker(t, gh)

	post(t, s, "alice-token", "")
	if _, body := post(t, s, "alice-token", ""); decode(t, body).Token != "ghs_token2" {
		t.Errorf("reused a token that expires within %s: %s", reuseFor, body)
	}
}

func TestPKCS8Key(t *testing.T) {
	gh := newFakeGitHub(t)
	app, err := newGitHubApp(&GitHub{AppID: "Iv1.test", PrivateKeyFile: writeKey(t, true), API: gh.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.token(t.Context(), 1, nil, nil); err != nil {
		t.Error(err)
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
	key := writeKey(t, false)
	t.Setenv("TEST_KEY", key)

	c, err := loadConfig(write(`{"clients": {"a": "${TEST_TOKEN}"}, "github": {"app_id": "Iv1.${TEST_TOKEN}", "private_key_file": "${TEST_KEY}",
		"access": {"a": {"installation_id": 1, "repositories": ["${TEST_TOKEN}"], "permissions": {"contents": "read"}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	g := c.GitHub
	if c.Listen != ":8080" || c.Clients["a"] != "sekrit" || g.AppID != "Iv1.sekrit" || g.PrivateKeyFile != key || g.API != "https://api.github.com" || g.Access["a"].Repositories[0] != "sekrit" {
		t.Errorf("got %+v, %+v", c, g)
	}

	const gh = `"github": {"app_id": "1", "private_key_file": "k"}`
	for name, tc := range map[string]struct{ config, wantErr string }{
		"unset":          {`{"clients": {"a": "${TEST_UNSET}"}, ` + gh + `}`, "TEST_UNSET"},
		"empty":          {`{"clients": {"a": "${TEST_EMPTY}"}, ` + gh + `}`, "TEST_EMPTY"},
		"no clients":     {`{` + gh + `}`, "no clients"},
		"client name":    {`{"clients": {"-": "x"}, ` + gh + `}`, "a name must"},
		"same token":     {`{"clients": {"a": "${TEST_TOKEN}", "b": "${TEST_TOKEN}"}, ` + gh + `}`, "same token"},
		"token newline":  {`{"clients": {"a": "${TEST_NEWLINE}"}, ` + gh + `}`, "control character"},
		"no github":      {`{"clients": {"a": "x"}}`, "github is not set"},
		"no app":         {`{"clients": {"a": "x"}, "github": {"private_key_file": "k"}}`, "app_id and private_key_file"},
		"app newline":    {`{"clients": {"a": "x"}, "github": {"app_id": "${TEST_NEWLINE}", "private_key_file": "k"}}`, "app_id contains a control character"},
		"api url":        {`{"clients": {"a": "x"}, "github": {"app_id": "1", "private_key_file": "k", "api_url": "api.github.com"}}`, "api_url"},
		"unknown client": {`{"clients": {"a": "x"}, "github": {"app_id": "1", "private_key_file": "k", "access": {"b": {"installation_id": 1}}}}`, "unknown client"},
		"installation":   {`{"clients": {"a": "x"}, "github": {"app_id": "1", "private_key_file": "k", "access": {"a": {}}}}`, "installation_id"},
		"no repository":  {`{"clients": {"a": "x"}, "github": {"app_id": "1", "private_key_file": "k", "access": {"a": {"installation_id": 1, "repositories": []}}}}`, "repositories is empty"},
		"no permission":  {`{"clients": {"a": "x"}, "github": {"app_id": "1", "private_key_file": "k", "access": {"a": {"installation_id": 1, "permissions": {}}}}}`, "permissions is empty"},
		"level":          {`{"clients": {"a": "x"}, "github": {"app_id": "1", "private_key_file": "k", "access": {"a": {"installation_id": 1, "permissions": {"contents": "${TEST_TOKEN}"}}}}}`, "level must be"},
		"repository":     {`{"clients": {"a": "x"}, "github": {"app_id": "1", "private_key_file": "k", "access": {"a": {"installation_id": 1, "repositories": ["o/r"]}}}}`, "repository name"},
		"unknown field":  {`{"clients": {"a": "x"}, "github": {"app_id": "1", "private_key": "k"}}`, "unknown field"},
	} {
		_, err := loadConfig(write(tc.config))
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err %v, want %q", name, err, tc.wantErr)
		}
		if err != nil && strings.Contains(err.Error(), "sekrit") {
			t.Errorf("%s: error shows a secret: %v", name, err)
		}
	}

	// A file that is not a key is refused without quoting it.
	notKey := filepath.Join(t.TempDir(), "not-a-key.pem")
	os.WriteFile(notKey, []byte("-----BEGIN RSA PRIVATE KEY-----\nc2Vrcml0\n-----END RSA PRIVATE KEY-----\n"), 0o600)
	if _, err := newGitHubApp(&GitHub{AppID: "1", PrivateKeyFile: notKey}); err == nil || strings.Contains(err.Error(), "c2Vrcml0") {
		t.Errorf("err %v", err)
	}
}

func TestAccessLog(t *testing.T) {
	lines := make(chan string, 10)
	log.SetOutput(lineWriter(lines))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s := newTestBroker(t, newFakeGitHub(t))

	for token, want := range map[string]string{"alice-token": " alice POST /github/token 200 ", "nope": " - POST /github/token 401 "} {
		post(t, s, token, "")
		select {
		case line := <-lines:
			if !strings.Contains(line, want) {
				t.Errorf("log %q, want %q", line, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no log line")
		}
	}
}

type lineWriter chan string

func (w lineWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}
