// Package cli implements the faultline command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"faultline/internal/config"
	"faultline/internal/experiment"
	"faultline/internal/yamlx"
)

// Version is overridden at build time with -ldflags.
var Version = "0.1.0"

const (
	exitOK       = 0
	exitInternal = 1
	exitUsage    = 2
)

const usage = `faultline: fault-injection proxy and control plane for test environments

Usage:
  faultline version
  faultline init [dir]
  faultline validate <file|dir> [--config FILE]
  faultline up [--config FILE]
  faultline proxy [--config FILE]
  faultline control [--config FILE]
  faultline run <file|dir> [--fail-on-breach] [--report-dir DIR] [--format json,md,html,junit] [--url URL]
  faultline status [--url URL]
  faultline stop [--url URL]
  faultline report <run_id> --format F [--out FILE] [--url URL]

Environment:
  FAULTLINE_URL          control plane URL (default http://127.0.0.1:7700)
  FAULTLINE_TOKEN        bearer token for the control API
  FAULTLINE_ADMIN_TOKEN  bearer token for the proxy admin API
`

type env struct {
	stdout io.Writer
	stderr io.Writer
}

// Run executes one command and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	e := &env{stdout: stdout, stderr: stderr}
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "faultline %s\n", Version)
		return exitOK
	case "init":
		return e.cmdInit(rest)
	case "validate":
		return e.cmdValidate(rest)
	case "up":
		return e.cmdUp(rest)
	case "proxy":
		return e.cmdProxy(rest)
	case "control":
		return e.cmdControl(rest)
	case "run":
		return e.cmdRun(rest)
	case "status":
		return e.cmdStatus(rest)
	case "stop":
		return e.cmdStop(rest)
	case "report":
		return e.cmdReport(rest)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "faultline: unknown command %q\n\n%s", cmd, usage)
	return exitUsage
}

func (e *env) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("faultline "+name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	return fs
}

// parse accepts flags before, between and after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func (e *env) usageErr(format string, args ...any) int {
	fmt.Fprintf(e.stderr, "faultline: "+format+"\n", args...)
	return exitUsage
}

// loadConfig prints the reason and returns exit code 2 when the config is unusable.
func (e *env) loadConfig(path string) (*config.Config, int) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, exitOK
	}
	var verrs yamlx.Errors
	if errors.As(err, &verrs) {
		fmt.Fprintf(e.stderr, "faultline: %s is invalid:\n", path)
		for _, v := range verrs {
			fmt.Fprintf(e.stderr, "  %s\n", v)
		}
	} else {
		fmt.Fprintf(e.stderr, "faultline: %v\n", err)
	}
	return nil, exitUsage
}

func (e *env) cmdValidate(args []string) int {
	fs := e.flags("validate")
	cfgPath := fs.String("config", "faultline.yaml", "path to faultline.yaml")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 1 {
		return e.usageErr("validate needs exactly one file or directory")
	}
	cfg, code := e.loadConfig(*cfgPath)
	if cfg == nil {
		return code
	}
	files, err := experimentFiles(pos[0])
	if err != nil {
		return e.usageErr("%v", err)
	}
	invalid := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return e.usageErr("%v", err)
		}
		_, errs := experiment.Parse(data, cfg)
		if len(errs) == 0 {
			fmt.Fprintf(e.stdout, "%s: valid\n", f)
			continue
		}
		invalid++
		fmt.Fprintf(e.stdout, "%s: invalid\n", f)
		for _, v := range errs {
			fmt.Fprintln(e.stdout, v.String())
		}
	}
	if invalid > 0 {
		return exitUsage
	}
	return exitOK
}

// experimentFiles expands a file or a directory into experiment file paths.
func experimentFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, ent := range entries {
		ext := strings.ToLower(filepath.Ext(ent.Name()))
		if !ent.IsDir() && (ext == ".yaml" || ext == ".yml") {
			files = append(files, filepath.Join(path, ent.Name()))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("no .yaml or .yml files in %s", path)
	}
	return files, nil
}
