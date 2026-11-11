package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// githubApp creates installation tokens for one GitHub App.
type githubApp struct {
	api    string
	appID  string
	key    *rsa.PrivateKey
	client *http.Client
}

// installationToken is GitHub's answer, cut to what clients need.
type installationToken struct {
	Token        string            `json:"token"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Permissions  map[string]string `json:"permissions"`
	Repositories []string          `json:"repositories,omitempty"`
}

func newGitHubApp(g *GitHub) (*githubApp, error) {
	data, err := os.ReadFile(g.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("github: private key: %w", err)
	}
	key, err := parseRSAKey(data)
	if err != nil {
		return nil, fmt.Errorf("github: private key %s: %w", g.PrivateKeyFile, err)
	}
	return &githubApp{api: g.API, appID: g.AppID, key: key, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

// parseRSAKey reads an RSA private key in PEM, in PKCS #1 as GitHub hands
// it out, or in PKCS #8. Errors never quote the key.
func parseRSAKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("not an RSA private key in PKCS #1 or PKCS #8")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key")
	}
	return rk, nil
}

// jwt returns a JSON Web Token that authenticates as the App for nine
// minutes. It is dated a minute back, as GitHub advises, in case of clock
// drift.
func (a *githubApp) jwt(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.appID,
	})
	signed := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

// token creates an installation token. Empty repos or perms leave the
// token with everything the installation has.
func (a *githubApp) token(ctx context.Context, installationID int64, repos []string, perms map[string]string) (*installationToken, error) {
	jwt, err := a.jwt(time.Now())
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(struct {
		Repositories []string          `json:"repositories,omitempty"`
		Permissions  map[string]string `json:"permissions,omitempty"`
	}{repos, perms})
	u := a.api + "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "hive-broker")

	resp, err := a.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is in the config; keep the error short
		}
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated {
		var e struct{ Message string }
		json.Unmarshal(data, &e)
		if e.Message == "" {
			return nil, errors.New(resp.Status)
		}
		return nil, fmt.Errorf("%s: %s", resp.Status, e.Message)
	}
	var t struct {
		Token        string            `json:"token"`
		ExpiresAt    time.Time         `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []struct {
			Name string `json:"name"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(data, &t); err != nil || t.Token == "" {
		return nil, errors.New("unexpected answer from GitHub")
	}
	it := &installationToken{Token: t.Token, ExpiresAt: t.ExpiresAt, Permissions: t.Permissions}
	for _, r := range t.Repositories {
		it.Repositories = append(it.Repositories, r.Name)
	}
	return it, nil
}
