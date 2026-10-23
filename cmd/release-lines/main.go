// Command release-lines prints the release lines the provider declares as the
// JSON document published beside each release.
package main

import (
	"fmt"
	"os"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/releasemanifest"
)

func main() {
	out, err := releasemanifest.Build(backend.Description())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
