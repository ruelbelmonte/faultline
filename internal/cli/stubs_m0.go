package cli

import "fmt"

func (e *env) notYet(name string) int {
	fmt.Fprintf(e.stderr, "faultline: %s is not built yet\n", name)
	return exitInternal
}

func (e *env) cmdUp(args []string) int      { return e.notYet("up") }
func (e *env) cmdProxy(args []string) int   { return e.notYet("proxy") }
func (e *env) cmdControl(args []string) int { return e.notYet("control") }
func (e *env) cmdRun(args []string) int     { return e.notYet("run") }
func (e *env) cmdStatus(args []string) int  { return e.notYet("status") }
func (e *env) cmdStop(args []string) int    { return e.notYet("stop") }
func (e *env) cmdReport(args []string) int  { return e.notYet("report") }
