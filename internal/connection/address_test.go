package connection_test

import (
	"cmp"
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
)

func TestAddress(t *testing.T) {
	for _, test := range []struct{ name, input, dsn, database string }{
		{"plaintext", "ydb://host/local", "grpc://host:2136/local", "/local"},
		{"TLS", "ydbs://host/ru/folder/database", "grpcs://host:2135/ru/folder/database", "/ru/folder/database"},
		{"IPv6", "ydb://[::1]:22136/local/", "grpc://[::1]:22136/local", "/local"},
		{"database parameter", "ydb://host?database=local&go_balancer=disable", "grpc://host:2136/local?go_balancer=disable", "/local"},
		{"matching database", "ydb://host/local?database=/local/", "grpc://host:2136/local", "/local"},
		{"token omitted", "ydb://host/local?token=test-secret", "grpc://host:2136/local", "/local"},
		{"environment credentials", "ydb://host/local?use_env_credentials=true", "grpc://host:2136/local", "/local"},
		{"bare environment credentials", "ydb://host/local?use_env_credentials", "grpc://host:2136/local", "/local"},
		{"environment credentials off", "ydb://host/local?use_env_credentials=false&token=test-secret", "grpc://host:2136/local", "/local"},
		{"password omitted", "ydbs://alice:test-secret@host/local", "grpcs://host:2135/local", "/local"},
		{"default idempotence", "ydb://host/local?go_default_idempotent=true", "grpc://host:2136/local?go_default_idempotent=true", "/local"},
		{"numeric idempotence", "ydb://host/local?go_default_idempotent=1", "grpc://host:2136/local?go_default_idempotent=1", "/local"},
		{"prefetch parts", "ydb://host/local?prefetch_query_result_parts=0", "grpc://host:2136/local?prefetch_query_result_parts=0", "/local"},
		{"legacy balancer", "ydb://host/local?balancer=disable", "grpc://host:2136/local?balancer=disable", "/local"},
		{"query mode", "ydb://host/local?go_query_mode=query&query_mode=query", "grpc://host:2136/local", "/local"},
		{"every SDK parameter", "ydbs://host?database=/ru/b1g&go_balancer=single&go_default_idempotent=false&prefetch_query_result_parts=3", "grpcs://host:2135/ru/b1g?go_balancer=single&go_default_idempotent=false&prefetch_query_result_parts=3", "/ru/b1g"},
		{"monitoring endpoint", "ydb://host/local?monitoring=http://host:8765", "grpc://host:2136/local", "/local"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			address, err := connection.Parse(test.input)
			c.Assert(err, qt.IsNil)
			c.Assert(address.String(), qt.Equals, test.dsn)
			c.Assert(fmt.Sprintf("%+v", address), qt.Equals, test.dsn)
			c.Assert(fmt.Sprintf("%#v", address), qt.Equals, test.dsn)
			c.Assert(address.Database(), qt.Equals, test.database)
		})
	}
}

func TestAddressRefusesBeforeConnecting(t *testing.T) {
	for _, test := range []struct{ name, input string }{
		{"malformed credentials", "ydb://user:test-secret%zz@host/local"},
		{"bad query escape", "ydb://host/local?token=test-secret%zz"},
		{"query semicolon", "ydb://host/local?token=test-secret;go_balancer=disable"},
		{"unsupported scheme", "postgres://user:test-secret@host/local"},
		{"missing database", "ydb://user:test-secret@host"},
		{"conflicting database", "ydb://host/local?database=test-secret"},
		{"duplicate token", "ydb://host/local?token=test-secret&token=other"},
		{"duplicate balancer", "ydb://host/local?go_balancer=disable&go_balancer=disable"},
		{"unknown parameter", "ydb://host/local?test-secret=true"},
		{"credential conflict", "ydb://alice:test-secret@host/local?token=other"},
		{"empty token", "ydb://host/local?token="},
		{"empty password", "ydb://alice:@host/local"},
		{"bad balancer", "ydb://host/local?go_balancer=test-secret"},
		{"empty balancer", "ydb://host/local?go_balancer="},
		{"no port", "ydb://host:0/local"},
		{"large port", "ydb://host:65536/local"},
		{"fragment", "ydb://host/local#test-secret"},
		{"parent directory", "ydb://host/local/../test-secret"},
		{"current directory", "ydb://host/local/./other"},
		{"repeated separator", "ydb://host/local//other"},
		{"backslash", "ydb://host/local%5cother"},
		{"NUL", "ydb://host/local%00other"},
		{"realm with a capital", "ydb://host/local?dev_realm=Test-Secret"},
		{"realm with a slash", "ydb://host/local?dev_realm=a/test-secret"},
		{"realm that climbs out", "ydb://host/local?dev_realm=.."},
		{"empty realm", "ydb://host/local?dev_realm="},
		{"duplicate realm", "ydb://host/local?dev_realm=r1&dev_realm=r2"},
		{"realm too long", "ydb://host/local?dev_realm=01234567890123456789012345678901234567890123456789012345678901234"},
		{"realm without a database", "ydb://host?dev_realm=r1"},
		{"monitoring not a URL scheme", "ydb://host/local?monitoring=ftp://host:8765"},
		{"monitoring without a scheme", "ydb://host/local?monitoring=host:8765"},
		{"monitoring empty", "ydb://host/local?monitoring="},
		{"monitoring without a host", "ydb://host/local?monitoring=http://"},
		{"monitoring with a user", "ydb://host/local?monitoring=http://alice:test-secret@host:8765"},
		{"monitoring with a path", "ydb://host/local?monitoring=http://host:8765/viewer"},
		{"monitoring with a query", "ydb://host/local?monitoring=http://host:8765%3Ftest-secret=1"},
		{"monitoring with a fragment", "ydb://host/local?monitoring=http://host:8765%23test-secret"},
		{"duplicate monitoring", "ydb://host/local?monitoring=http://host:8765&monitoring=http://host:8766"},
		{"legacy and current balancer", "ydb://host/local?balancer=disable&go_balancer=disable"},
		{"bad legacy balancer", "ydb://host/local?balancer=test-secret"},
		{"idempotent not a boolean", "ydb://host/local?go_default_idempotent=test-secret"},
		{"empty idempotent", "ydb://host/local?go_default_idempotent="},
		{"duplicate idempotent", "ydb://host/local?go_default_idempotent=true&go_default_idempotent=false"},
		{"prefetch not a number", "ydb://host/local?prefetch_query_result_parts=test-secret"},
		{"prefetch negative", "ydb://host/local?prefetch_query_result_parts=-1"},
		{"empty prefetch", "ydb://host/local?prefetch_query_result_parts="},
		{"table service query mode", "ydb://host/local?go_query_mode=test-secret"},
		{"table service legacy query mode", "ydb://host/local?query_mode=scripting"},
		{"faked transactions", "ydb://host/local?go_fake_tx=true"},
		{"query binders", "ydb://host/local?go_query_bind=test-secret"},
		{"environment credentials with a token", "ydb://host/local?use_env_credentials=true&token=test-secret"},
		{"environment credentials with a user", "ydb://alice:test-secret@host/local?use_env_credentials"},
		{"environment credentials not a boolean", "ydb://host/local?use_env_credentials=maybe"},
		{"duplicate environment credentials", "ydb://host/local?use_env_credentials&use_env_credentials"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			_, err := connection.Parse(test.input)
			c.Assert(err, qt.IsNotNil)
			c.Assert(err.Error(), qt.Not(qt.Contains), "test-secret")
		})
	}
}

func environment(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestCheckEnvironment_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]string
	}{
		{"access token", map[string]string{"YDB_ACCESS_TOKEN_CREDENTIALS": "test-secret"}},
		{"metadata", map[string]string{"YDB_METADATA_CREDENTIALS": "1"}},
		{"explicit anonymous", map[string]string{"YDB_ANONYMOUS_CREDENTIALS": "1"}},
		{"static", map[string]string{"YDB_STATIC_CREDENTIALS_USER": "alice", "YDB_STATIC_CREDENTIALS_PASSWORD": "test-secret", "YDB_STATIC_CREDENTIALS_ENDPOINT": "grpc://host:2136"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(connection.CheckEnvironment(environment(test.values)), qt.IsNil)
		})
	}
}

func TestCheckEnvironment_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, want string
		values     map[string]string
	}{
		{"nothing set", "no credential variable is", nil},
		{"metadata switch", "reads only 1", map[string]string{"YDB_METADATA_CREDENTIALS": "true"}},
		{"static without password", "without YDB_STATIC_CREDENTIALS_PASSWORD and", map[string]string{"YDB_STATIC_CREDENTIALS_USER": "alice"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			err := connection.CheckEnvironment(environment(test.values))
			c.Assert(err, qt.ErrorMatches, "(?s).*"+test.want+".*")
			c.Assert(err, qt.Not(qt.ErrorMatches), "(?s).*test-secret.*")
		})
	}
}

func TestAddressMonitoring(t *testing.T) {
	for _, test := range []struct{ name, input, want string }{
		{"none", "ydb://host/local", ""},
		{"plain", "ydb://host/local?monitoring=http://host:8765", "http://host:8765"},
		{"TLS", "ydbs://host/local?monitoring=https://host:8765", "https://host:8765"},
		{"trailing slash", "ydb://host/local?monitoring=http://host:8765/", "http://host:8765"},
		{"default port", "ydb://host/local?monitoring=http://host", "http://host"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			address, err := connection.Parse(test.input)
			c.Assert(err, qt.IsNil)
			c.Assert(fmt.Sprint(address.Monitoring()), qt.Equals, cmp.Or(test.want, "<nil>"))
		})
	}
}

func TestAddressMonitoring_CopyIsIndependent(t *testing.T) {
	c := qt.New(t)
	address, err := connection.Parse("ydb://host/local?monitoring=http://host:8765")
	c.Assert(err, qt.IsNil)
	address.Monitoring().Host = "elsewhere:1"
	c.Assert(address.Monitoring().Host, qt.Equals, "host:8765")
}

func TestAddressRefusalNamesAcceptedParameters(t *testing.T) {
	c := qt.New(t)
	_, err := connection.Parse("ydb://host/local?test-secret=true")
	c.Assert(err, qt.ErrorMatches, `unsupported YDB URL parameter; supported parameters: database, monitoring, dev_realm, token, use_env_credentials, go_balancer, balancer, go_query_mode, query_mode, go_default_idempotent, prefetch_query_result_parts`)
}

func TestAddressRealm(t *testing.T) {
	for _, test := range []struct{ name, input, realm, root, dsn string }{
		{"none", "ydb://host/local", "", "/local", "grpc://host:2136/local"},
		{"realm", "ydb://host/local?dev_realm=r1", "r1", "/local/ptah_dev/r1", "grpc://host:2136/local"},
		{"nested database", "ydbs://host/ru/b1g?dev_realm=abc_123", "abc_123", "/ru/b1g/ptah_dev/abc_123", "grpcs://host:2135/ru/b1g"},
		{"longest name", "ydb://host/local?dev_realm=0123456789012345678901234567890123456789012345678901234567890123", "0123456789012345678901234567890123456789012345678901234567890123", "/local/ptah_dev/0123456789012345678901234567890123456789012345678901234567890123", "grpc://host:2136/local"},
		{"with SDK parameters", "ydb://host/local?dev_realm=r1&go_balancer=disable&monitoring=http://host:8765", "r1", "/local/ptah_dev/r1", "grpc://host:2136/local?go_balancer=disable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			address, err := connection.Parse(test.input)
			c.Assert(err, qt.IsNil)
			c.Assert(address.Realm(), qt.Equals, test.realm)
			c.Assert(address.Root(), qt.Equals, test.root)
			c.Assert(address.String(), qt.Equals, test.dsn)
		})
	}
}
