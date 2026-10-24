// Package featureflags reads the feature flags a YDB database runs with from
// the monitoring endpoint's page and turns the flags that gate a capability
// the provider owns into that capability's value in the session.
//
// A release line runs with default flags, and an operator can turn a flag on
// or off, so one cluster can do more or less than its line's defaults say. The
// cluster's own page is the answer: `GET /viewer/json/feature_flags?database=<db>`
// lists every flag the database knows with its default and, where it was set,
// its current value. The page layout and the gate of each capability are the
// host's embedded engine's (internal/ydbflags in Ptah).
package featureflags

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Path is the monitoring page the flags are read from.
const Path = "/viewer/json/feature_flags"

// Flags is the value of every flag one database lists: the current value where
// the cluster set one, and the default otherwise.
type Flags map[string]bool

// Gate is one session capability a YDB feature flag decides.
type Gate struct {
	// Key is the capability, spelled as the host's vocabulary spells it.
	Key string
	// Flag is the flag's name as the monitoring page spells it.
	Flag string
}

// gates are the flags that decide a capability the provider plans with.
//
// tiered_ttl follows EnableTieringInColumnShard. The flag is off by default on
// every line from 25.1.4.7 to 26.2.1.14, and with it off a column table's TTL
// tier that moves rows to an external data source is refused with `Tiering
// functionality is disabled for OLAP tables` (measured on 26.2.1.14; turned on
// through the dynamic configuration, the same CREATE TABLE succeeds).
//
// The host maps more flags than these. The provider reports only the
// capabilities it owns, because the host plans the others itself: an
// index_rename the host read as true would make the host plan a RENAME INDEX
// that the provider's alteration does not carry out.
var gates = []Gate{
	{Key: "tiered_ttl", Flag: "EnableTieringInColumnShard"},
}

// Gates returns every capability a flag decides, in a fixed order.
func Gates() []Gate { return slices.Clone(gates) }

// Refine returns caps with every gated capability set from its flag, where the
// flags list it. A flag the database does not list leaves its capability as
// caps has it. caps is not changed.
func (f Flags) Refine(caps map[string]bool) map[string]bool {
	refined := maps.Clone(caps)
	if refined == nil {
		refined = make(map[string]bool, len(gates))
	}
	for _, gate := range gates {
		if value, listed := f[gate.Flag]; listed {
			refined[gate.Key] = value
		}
	}
	return refined
}

// pageVersion is the only page layout the provider reads, the one local-ydb
// serves from 25.1.4.7 to 26.2.1.14.
const pageVersion = 2

// page is the monitoring endpoint's answer.
type page struct {
	Version   int `json:"Version"`
	Databases []struct {
		Name         string `json:"Name"`
		FeatureFlags []struct {
			Name    string `json:"Name"`
			Default *bool  `json:"Default"`
			Current *bool  `json:"Current"`
		} `json:"FeatureFlags"`
	} `json:"Databases"`
}

// Decode reads the flags of database out of a feature-flags page. A page in
// another layout, one that does not list the database, a flag listed twice and
// a flag with no value are refused rather than read as flags that are off.
func Decode(body []byte, database string) (Flags, error) {
	var decoded page
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("the feature-flags page is not the JSON the provider reads")
	}
	if decoded.Version != pageVersion {
		return nil, fmt.Errorf("the feature-flags page has version %d, and the provider reads version %d", decoded.Version, pageVersion)
	}
	want := "/" + strings.Trim(database, "/")
	for _, listed := range decoded.Databases {
		if "/"+strings.Trim(listed.Name, "/") != want {
			continue
		}
		flags := make(Flags, len(listed.FeatureFlags))
		for _, flag := range listed.FeatureFlags {
			if _, repeated := flags[flag.Name]; repeated {
				return nil, fmt.Errorf("the feature-flags page lists %s twice", flag.Name)
			}
			switch {
			case flag.Current != nil:
				flags[flag.Name] = *flag.Current
			case flag.Default != nil:
				flags[flag.Name] = *flag.Default
			default:
				return nil, fmt.Errorf("the feature-flags page lists %s with no value", flag.Name)
			}
		}
		return flags, nil
	}
	return nil, fmt.Errorf("the feature-flags page does not list database %s", want)
}
