package connection_test

import (
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
		{"password omitted", "ydbs://alice:test-secret@host/local", "grpcs://host:2135/local", "/local"},
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
		{"unimplemented realm", "ydb://host/local?dev_realm=test-secret"},
		{"unimplemented monitoring", "ydb://host/local?monitoring=http://host:8765"},
		{"unimplemented environment credentials", "ydb://host/local?use_env_credentials=true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			_, err := connection.Parse(test.input)
			c.Assert(err, qt.IsNotNil)
			c.Assert(err.Error(), qt.Not(qt.Contains), "test-secret")
		})
	}
}
