package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"strings"
)

// Addresses and names of cloud instance metadata services, plus all
// link-local addresses (169.254.0.0/16, fe80::/10). A probe or traffic URL
// that reaches one of them could read instance credentials, so they are
// refused unless allowed_hosts names them exactly (issue #11).
var metadataPrefixes = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS over IPv6
}

var metadataNames = map[string]bool{
	"metadata":                   true,
	"metadata.google.internal":   true,
	"metadata.goog":              true,
	"instance-data":              true,
	"instance-data.ec2.internal": true,
}

// IsMetadataAddr reports whether ip is link-local or a known metadata address.
func IsMetadataAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range metadataPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func isMetadataHost(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return IsMetadataAddr(ip)
	}
	return metadataNames[strings.TrimSuffix(host, ".")]
}

// hostAndPort returns the lower-cased host (without IPv6 brackets) and
// host:port of u, filling in the scheme's default port.
func hostAndPort(u *url.URL) (host, hostPort string) {
	host = strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return host, net.JoinHostPort(host, port)
}

func hasGlob(p string) bool { return strings.ContainsAny(p, `*?[\`) }

// explicitlyAllowed reports whether allowed_hosts names host (or host:port)
// literally, without a wildcard.
func (c *Config) explicitlyAllowed(host, hostPort string) bool {
	for _, p := range c.AllowedHosts {
		if hasGlob(p) {
			continue
		}
		p = strings.Trim(strings.ToLower(p), "[]")
		if p == host || p == hostPort || p == strings.ReplaceAll(hostPort, "[", "") {
			return true
		}
	}
	return false
}

// AllowedIP reports whether the control plane may connect to ip. It refuses
// link-local and metadata addresses unless allowed_hosts names the address
// literally. It is checked again at connect time because a host name that
// passed validation can resolve to such an address.
func (c *Config) AllowedIP(ip netip.Addr) bool {
	if !IsMetadataAddr(ip) {
		return true
	}
	s := strings.ToLower(ip.Unmap().String())
	for _, p := range c.AllowedHosts {
		p = strings.ToLower(p)
		if hasGlob(p) {
			continue
		}
		if h, _, err := net.SplitHostPort(p); err == nil {
			p = h
		}
		if strings.Trim(p, "[]") == s {
			return true
		}
	}
	return false
}

// CheckHost reports why the control plane may not send probe or traffic
// requests to rawURL, or nil if it may. Rules: link-local and metadata
// addresses and names are refused unless allowed_hosts names them exactly;
// when allowed_hosts is not empty, the host or host:port must match one of
// its patterns (path.Match syntax, case-insensitive).
func (c *Config) CheckHost(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return nil // reported by URL validation
	}
	host, hostPort := hostAndPort(u)
	if isMetadataHost(host) && !c.explicitlyAllowed(host, hostPort) {
		return fmt.Errorf("host %s is a link-local or cloud metadata address; it is refused unless control.allowed_hosts lists it exactly", host)
	}
	if len(c.AllowedHosts) == 0 {
		return nil
	}
	for _, p := range c.AllowedHosts {
		p = strings.ToLower(p)
		if ok, _ := path.Match(p, host); ok {
			return nil
		}
		if ok, _ := path.Match(p, hostPort); ok {
			return nil
		}
		if ok, _ := path.Match(strings.Trim(p, "[]"), strings.ReplaceAll(strings.ReplaceAll(hostPort, "[", ""), "]", "")); ok {
			return nil
		}
	}
	return fmt.Errorf("host %s is not in control.allowed_hosts", hostPort)
}
