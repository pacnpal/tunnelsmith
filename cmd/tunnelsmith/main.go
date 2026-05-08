package main

import (
	"fmt"
	"os"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if _, err := fmt.Fprintf(os.Stdout, "tunnelsmith %s (commit %s, built %s)\n", version, commit, date); err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		os.Exit(1)
	}
}
