package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"faultline/internal/config"
)

// Run serves traffic and the admin API until ctx is cancelled.
func Run(ctx context.Context, cfg *config.Config, adminToken string, log *slog.Logger) error {
	p, err := New(cfg, log)
	if err != nil {
		return err
	}
	trafficLn, err := net.Listen("tcp", cfg.ProxyListen)
	if err != nil {
		return fmt.Errorf("proxy listen: %w", err)
	}
	adminLn, err := net.Listen("tcp", cfg.ProxyAdminListen)
	if err != nil {
		trafficLn.Close()
		return fmt.Errorf("proxy admin listen: %w", err)
	}

	traffic := &http.Server{
		Handler: p, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 1 << 20,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, connKey{}, c) },
	}
	admin := &http.Server{Handler: p.Admin(adminToken), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10}

	stopWatch := make(chan struct{})
	go p.engine.Watch(stopWatch)
	defer close(stopWatch)

	errc := make(chan error, 2)
	go func() { errc <- traffic.Serve(rawListener{trafficLn}) }()
	go func() { errc <- admin.Serve(adminLn) }()
	log.Info("proxy listening", "traffic", trafficLn.Addr().String(), "admin", adminLn.Addr().String(),
		"routes", len(cfg.Routes), "environment", cfg.Environment, "lease_timeout", cfg.LeaseTimeout.String())

	select {
	case <-ctx.Done():
		err = nil
	case err = <-errc:
	}
	// Releases in-flight holds so shutdown is not delayed by them.
	p.engine.Clear("shutdown")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range []*http.Server{admin, traffic} {
		if s.Shutdown(shutdownCtx) != nil {
			s.Close()
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
