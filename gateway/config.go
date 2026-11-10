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

// Config is the gateway's JSON config file. String values may contain
// ${NAME}, which is replaced with the environment variable NAME when the file
// is loaded, so secrets can stay in the environment. Any other $ is kept as
// it is.
type Config struct {
	// Listen is the address to serve on. Default ":8080".
	Listen string `json:"listen"`
	// Clients maps each client's name to its own token. If not empty, every
	// request must carry one of the tokens, either as "Authorization: Bearer
	// <token>" or as "X-Api-Key: <token>", and the log names its client.
	Clients map[string]string `json:"clients"`
	// Routes maps a path prefix such as "/anthropic" to its upstream.
	Routes map[string]Route `json:"routes"`
}

// Route forwards requests below one path prefix to an upstream.
type Route struct {
	// Upstream is the base URL. A request for <prefix>/v1/messages goes to
	// <upstream>/v1/messages.
	Upstream string `json:"upstream"`
	// Headers are set on every forwarded request, replacing any value sent by
	// the client. This is where provider credentials go.
	Headers map[string]string `json:"headers"`
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
// variable is an error, so a missing secret is caught at startup rather than
// sent upstream as an empty credential.
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
	for prefix, r := range c.Routes {
		r.Upstream = expand(r.Upstream)
		headers := make(map[string]string, len(r.Headers))
		for k, v := range r.Headers {
			headers[k] = expand(v)
		}
		r.Headers = headers
		c.Routes[prefix] = r
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
	// Values are never printed, since they may contain secrets.
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
	if len(c.Routes) == 0 {
		return errors.New("no routes")
	}
	for prefix, r := range c.Routes {
		if !strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
			return fmt.Errorf("route %q: prefix must start with / and not end with /", prefix)
		}
		u, err := url.Parse(r.Upstream)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("route %q: upstream must be an absolute http or https URL", prefix)
		}
		for k, v := range r.Headers {
			if k == "" || strings.Trim(k, headerNameChars) != "" {
				return fmt.Errorf("route %q: invalid header name %q", prefix, k)
			}
			if hasControl(v) {
				return fmt.Errorf("route %q: header %q contains a control character, such as a trailing newline", prefix, k)
			}
		}
	}
	return nil
}

// clientName matches a client name. Names appear in logs, so they are kept
// to characters that need no quoting, and differ from "-", which marks a
// request that was not authenticated.
var clientName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// headerNameChars are the characters allowed in a header name (RFC 9110).
const headerNameChars = "!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f })
}
