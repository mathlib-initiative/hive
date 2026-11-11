package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Config is the broker's JSON config file. String values may contain
// ${NAME}, which is replaced with the environment variable NAME when the file
// is loaded, so secrets can stay in the environment. Any other $ is kept as
// it is.
type Config struct {
	// Listen is the address to serve on. Default ":8080".
	Listen string `json:"listen"`
	// Clients maps each client's name to its own token. Every request must
	// carry one of the tokens, either as "Authorization: Bearer <token>" or
	// as "X-Api-Key: <token>", and the log names its client.
	Clients map[string]string `json:"clients"`
	// GitHub issues installation tokens of a GitHub App.
	GitHub *GitHub `json:"github"`
}

// GitHub is a GitHub App and what each client may reach with its tokens.
type GitHub struct {
	// AppID is the App's client ID, which GitHub recommends, or its numeric
	// app ID.
	AppID string `json:"app_id"`
	// PrivateKeyFile is the path of one of the App's private keys, in PEM.
	PrivateKeyFile string `json:"private_key_file"`
	// API is the base URL of the REST API. Default "https://api.github.com".
	API string `json:"api_url"`
	// Access maps a client's name to the most its tokens may reach. A client
	// without an entry gets no GitHub tokens.
	Access map[string]GitHubAccess `json:"access"`
}

// GitHubAccess is the most that one client's tokens may reach.
type GitHubAccess struct {
	// InstallationID is the App's installation on the account that owns
	// the repositories.
	InstallationID int64 `json:"installation_id"`
	// Repositories, if not empty, limits tokens to these repositories of the
	// installation, by name.
	Repositories []string `json:"repositories"`
	// Permissions, if not empty, limits tokens to these permissions, such as
	// {"contents": "write"}.
	Permissions map[string]string `json:"permissions"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.expandEnv(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${NAME} in every string value. An unset or empty
// variable is an error, so a missing secret is caught at startup.
func (c *Config) expandEnv() error {
	var missing []string
	expand := func(s string) string {
		return envRef.ReplaceAllStringFunc(s, func(ref string) string {
			name := ref[2 : len(ref)-1]
			v := os.Getenv(name)
			if v == "" {
				missing = append(missing, name)
			}
			return v
		})
	}
	c.Listen = expand(c.Listen)
	for name, token := range c.Clients {
		c.Clients[name] = expand(token)
	}
	if g := c.GitHub; g != nil {
		g.AppID = expand(g.AppID)
		g.PrivateKeyFile = expand(g.PrivateKeyFile)
		g.API = expand(g.API)
		for name, a := range g.Access {
			for i, repo := range a.Repositories {
				a.Repositories[i] = expand(repo)
			}
			for p, level := range a.Permissions {
				a.Permissions[p] = expand(level)
			}
			g.Access[name] = a
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("unset or empty environment variables: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c *Config) validate() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	// Tokens are never printed.
	if len(c.Clients) == 0 {
		return errors.New("no clients")
	}
	tokens := make(map[string]bool, len(c.Clients))
	for name, token := range c.Clients {
		if !clientName.MatchString(name) {
			return fmt.Errorf("client %q: a name must start with an ASCII letter or digit and contain only ASCII letters, digits, '.', '_' and '-'", name)
		}
		if token == "" {
			return fmt.Errorf("client %q: empty token", name)
		}
		if hasControl(token) {
			return fmt.Errorf("client %q: token contains a control character, such as a trailing newline", name)
		}
		if tokens[token] {
			return fmt.Errorf("client %q: another client has the same token", name)
		}
		tokens[token] = true
	}
	g := c.GitHub
	if g == nil {
		return errors.New("no credentials to issue: github is not set")
	}
	if g.AppID == "" || g.PrivateKeyFile == "" {
		return errors.New("github: app_id and private_key_file are required")
	}
	if hasControl(g.AppID) {
		return errors.New("github: app_id contains a control character, such as a trailing newline")
	}
	if g.API == "" {
		g.API = "https://api.github.com"
	}
	g.API = strings.TrimSuffix(g.API, "/")
	u, err := url.Parse(g.API)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("github: api_url must be an absolute http or https URL without user, query or fragment")
	}
	for name, a := range g.Access {
		if _, ok := c.Clients[name]; !ok {
			return fmt.Errorf("github: access for unknown client %q", name)
		}
		if a.InstallationID <= 0 {
			return fmt.Errorf("github: access for %q: installation_id is required", name)
		}
		if err := checkRequest(a.Repositories, a.Permissions); err != nil {
			return fmt.Errorf("github: access for %q: %w", name, err)
		}
	}
	return nil
}

// checkRequest checks repository names and permission levels.
func checkRequest(repos []string, perms map[string]string) error {
	if len(repos) > 500 {
		return errors.New("more than 500 repositories")
	}
	seen := make(map[string]bool, len(repos))
	for _, repo := range repos {
		if !repoName.MatchString(repo) {
			return fmt.Errorf("invalid repository name %q", repo)
		}
		if seen[strings.ToLower(repo)] {
			return fmt.Errorf("repository %q is listed twice", repo)
		}
		seen[strings.ToLower(repo)] = true
	}
	for p, level := range perms {
		if !permName.MatchString(p) {
			return fmt.Errorf("invalid permission name %q", p)
		}
		if levels[level] == 0 {
			return fmt.Errorf("permission %q: level must be read, write or admin", p)
		}
	}
	return nil
}

// levels orders the permission levels.
var levels = map[string]int{"read": 1, "write": 2, "admin": 3}

var (
	// clientName matches a client name. Names appear in logs, so they are
	// kept to characters that need no quoting, and differ from "-", which
	// marks a request that was not authenticated.
	clientName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	repoName   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	permName   = regexp.MustCompile(`^[a-z_]+$`)
)

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f })
}
