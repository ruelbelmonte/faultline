// Package demo is a deliberately simple system under test: shop, inventory,
// pricing and an Anthropic-compatible LLM mock, in one process.
package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	ModeNaive = "naive"
	ModeGood  = "good"
)

type Options struct {
	Mode              string
	Proxy             string
	Shop              string
	Inventory         string
	Pricing           string
	LLM               string
	ScopeHeader       string
	CorrelationHeader string
}

type resilience struct {
	timeout       time.Duration
	attempts      int
	backoff       time.Duration
	honourRetry   bool
	retryAfterCap time.Duration
}

var modes = map[string]resilience{
	ModeNaive: {attempts: 4},
	ModeGood:  {timeout: time.Second, attempts: 2, backoff: 200 * time.Millisecond, honourRetry: true, retryAfterCap: 2 * time.Second},
}

func ValidMode(m string) bool {
	_, ok := modes[m]
	return ok
}

type app struct {
	opts   Options
	res    resilience
	client *http.Client
	log    *slog.Logger
}

// Run serves until ctx is cancelled, then shuts the servers down gracefully.
func Run(ctx context.Context, opts Options, log *slog.Logger) error {
	res, ok := modes[opts.Mode]
	if !ok {
		return fmt.Errorf("unknown mode %q", opts.Mode)
	}
	a := &app{
		opts:   opts,
		res:    res,
		client: &http.Client{Timeout: res.timeout},
		log:    log,
	}
	a.opts.Proxy = strings.TrimRight(opts.Proxy, "/")

	servers := []struct {
		name, addr string
		handler    http.Handler
	}{
		{"shop", opts.Shop, a.shop()},
		{"inventory", opts.Inventory, a.inventory()},
		{"pricing", opts.Pricing, a.pricing()},
		{"llm", opts.LLM, a.llm()},
	}
	errc := make(chan error, len(servers))
	var running []*http.Server
	for _, s := range servers {
		ln, err := net.Listen("tcp", s.addr)
		if err != nil {
			for _, r := range running {
				r.Close()
			}
			return fmt.Errorf("%s: %w", s.name, err)
		}
		srv := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second}
		running = append(running, srv)
		log.Info("listening", "server", s.name, "addr", ln.Addr().String(), "mode", opts.Mode)
		go func() { errc <- srv.Serve(ln) }()
	}

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, srv := range running {
		if srv.Shutdown(shutdownCtx) != nil {
			srv.Close()
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func health(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}

type stock struct {
	ID    string  `json:"id"`
	Stock int     `json:"stock"`
	Price float64 `json:"price"`
}

type price struct {
	ID    string  `json:"id"`
	Price float64 `json:"price"`
}

func (a *app) shop() http.Handler {
	mux := http.NewServeMux()
	health(mux)
	mux.HandleFunc("GET /api/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		var s stock
		if err := a.call(r, a.opts.Proxy+"/inventory/stock/"+r.PathValue("id"), &s); err != nil {
			a.log.Warn("shop: inventory call failed", "error", err.Error())
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream"})
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	return mux
}

func (a *app) inventory() http.Handler {
	mux := http.NewServeMux()
	health(mux)
	mux.HandleFunc("GET /stock/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p price
		if err := a.call(r, a.opts.Proxy+"/pricing/price/"+r.PathValue("id"), &p); err != nil {
			a.log.Warn("inventory: pricing call failed", "error", err.Error())
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream"})
			return
		}
		writeJSON(w, http.StatusOK, stock{ID: r.PathValue("id"), Stock: 42, Price: p.Price})
	})
	return mux
}

func (a *app) pricing() http.Handler {
	mux := http.NewServeMux()
	health(mux)
	mux.HandleFunc("GET /price/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, price{ID: r.PathValue("id"), Price: 9.99})
	})
	return mux
}

// call performs a downstream GET with the retry behaviour of the current mode.
func (a *app) call(in *http.Request, url string, out any) error {
	var lastErr error
	wait := time.Duration(0)
	for attempt := 1; attempt <= a.res.attempts; attempt++ {
		if attempt > 1 && wait > 0 {
			select {
			case <-time.After(wait):
			case <-in.Context().Done():
				return in.Context().Err()
			}
		}
		wait = a.res.backoff

		req, err := http.NewRequestWithContext(in.Context(), http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		for _, h := range []string{a.opts.ScopeHeader, a.opts.CorrelationHeader, "traceparent"} {
			if v := in.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		resp, err := a.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch {
		case readErr != nil:
			lastErr = readErr
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("downstream status %d", resp.StatusCode)
			if a.res.honourRetry {
				if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs >= 0 {
					wait = max(a.res.backoff, min(time.Duration(secs)*time.Second, a.res.retryAfterCap))
				}
			}
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("downstream status %d", resp.StatusCode)
		default:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("downstream body: %w", err)
			}
			return nil
		}
	}
	return lastErr
}
