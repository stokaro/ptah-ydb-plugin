package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

func TestUnofferedFeature_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	query := &protocol.Envelope{Kind: schema.StreamingQueryProperties, RequiredFeatures: []string{provider.PlanningObjectContextFeature}}
	plain := &protocol.Envelope{Kind: schema.TopicProperties}
	value, feature := schema.UnofferedFeature([]string{provider.ObservedColumnsFeature, provider.PlanningObjectContextFeature}, plain, query)
	c.Assert(value, qt.IsNil)
	c.Assert(feature, qt.Equals, "")
}

func TestUnofferedFeature_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	query := &protocol.Envelope{Kind: schema.StreamingQueryProperties, RequiredFeatures: []string{provider.PlanningObjectContextFeature}}
	plain := &protocol.Envelope{Kind: schema.TopicProperties}
	value, feature := schema.UnofferedFeature([]string{provider.ObservedColumnsFeature}, plain, query)
	c.Assert(value, qt.Equals, query)
	c.Assert(feature, qt.Equals, provider.PlanningObjectContextFeature)
}
