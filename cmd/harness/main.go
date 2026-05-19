// Command harness is the CLI entry point. It defers all argument
// parsing and dispatch to internal/cli; this file exists only to bind
// the process exit code to the CLI's reported result.
package main

import (
	"os"

	"github.com/victor-develop/advanced-tasker/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
