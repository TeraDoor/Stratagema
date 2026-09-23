package main

import (
	"fmt"
	"os"
)

// version is stamped at build time via:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
//
// A plain `go build` (no ldflags) leaves this at its zero value, so the
// version command falls back to something honest rather than a stale
// hardcoded string that silently drifts from what was actually released.
var version = ""

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, rest := os.Args[1], os.Args[2:]
	var code int
	switch cmd {
	case "identity":
		code = cmdIdentity(rest)
	case "tactic":
		code = cmdTactic(rest)
	case "lock":
		code = cmdLock(rest)
	case "interest":
		code = cmdInterest(rest)
	case "strategy":
		code = cmdStrategy(rest)
	case "serve":
		code = cmdServe(rest)
	case "version":
		v := version
		if v == "" {
			v = "0.0.0-dev"
		}
		fmt.Printf("stratagema %s\n", v)
		code = 0
	case "-h", "--help", "help":
		usage()
		code = 0
	default:
		fmt.Fprintf(os.Stderr, "stratagema: unknown command %q\n", cmd)
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Println(`stratagema — lock shared resources between agents, notify subscribers when they change

usage: stratagema <command> [flags]

commands:
  identity   create and list agent identities (create|list)
  tactic    scaffold, list, and view agent-role definition files (create|list|show)
  lock       claim, free, and inspect a named resource lock (acquire|release|status|list)
  interest   subscribe to a resource and read/acknowledge notifications (create|list|pause|resume|inbox|ack)
  strategy   durable strategy ledger: findings/decisions agents log as they work (create|list|show|log|activate|observe|close)
  serve      optional: push interest notifications live over SSE
  version    print version

run 'stratagema <command> -h' for a command's flags`)
}
