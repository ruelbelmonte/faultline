package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"faultline/internal/config"
	"faultline/internal/proxy"
)

// envParentWatch makes a child of `faultline up` shut down when its stdin
// closes. Windows cannot deliver SIGTERM to a child, and this also covers the
// parent being killed outright.
const envParentWatch = "FAULTLINE_PARENT_WATCH"

type serveFlags struct {
	config    *string
	logFormat *string
}

func (e *env) serveFlags(fs *flag.FlagSet) serveFlags {
	return serveFlags{
		config:    fs.String("config", "faultline.yaml", "path to faultline.yaml"),
		logFormat: fs.String("log-format", "text", "log format: text or json"),
	}
}

func (e *env) logger(format, component string) (*slog.Logger, bool) {
	var h slog.Handler
	switch format {
	case "text":
		h = slog.NewTextHandler(e.stderr, nil)
	case "json":
		h = slog.NewJSONHandler(e.stderr, nil)
	default:
		return nil, false
	}
	return slog.New(h).With("component", component), true
}

// serveSetup does everything proxy and control share before serving: flags,
// config, production guard, logger. A non-zero code means stop.
func (e *env) serveSetup(name string, args []string) (*config.Config, *slog.Logger, int) {
	fs := e.flags(name)
	sf := e.serveFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return nil, nil, exitUsage
	}
	if len(pos) > 0 {
		return nil, nil, e.usageErr("%s takes no arguments", name)
	}
	log, ok := e.logger(*sf.logFormat, name)
	if !ok {
		return nil, nil, e.usageErr("--log-format must be text or json")
	}
	cfg, code := e.loadConfig(*sf.config)
	if cfg == nil {
		return nil, nil, code
	}
	return cfg, log, exitOK
}

func (e *env) requireToken(name string) (string, bool) {
	tok, err := config.Token(name)
	if err != nil {
		fmt.Fprintf(e.stderr, "faultline: %v\n", err)
		return "", false
	}
	return tok, true
}

// serveContext is cancelled by SIGINT, SIGTERM or, for children of
// `faultline up`, by the parent closing our stdin.
func serveContext() (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if os.Getenv(envParentWatch) == "1" {
		go func() {
			io.Copy(io.Discard, os.Stdin)
			cancel()
		}()
	}
	return ctx, cancel
}

func (e *env) cmdProxy(args []string) int {
	cfg, log, code := e.serveSetup("proxy", args)
	if cfg == nil {
		return code
	}
	token, ok := e.requireToken(config.EnvAdminToken)
	if !ok {
		return exitUsage
	}
	ctx, cancel := serveContext()
	defer cancel()
	if err := proxy.Run(ctx, cfg, token, log); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(e.stderr, "faultline: %v\n", err)
		return exitInternal
	}
	return exitOK
}
