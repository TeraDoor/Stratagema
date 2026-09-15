package main

import (
	"fmt"
	"os"
)

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
	case "lock":
		code = cmdLock(rest)
	case "interest":
		code = cmdInterest(rest)
	case "serve":
		code = cmdServe(rest)
	case "version":
		fmt.Println("stratagema 0.1.0-dev")
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
  lock       claim, free, and inspect a named resource lock (acquire|release|status|list)
  interest   subscribe to a resource and read/acknowledge notifications (create|list|pause|resume|inbox|ack)
  serve      optional: push interest notifications live over SSE
  version    print version

run 'stratagema <command> -h' for a command's flags`)
}
