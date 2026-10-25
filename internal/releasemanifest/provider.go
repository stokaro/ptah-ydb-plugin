package releasemanifest

import (
	"encoding/json"
	"fmt"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// ProviderDocumentName is the release asset a host reads to choose a
// release without downloading an archive.
const ProviderDocumentName = "ptah-provider.json"

// Platform is one operating system and architecture a release ships an
// archive for.
type Platform struct {
	OS   string
	Arch string
}

// Platforms lists the archives every release ships, in the order
// .goreleaser.yaml builds them. The release workflow checks this list against
// the archives GoReleaser writes, so the two cannot drift apart unnoticed.
var Platforms = []Platform{
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"linux", "amd64"}, {"linux", "arm64"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

// Artifact names the archive for one platform and the executable inside it.
// Its digest is the one checksums.txt lists for Archive.
type Artifact struct {
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Archive    string `json:"archive"`
	Executable string `json:"executable"`
}

// ContractVersion is a provider contract version. The major version changes
// only on an incompatible change; the minor version grows with each
// compatible addition.
type ContractVersion struct {
	Major uint32 `json:"major"`
	Minor uint32 `json:"minor"`
}

// ProviderDocument is the ptah-provider.json asset of a release: who the
// provider is, the oldest provider contract a host must implement to start it
// and which archives the release carries. A host starts the release when its
// contract has the same major version and a minor version at least
// MinContract.Minor.
type ProviderDocument struct {
	Schema      int             `json:"schema"`
	ProviderID  string          `json:"provider_id"`
	Version     string          `json:"version"`
	MinContract ContractVersion `json:"min_contract"`
	Engines     []string        `json:"engines"`
	Artifacts   []Artifact      `json:"artifacts"`
}

// BuildProvider renders the ptah-provider.json document for a release. The
// minimum contract is the one description declares, so the manifest and the
// provider's Describe reply cannot disagree about which hosts can start it.
// version is the tag without its leading "v", as GoReleaser's .Version is;
// project and executable are the archive prefix and the binary name.
func BuildProvider(description *protocol.Description, version, project, executable string) ([]byte, error) {
	if version == "" || strings.HasPrefix(version, "v") {
		return nil, fmt.Errorf("release version %q must be the tag without its leading v", version)
	}
	minimum := provider.MinimumContract(description)
	doc := ProviderDocument{
		Schema:      1,
		ProviderID:  description.GetProviderId(),
		Version:     version,
		MinContract: ContractVersion{Major: minimum.GetMajor(), Minor: minimum.GetMinor()},
	}
	for _, engine := range description.GetEngines() {
		doc.Engines = append(doc.Engines, engine.GetId())
	}
	for _, platform := range Platforms {
		archive := fmt.Sprintf("%s_%s_%s_%s.tar.gz", project, version, platform.OS, platform.Arch)
		binary := executable
		if platform.OS == "windows" {
			archive = strings.TrimSuffix(archive, ".tar.gz") + ".zip"
			binary += ".exe"
		}
		doc.Artifacts = append(doc.Artifacts, Artifact{OS: platform.OS, Arch: platform.Arch, Archive: archive, Executable: binary})
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
