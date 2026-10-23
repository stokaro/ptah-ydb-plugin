// Package releasemanifest renders the release lines a provider declares as
// the JSON document published beside each release, so a host can read them
// without starting the provider.
package releasemanifest

import (
	"encoding/json"
	"fmt"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Line is one release line of an engine.
type Line struct {
	Line     string `json:"line"`
	Label    string `json:"label"`
	Support  string `json:"support"`
	Image    string `json:"image,omitempty"`
	Unprobed string `json:"unprobed,omitempty"`
	Note     string `json:"note,omitempty"`
}

// Engine is the set of release lines one engine declares.
type Engine struct {
	ID    string `json:"engine"`
	Lines []Line `json:"lines"`
}

// Manifest is the document published as release-lines.json.
type Manifest struct {
	Provider string   `json:"provider"`
	Engines  []Engine `json:"engines"`
}

// Build renders the release lines of every engine in description. An engine
// that declares none is left out. The output is indented and ends with a
// newline, and two calls over the same description return the same bytes.
func Build(description *protocol.Description) ([]byte, error) {
	manifest := Manifest{Provider: description.GetProviderId(), Engines: []Engine{}}
	for _, engine := range description.GetEngines() {
		if len(engine.GetReleaseLines()) == 0 {
			continue
		}
		lines := make([]Line, len(engine.GetReleaseLines()))
		for i, line := range engine.GetReleaseLines() {
			lines[i] = Line{
				Line: line.GetLine(), Label: line.GetLabel(), Support: strings.ToLower(line.GetSupport().String()),
				Image: line.GetImage(), Unprobed: line.GetUnprobed(), Note: line.GetNote(),
			}
		}
		manifest.Engines = append(manifest.Engines, Engine{ID: engine.GetId(), Lines: lines})
	}
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render release lines: %w", err)
	}
	return append(out, '\n'), nil
}
