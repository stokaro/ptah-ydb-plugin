// Command lintcontract reports how this module's .golangci.yml departs from the
// host's lint contract, one line per departure, and exits 1 when it does. CI
// runs it over the host's .golangci.yml at the revision scripts/resolve-host.sh
// names:
//
//	go run ./internal/cmd/lintcontract -host .host-contract/.golangci.yml
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/stokaro/ptah-ydb-plugin/internal/lintcontract"
)

func main() {
	host := flag.String("host", "", "the host's .golangci.yml")
	module := flag.String("module", ".golangci.yml", "this module's .golangci.yml")
	flag.Parse()
	if *host == "" {
		fmt.Fprintln(os.Stderr, "lintcontract: -host names no file")
		os.Exit(2)
	}
	hostConfig, err := os.ReadFile(*host)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lintcontract:", err)
		os.Exit(2)
	}
	moduleConfig, err := os.ReadFile(*module)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lintcontract:", err)
		os.Exit(2)
	}
	drift, err := lintcontract.Drift(hostConfig, moduleConfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lintcontract:", err)
		os.Exit(2)
	}
	for _, line := range drift {
		fmt.Println(line)
	}
	if len(drift) > 0 {
		fmt.Fprintf(os.Stderr, "lintcontract: %s departs from the host's lint contract in %d places\n", *module, len(drift))
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "lintcontract: %s keeps the host's lint contract\n", *module)
}
