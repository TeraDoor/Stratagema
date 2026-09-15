package main

import (
	"flag"
	"fmt"
	"os"
)

// die prints a uniform error to stderr and returns the exit code the
// caller should return from main. Never panics, never os.Exit's directly —
// callers propagate the code so main is the only place that exits.
func die(code int, format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "stratagema: "+format+"\n", args...)
	return code
}

// dbPathFlag is the one flag every subcommand accepts, matching across
// commands so a resource, identity, and interest all resolve to the same
// store without needing to be told three times in one script.
func dbPathFlag(fs *flag.FlagSet) *string {
	return fs.String("db", "", "event database path (default: $STRATAGEMA_DB, else ./.stratagema/events.db)")
}
