package connection

// White-box testing required: credentials is unexported and its choice between
// the environment and anonymous access is observable only through the SDK option.

import (
	"testing"

	qt "github.com/frankban/quicktest"
)

func TestAddressCredentials_HappyPath(t *testing.T) {
	c := qt.New(t)
	c.Setenv("YDB_ACCESS_TOKEN_CREDENTIALS", "test-secret")
	address, err := Parse("ydb://host/local?use_env_credentials=true")
	c.Assert(err, qt.IsNil)
	option, err := address.credentials()
	c.Assert(err, qt.IsNil)
	c.Assert(option, qt.IsNotNil)
}

func TestAddressCredentials_FailurePath(t *testing.T) {
	c := qt.New(t)
	c.Setenv("YDB_METADATA_CREDENTIALS", "true")
	address, err := Parse("ydb://host/local?use_env_credentials=true")
	c.Assert(err, qt.IsNil)
	option, err := address.credentials()
	c.Assert(err, qt.ErrorMatches, ".*reads only 1")
	c.Assert(option, qt.IsNil)
}
