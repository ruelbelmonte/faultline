// Package config loads and validates faultline.yaml.
package config

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"faultline/internal/baggage"
	"faultline/internal/yamlx"
)

const (
	EnvToken      = "FAULTLINE_TOKEN"
	EnvTokensFile = "FAULTLINE_TOKENS_FILE"
	EnvAdminToken = "FAULTLINE_ADMIN_TOKEN"
	MinTokenLen   = 16
)

var NameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

type Route struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
}

type Config struct {
	Version        int
	Environment    string
	DataDir        string
	ExperimentsDir string

	ProxyListen       string
	ProxyAdminListen  string
	LeaseTimeout      time.Duration
	ExposeFaultHeader bool

	ControlListen         string
	ProxyAdminURL         string
	MaxExperimentDuration time.Duration
	AllowUnscoped         bool
	// AllowCommands enables traffic.command. Off by default: with it on,
	// anyone holding the control token can run shell commands on the
	// control host.
	AllowCommands bool
	// AllowInsecureAdmin permits an http:// proxy_admin_url to a host that is
	// not loopback, which sends the admin token in clear text.
	AllowInsecureAdmin bool
	// AllowedHosts restricts the hosts that probe and traffic URLs may name
	// (path.Match patterns on host or host:port). Empty allows any host
	// except link-local and metadata addresses.
	AllowedHosts []string
	// TrustedProxies are the reverse proxies whose X-Forwarded-For control
	// believes when it records a request's source address.
	TrustedProxies []netip.Prefix

	ScopeHeader       string
	CorrelationHeader string
	// ScopeBaggageKey and CorrelationBaggageKey name W3C baggage entries that
	// carry the run id and correlation id when the headers are absent.
	// Empty turns baggage off for that value.
	ScopeBaggageKey       string
	CorrelationBaggageKey string

	RedactionHeaders []string
	// RedactionQueryParams are substrings of query parameter names whose
	// values are redacted from stored URLs (compared case-insensitively).
	RedactionQueryParams []string
	StoreBodies          bool

	RetryAmplificationThreshold float64

	Routes []Route
}

func Default() *Config {
	return &Config{
		Version:                     1,
		Environment:                 "local",
		DataDir:                     ".faultline",
		ExperimentsDir:              "experiments",
		ProxyListen:                 "127.0.0.1:7701",
		ProxyAdminListen:            "127.0.0.1:7702",
		LeaseTimeout:                10 * time.Second,
		ExposeFaultHeader:           true,
		ControlListen:               "127.0.0.1:7700",
		ProxyAdminURL:               "http://127.0.0.1:7702",
		MaxExperimentDuration:       15 * time.Minute,
		ScopeHeader:                 "X-Chaos-Run",
		CorrelationHeader:           "X-Chaos-Correlation",
		ScopeBaggageKey:             "chaos-run",
		CorrelationBaggageKey:       "chaos-corr",
		RedactionHeaders:            []string{"authorization", "proxy-authorization", "x-api-key", "cookie", "set-cookie"},
		RedactionQueryParams:        []string{"token", "key", "secret", "passw", "pwd", "auth", "sig", "session", "credential"},
		RetryAmplificationThreshold: 3.0,
	}
}

func (c *Config) Route(name string) (Route, bool) {
	for _, r := range c.Routes {
		if r.Name == name {
			return r, true
		}
	}
	return Route{}, false
}

// IsProduction reports whether an environment name is one Faultline refuses to run in.
func IsProduction(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "prod", "production", "live":
		return true
	}
	return false
}

// ProductionError is returned when the environment guard trips.
type ProductionError struct{ Environment string }

func (e *ProductionError) Error() string {
	return fmt.Sprintf("refusing to start: environment %q looks like production. Faultline is for test environments with synthetic data only", e.Environment)
}

// Load reads and validates a config file. Validation problems are returned as
// yamlx.Errors; the production guard as *ProductionError.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config: %w", err)
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	var errs yamlx.Errors
	c := Default()
	root := yamlx.Parse(data, &errs)
	if root == nil {
		return nil, errs
	}

	if root.Require("version") {
		if v, ok := root.Int("version"); ok && v != 1 {
			errs.Add("version", "must be 1")
		}
	}
	if v, ok := root.Str("environment"); ok {
		c.Environment = v
		if IsProduction(v) {
			return nil, &ProductionError{Environment: v}
		}
		switch v {
		case "local", "ci", "qa":
		default:
			errs.Add("environment", "must be one of local, ci, qa")
		}
	}
	if v, ok := root.Str("data_dir"); ok {
		c.DataDir = nonEmpty(&errs, "data_dir", v)
	}
	if v, ok := root.Str("experiments_dir"); ok {
		c.ExperimentsDir = nonEmpty(&errs, "experiments_dir", v)
	}

	if m := root.Map("proxy"); m != nil {
		if v, ok := m.Str("listen"); ok {
			c.ProxyListen = listenAddr(&errs, m.Path("listen"), v)
		}
		if v, ok := m.Str("admin_listen"); ok {
			c.ProxyAdminListen = listenAddr(&errs, m.Path("admin_listen"), v)
		}
		if v, ok := m.Dur("lease_timeout"); ok {
			if v < time.Second {
				errs.Add(m.Path("lease_timeout"), "must be at least 1s")
			}
			c.LeaseTimeout = v
		}
		if v, ok := m.Bool("expose_fault_header"); ok {
			c.ExposeFaultHeader = v
		}
		m.Done()
	}

	if m := root.Map("control"); m != nil {
		if v, ok := m.Str("listen"); ok {
			c.ControlListen = listenAddr(&errs, m.Path("listen"), v)
		}
		if v, ok := m.Str("proxy_admin_url"); ok {
			c.ProxyAdminURL = strings.TrimRight(httpURL(&errs, m.Path("proxy_admin_url"), v), "/")
		}
		if v, ok := m.Dur("max_experiment_duration"); ok {
			if v <= 0 {
				errs.Add(m.Path("max_experiment_duration"), "must be greater than 0")
			}
			c.MaxExperimentDuration = v
		}
		if v, ok := m.Bool("allow_unscoped"); ok {
			c.AllowUnscoped = v
		}
		if v, ok := m.Bool("allow_commands"); ok {
			c.AllowCommands = v
		}
		if v, ok := m.Bool("allow_insecure_admin"); ok {
			c.AllowInsecureAdmin = v
		}
		if u, err := url.Parse(c.ProxyAdminURL); err == nil && u.Scheme == "http" && !isLoopback(u.Hostname()) && !c.AllowInsecureAdmin {
			errs.Add(m.Path("proxy_admin_url"), "must be https unless the host is loopback, because the admin token would cross the network in clear text; set control.allow_insecure_admin: true to accept that")
		}
		for _, it := range m.Seq("allowed_hosts") {
			if v, ok := it.Str(); ok {
				v = strings.TrimSpace(v)
				if _, err := path.Match(v, ""); err != nil || v == "" || strings.Contains(v, "/") {
					errs.Add(it.Path, "must be a host or host:port pattern such as *.qa.internal or 10.0.0.5:8080")
				}
				c.AllowedHosts = append(c.AllowedHosts, v)
			}
		}
		for _, it := range m.Seq("trusted_proxies") {
			if v, ok := it.Str(); ok {
				v = strings.TrimSpace(v)
				p, err := netip.ParsePrefix(v)
				if a, aerr := netip.ParseAddr(v); err != nil && aerr == nil {
					p, err = a.Prefix(a.BitLen())
				}
				if err != nil {
					errs.Add(it.Path, "must be an IP address or CIDR range such as 10.0.0.0/8")
					continue
				}
				c.TrustedProxies = append(c.TrustedProxies, p.Masked())
			}
		}
		m.Done()
	}

	if m := root.Map("scope"); m != nil {
		if v, ok := m.Str("header"); ok {
			c.ScopeHeader = headerName(&errs, m.Path("header"), v)
		}
		if v, ok := m.Str("baggage_key"); ok {
			c.ScopeBaggageKey = baggageKey(&errs, m.Path("baggage_key"), v)
		}
		m.Done()
	}
	if m := root.Map("correlation"); m != nil {
		if v, ok := m.Str("header"); ok {
			c.CorrelationHeader = headerName(&errs, m.Path("header"), v)
		}
		if v, ok := m.Str("baggage_key"); ok {
			c.CorrelationBaggageKey = baggageKey(&errs, m.Path("baggage_key"), v)
		}
		m.Done()
	}

	if m := root.Map("redaction"); m != nil {
		if m.Has("headers") {
			c.RedactionHeaders = nil
			for _, it := range m.Seq("headers") {
				if v, ok := it.Str(); ok {
					c.RedactionHeaders = append(c.RedactionHeaders, strings.ToLower(headerName(&errs, it.Path, v)))
				}
			}
		}
		if m.Has("query_params") {
			c.RedactionQueryParams = nil
			for _, it := range m.Seq("query_params") {
				if v, ok := it.Str(); ok {
					if strings.TrimSpace(v) == "" {
						errs.Add(it.Path, "must not be empty")
					}
					c.RedactionQueryParams = append(c.RedactionQueryParams, strings.ToLower(strings.TrimSpace(v)))
				}
			}
		}
		if v, ok := m.Bool("store_bodies"); ok && v {
			errs.Add(m.Path("store_bodies"), "must be false: this version never stores bodies")
		}
		m.Done()
	}

	if m := root.Map("findings"); m != nil {
		if v, ok := m.Float("retry_amplification_threshold"); ok {
			if v <= 1 {
				errs.Add(m.Path("retry_amplification_threshold"), "must be greater than 1")
			}
			c.RetryAmplificationThreshold = v
		}
		m.Done()
	}

	seen := map[string]bool{}
	for _, it := range root.Seq("routes") {
		m := it.Map()
		if m == nil {
			continue
		}
		var r Route
		if m.Require("name") {
			if v, ok := m.Str("name"); ok {
				r.Name = v
				switch {
				case !NameRE.MatchString(v):
					errs.Add(m.Path("name"), "must match [a-z0-9-]+")
				case seen[v]:
					errs.Add(m.Path("name"), "duplicate route name %q", v)
				}
				seen[v] = true
			}
		}
		if m.Require("upstream") {
			if v, ok := m.Str("upstream"); ok {
				r.Upstream = strings.TrimRight(httpURL(&errs, m.Path("upstream"), v), "/")
			}
		}
		m.Done()
		c.Routes = append(c.Routes, r)
	}
	root.Done()

	if len(errs) > 0 {
		return nil, errs
	}
	return c, nil
}

func nonEmpty(errs *yamlx.Errors, path, v string) string {
	if strings.TrimSpace(v) == "" {
		errs.Add(path, "must not be empty")
	}
	return v
}

func listenAddr(errs *yamlx.Errors, path, v string) string {
	if _, _, err := net.SplitHostPort(v); err != nil {
		errs.Add(path, "must be host:port")
	}
	return v
}

func httpURL(errs *yamlx.Errors, path, v string) string {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs.Add(path, "must be an http or https URL")
	}
	return v
}

func isLoopback(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

var headerRE = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]+$`)

func headerName(errs *yamlx.Errors, path, v string) string {
	if !headerRE.MatchString(v) {
		errs.Add(path, "must be a valid HTTP header name")
	}
	return v
}

// IsTokenVar reports whether an environment variable name is one of
// Faultline's secret tokens: any FAULTLINE_* variable whose name contains
// TOKEN. Names are compared case-insensitively, as Windows does.
func IsTokenVar(name string) bool {
	n := strings.ToUpper(name)
	return strings.HasPrefix(n, "FAULTLINE_") && strings.Contains(n, "TOKEN")
}

// EnvWithoutTokens returns a copy of environ (KEY=VALUE entries, as from
// os.Environ) without Faultline's token variables, for starting processes
// that must not see them.
func EnvWithoutTokens(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !IsTokenVar(name) {
			out = append(out, kv)
		}
	}
	return out
}

func baggageKey(errs *yamlx.Errors, path, v string) string {
	if v != "" && !baggage.ValidKey(v) {
		errs.Add(path, "must be a valid baggage key (an HTTP token), or empty to turn baggage off")
	}
	return v
}

// Token reads a required bearer token from the environment.
// NamedToken is a control token known by its SHA-256 only.
type NamedToken struct {
	Name string
	Sum  [sha256.Size]byte
}

// LoadTokens reads the control tokens: FAULTLINE_TOKEN as the single token
// named "default", or FAULTLINE_TOKENS_FILE with one "name:sha256hex" per
// line. Setting both is an error, so a shared token cannot linger unnoticed
// beside named ones. Errors never repeat a hash.
func LoadTokens() ([]NamedToken, error) {
	file := os.Getenv(EnvTokensFile)
	if os.Getenv(EnvToken) != "" && file != "" {
		return nil, fmt.Errorf("set %s or %s, not both", EnvToken, EnvTokensFile)
	}
	if file == "" {
		tok, err := Token(EnvToken)
		if err != nil {
			return nil, fmt.Errorf("%w (or set %s)", err, EnvTokensFile)
		}
		return []NamedToken{{Name: "default", Sum: sha256.Sum256([]byte(tok))}}, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvTokensFile, errors.Unwrap(err))
	}
	var out []NamedToken
	names, sums := map[string]bool{}, map[[sha256.Size]byte]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, h, ok := strings.Cut(line, ":")
		name, h = strings.TrimSpace(name), strings.TrimSpace(h)
		raw, herr := hex.DecodeString(h)
		switch {
		case !ok || herr != nil || len(raw) != sha256.Size:
			return nil, fmt.Errorf("%s line %d: must be name:sha256hex (64 hex digits)", EnvTokensFile, n)
		case !NameRE.MatchString(name):
			return nil, fmt.Errorf("%s line %d: the name may only contain lowercase letters, digits and hyphens", EnvTokensFile, n)
		case names[name]:
			return nil, fmt.Errorf("%s line %d: duplicate name %s", EnvTokensFile, n, name)
		}
		t := NamedToken{Name: name, Sum: [sha256.Size]byte(raw)}
		if t.Sum == sha256.Sum256(nil) {
			return nil, fmt.Errorf("%s line %d: that is the hash of an empty token", EnvTokensFile, n)
		}
		if sums[t.Sum] {
			return nil, fmt.Errorf("%s line %d: the same token is listed twice", EnvTokensFile, n)
		}
		names[name], sums[t.Sum] = true, true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists no tokens", EnvTokensFile)
	}
	return out, nil
}

func Token(env string) (string, error) {
	v := os.Getenv(env)
	if v == "" {
		return "", fmt.Errorf("%s is not set", env)
	}
	if len(v) < MinTokenLen {
		return "", fmt.Errorf("%s must be at least %d characters", env, MinTokenLen)
	}
	return v, nil
}
