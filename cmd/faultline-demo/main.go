// faultline-demo is the demo system under test. It never imports the
// faultline proxy or control plane.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"faultline/internal/demo"
)

func main() {
	var o demo.Options
	fs := flag.NewFlagSet("faultline-demo", flag.ContinueOnError)
	fs.StringVar(&o.Mode, "mode", demo.ModeNaive, "resilience mode: naive or good")
	fs.StringVar(&o.Proxy, "proxy", "http://127.0.0.1:7701", "Faultline proxy base URL used for downstream calls")
	fs.StringVar(&o.Shop, "shop", "127.0.0.1:9000", "shop listen address")
	fs.StringVar(&o.Inventory, "inventory", "127.0.0.1:9001", "inventory listen address")
	fs.StringVar(&o.Pricing, "pricing", "127.0.0.1:9002", "pricing listen address")
	fs.StringVar(&o.LLM, "llm", "127.0.0.1:9003", "LLM mock listen address")
	fs.StringVar(&o.ScopeHeader, "scope-header", "X-Chaos-Run", "scope header forwarded downstream")
	fs.StringVar(&o.CorrelationHeader, "correlation-header", "X-Chaos-Correlation", "correlation header forwarded downstream")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if !demo.ValidMode(o.Mode) {
		fmt.Fprintf(os.Stderr, "faultline-demo: --mode must be naive or good, got %q\n", o.Mode)
		os.Exit(2)
	}

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := demo.Run(ctx, o, slog.New(handler)); err != nil {
		fmt.Fprintf(os.Stderr, "faultline-demo: %v\n", err)
		os.Exit(1)
	}
}
