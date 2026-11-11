package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"faultline/internal/config"
	"faultline/internal/control"
)

func (e *env) cmdControl(args []string) int {
	cfg, log, code := e.serveSetup("control", args)
	if cfg == nil {
		return code
	}
	token, ok := e.requireToken(config.EnvToken)
	if !ok {
		return exitUsage
	}
	adminToken, ok := e.requireToken(config.EnvAdminToken)
	if !ok {
		return exitUsage
	}
	ctx, cancel := serveContext()
	defer cancel()
	if err := control.Serve(ctx, cfg, token, adminToken, log); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(e.stderr, "faultline: %v\n", err)
		return exitInternal
	}
	return exitOK
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type child struct {
	name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}
}

// displayURL turns a listen address into something a browser can open.
func displayURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// cmdUp spawns proxy and control as child processes and stops both on exit.
func (e *env) cmdUp(args []string) int {
	fs := e.flags("up")
	sf := e.serveFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) > 0 {
		return e.usageErr("up takes no arguments")
	}
	if _, ok := e.logger(*sf.logFormat, "up"); !ok {
		return e.usageErr("--log-format must be text or json")
	}
	cfg, code := e.loadConfig(*sf.config)
	if cfg == nil {
		return code
	}

	tokens := map[string]string{}
	generated := map[string]bool{}
	for _, name := range []string{config.EnvToken, config.EnvAdminToken} {
		if os.Getenv(name) == "" {
			tok, err := newToken()
			if err != nil {
				fmt.Fprintf(e.stderr, "faultline: generating a token failed: %v\n", err)
				return exitInternal
			}
			tokens[name], generated[name] = tok, true
			continue
		}
		tok, err := config.Token(name)
		if err != nil {
			fmt.Fprintf(e.stderr, "faultline: %v\n", err)
			return exitUsage
		}
		tokens[name] = tok
	}

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(e.stderr, "faultline: %v\n", err)
		return exitInternal
	}
	var children []*child
	stopAll := func() {
		for _, c := range children {
			// Closing stdin asks the child to shut down on every platform;
			// the signal covers Unix children that are blocked elsewhere.
			c.stdin.Close()
			c.cmd.Process.Signal(os.Interrupt)
		}
		deadline := time.After(10 * time.Second)
		for _, c := range children {
			select {
			case <-c.done:
			case <-deadline:
				c.cmd.Process.Kill()
				<-c.done
			}
		}
	}
	for _, name := range []string{"proxy", "control"} {
		cmd := exec.Command(self, name, "--config", *sf.config, "--log-format", *sf.logFormat)
		cmd.Env = append(os.Environ(),
			config.EnvToken+"="+tokens[config.EnvToken],
			config.EnvAdminToken+"="+tokens[config.EnvAdminToken],
			envParentWatch+"=1",
		)
		cmd.Stdout, cmd.Stderr = e.stderr, e.stderr
		stdin, err := cmd.StdinPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			fmt.Fprintf(e.stderr, "faultline: starting %s failed: %v\n", name, err)
			stopAll()
			return exitInternal
		}
		c := &child{name: name, cmd: cmd, stdin: stdin, done: make(chan struct{})}
		go func() {
			cmd.Wait()
			close(c.done)
		}()
		children = append(children, c)
	}

	exited := make(chan *child, len(children))
	for _, c := range children {
		go func() {
			<-c.done
			exited <- c
		}()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	ready := make(chan bool, 1)
	go func() {
		ready <- waitHealthy(ctx, cfg.ProxyAdminURL+"/admin/v1/healthz", displayURL(cfg.ControlListen)+"/healthz")
	}()

	exitCode := exitOK
	for {
		select {
		case ok := <-ready:
			if !ok {
				continue
			}
			fmt.Fprintf(e.stdout, "Faultline is up (environment: %s)\n", cfg.Environment)
			fmt.Fprintf(e.stdout, "  Dashboard:    %s\n", displayURL(cfg.ControlListen))
			fmt.Fprintf(e.stdout, "  Proxy:        %s\n", displayURL(cfg.ProxyListen))
			fmt.Fprintf(e.stdout, "  Proxy admin:  %s\n", displayURL(cfg.ProxyAdminListen))
			for _, name := range []string{config.EnvToken, config.EnvAdminToken} {
				if generated[name] {
					// Shown once, never logged. Paste into your shell to use the CLI.
					fmt.Fprintf(e.stdout, "%s=%s\n", name, tokens[name])
				} else {
					fmt.Fprintf(e.stdout, "  %s is taken from the environment\n", name)
				}
			}
			fmt.Fprintln(e.stdout, "Press Ctrl+C to stop.")
			continue
		case c := <-exited:
			fmt.Fprintf(e.stderr, "faultline: %s exited (code %d); stopping\n", c.name, c.cmd.ProcessState.ExitCode())
			exitCode = exitInternal
			if c.cmd.ProcessState.ExitCode() == exitUsage {
				exitCode = exitUsage
			}
		case <-ctx.Done():
		}
		break
	}
	stopAll()
	return exitCode
}

func waitHealthy(ctx context.Context, urls ...string) bool {
	client := &http.Client{Timeout: time.Second}
	for _, u := range urls {
		for {
			if ctx.Err() != nil {
				return false
			}
			resp, err := client.Get(u)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return true
}
