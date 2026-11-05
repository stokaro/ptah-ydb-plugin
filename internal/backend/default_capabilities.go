package backend

import (
	"maps"

	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
)

// offlineLine is the release line an offline command plans YDB with: the
// newest certified line, with the cluster's default feature flags.
const offlineLine = "26.2"

// defaultCapabilities are the facts the host plans the ydb engine with when no
// session states a server's (contract 25.27, engine.default-capabilities): an
// offline render, generation or export. They are the facts a session states
// for a server of offlineLine whose address names no monitoring endpoint,
// with the facts of the common objects the host carries on the common wire.
// A host that negotiates neither the common wire nor the feature never reads
// them. These are the facts of the built-in engine's YDB262 preset for the
// keys this provider reads.
func defaultCapabilities() map[string]bool {
	return lineCapabilities(offlineLine)
}

// lineCapabilities are the facts a session states for a server of line whose
// address names no monitoring endpoint, with the facts of the common objects:
// what the host plans with for a --server-version that names the line
// (ReleaseLine.capabilities, contract 25.27).
func lineCapabilities(line string) map[string]bool {
	result := sessionCapabilities(line)
	maps.Copy(result, commonobject.Capabilities())
	return result
}
