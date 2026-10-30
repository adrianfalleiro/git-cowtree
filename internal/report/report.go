// Package report prints progress, warnings and timing traces. Everything goes
// to stderr, so stdout stays clean for callers that parse it.
package report

import (
	"fmt"
	"os"
	"time"
)

// Prog is the program name used as the prefix of every message.
const Prog = "git-cowtree"

var (
	quiet = os.Getenv("COWTREE_QUIET") != ""

	// Tracing reports whether COWTREE_TRACE is set, which prints step timings.
	Tracing = os.Getenv("COWTREE_TRACE") != ""
)

// Logf prints a progress message unless COWTREE_QUIET is set.
func Logf(format string, a ...any) {
	if !quiet {
		fmt.Fprintf(os.Stderr, Prog+": "+format+"\n", a...)
	}
}

// Errorf prints an error message, whatever COWTREE_QUIET says.
func Errorf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, Prog+": "+format+"\n", a...)
}

// Fatalf prints an error message and exits with status 1.
func Fatalf(format string, a ...any) {
	Errorf(format, a...)
	os.Exit(1)
}

// Trace prints how long a step took since start, when tracing is on.
// Use it as `defer report.Trace(time.Now(), "step")`.
func Trace(start time.Time, what string) {
	if Tracing {
		fmt.Fprintf(os.Stderr, "%s: %7.1fms  %s\n", Prog, float64(time.Since(start).Microseconds())/1000, what)
	}
}
