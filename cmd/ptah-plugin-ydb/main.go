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
	if err := runtime.Serve(runtime.Server{Description: backend.Description(), Sessions: owner, Inspector: owner, ValueNormalizer: owner, ValueConverter: owner, ModelComparer: owner, FeaturePlanner: owner, TablePreparer: owner, Renderer: owner, SourceDecoder: owner, Executor: owner, Transactions: owner, History: owner, Locks: owner, Metadata: owner, SQLAnalyzer: owner}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
