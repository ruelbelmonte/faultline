// Package config loads and validates faultline.yaml.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"faultline/internal/yamlx"
)

const (
	EnvToken      = "FAULTLINE_TOKEN"
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

	ScopeHeader       string
	CorrelationHeader string

	RedactionHeaders []string
	StoreBodies      bool

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
		RedactionHeaders:            []string{"authorization", "proxy-authorization", "x-api-key", "cookie", "set-cookie"},
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
		m.Done()
	}

	if m := root.Map("scope"); m != nil {
		if v, ok := m.Str("header"); ok {
			c.ScopeHeader = headerName(&errs, m.Path("header"), v)
		}
		m.Done()
	}
	if m := root.Map("correlation"); m != nil {
		if v, ok := m.Str("header"); ok {
			c.CorrelationHeader = headerName(&errs, m.Path("header"), v)
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

var headerRE = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]+$`)

func headerName(errs *yamlx.Errors, path, v string) string {
	if !headerRE.MatchString(v) {
		errs.Add(path, "must be a valid HTTP header name")
	}
	return v
}

// Token reads a required bearer token from the environment.
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
