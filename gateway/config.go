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
	// Token, if set, must be sent by every client, either as
	// "Authorization: Bearer <token>" or as "X-Api-Key: <token>".
	Token string `json:"token"`
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
	c.Token = expand(c.Token)
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
	if hasControl(c.Token) {
		return errors.New("token contains a control character, such as a trailing newline")
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

// headerNameChars are the characters allowed in a header name (RFC 9110).
const headerNameChars = "!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f })
}
