package control

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// dialContext connects for the traffic driver and the probes. It refuses
// link-local and cloud metadata addresses unless control.allowed_hosts names
// them exactly. The check runs on the address actually dialled, after name
// resolution, so a host name that passed validation but resolves to such an
// address is refused too (issue #11).
func (c *Control) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			if !c.cfg.AllowedIP(ip) {
				return fmt.Errorf("refusing to connect to %s: link-local or cloud metadata address not listed in control.allowed_hosts", ip)
			}
			return nil
		},
	}
	return d.DialContext(ctx, network, addr)
}
