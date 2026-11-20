package cli

import (
	"fmt"

	"faultline/internal/control"
)

func (e *env) cmdAudit(args []string) int {
	if len(args) == 0 || args[0] != "verify" {
		return e.usageErr("audit needs the subcommand verify")
	}
	fs := e.flags("audit verify")
	cfgPath := fs.String("config", "faultline.yaml", "path to faultline.yaml")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return exitUsage
	}
	if len(pos) != 0 {
		return e.usageErr("audit verify takes no arguments")
	}
	cfg, code := e.loadConfig(*cfgPath)
	if cfg == nil {
		return code
	}
	n, problems, err := control.VerifyAudit(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "faultline: %v\n", err)
		return exitInternal
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(e.stdout, p)
		}
		return exitFailed
	}
	fmt.Fprintf(e.stdout, "audit log verified: %d entries, chain intact, SQLite matches\n", n)
	return exitOK
}
