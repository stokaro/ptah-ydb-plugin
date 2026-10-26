// Command ptah-plugin-ydb serves YDB through the experimental Ptah protocol.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/devdatabase"
	"github.com/stokaro/ptah/provider-sdk/runtime"
)

func main() {
	owner := backend.New()
	dev := devdatabase.NewDocker()
	// A host that crashed never releases its leases, so the containers go with
	// the process when it is told to stop.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-stop
		_ = dev.Close()
		os.Exit(1) //revive:disable-line:deep-exit the provider process must not outlive the signal that stopped it
	}()
	err := runtime.Serve(runtime.Server{DevProvisioner: dev, Description: backend.Description(), Sessions: owner, Inspector: owner, ValueNormalizer: owner, ValueConverter: owner, ModelComparer: owner, FeaturePlanner: owner, Declarations: owner, TablePreparer: owner, TableCreations: owner, SchemaValidator: owner, Renderer: owner, SourceDecoder: owner, SQLSchemaReader: owner, Executor: owner, ExecutionInputs: owner, Transactions: owner, Statements: owner, History: owner, Locks: owner, Metadata: owner, SQLAnalyzer: owner, Linter: owner, RowTyper: owner, RowReader: owner, RowWriter: owner, Seeds: owner, DevCleaner: owner, ReplayGuard: owner, EmbeddingStore: owner, CommonObjects: owner, SourceEncoder: owner})
	if closeErr := dev.Close(); closeErr != nil {
		fmt.Fprintln(os.Stderr, closeErr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
