// Command ptah-plugin-ydb serves YDB through the experimental Ptah protocol.
package main

import (
	"fmt"
	"os"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah/provider-sdk/runtime"
)

func main() {
	owner := backend.New()
	if err := runtime.Serve(runtime.Server{Description: backend.Description(), Sessions: owner, Inspector: owner, ValueNormalizer: owner}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
