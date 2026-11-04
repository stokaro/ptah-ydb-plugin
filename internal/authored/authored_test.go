package authored_test

import (
	"errors"
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
)

// driverText stands for a driver's error, which can quote a credential.
const driverText = "transport: dial ydb://admin:hunter2@db:2136/local: connection refused" // #nosec G101 -- a fake credential a host must never show

// reasons returns the diagnostics a status carries.
func reasons(c *qt.C, err error) []*protocol.ProviderDiagnostic {
	c.Helper()
	var found []*protocol.ProviderDiagnostic
	for _, detail := range status.Convert(err).Details() {
		diagnostic, ok := detail.(*protocol.ProviderDiagnostic)
		c.Assert(ok, qt.IsTrue, qt.Commentf("detail %T", detail))
		found = append(found, diagnostic)
	}
	return found
}

// A status built from an error that holds a reason carries the reason alone,
// and keeps the whole error's text as its message, which no host prints.
func TestStatus_HappyPath(t *testing.T) {
	sentinel := errors.New("a sentinel")
	tests := []struct {
		name, message string
		err           error
	}{
		{name: "a reason", err: authored.New("the streaming query capability was not established"),
			message: "the streaming query capability was not established"},
		{name: "a reason an outer error wraps", err: fmt.Errorf("compare %s: %w", driverText, authored.New("the streaming query capability was not established")),
			message: "compare " + driverText + ": the streaming query capability was not established"},
		{name: "a reason joined to a driver's error", err: errors.Join(errors.New(driverText), authored.New("the streaming query capability was not established")),
			message: driverText + "\nthe streaming query capability was not established"},
		{name: "a reason that wraps a sentinel", err: authored.Wrap(sentinel, "the streaming query capability was not established"),
			message: "the streaming query capability was not established"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			err := authored.Status(codes.InvalidArgument, test.err)

			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(status.Convert(err).Message(), qt.Equals, test.message)
			c.Assert(reasons(c, err), qt.CmpEquals(protocmp.Transform()),
				[]*protocol.ProviderDiagnostic{{Reason: "the streaming query capability was not established"}})
		})
	}
}

// An error that holds no reason becomes a status with its text as the
// message and no diagnostic, so a host prints its code alone.
func TestStatus_FailurePath(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "a driver's error", err: errors.New(driverText)},
		{name: "a wrapped driver's error", err: fmt.Errorf("open YDB connection: %w", errors.New(driverText))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			err := authored.Status(codes.Unavailable, test.err)

			c.Assert(status.Code(err), qt.Equals, codes.Unavailable)
			c.Assert(status.Convert(err).Message(), qt.Equals, test.err.Error())
			c.Assert(reasons(c, err), qt.HasLen, 0)
		})
	}
}

// Wrap keeps the sentinel for errors.Is and leaves its text out.
func TestWrapMatchesItsCause(t *testing.T) {
	c := testcheck.New(t)
	sentinel := errors.New("YDB refused this user Ptah's lock")
	err := authored.Wrap(sentinel, "this user may not use the YDB coordination node /local/ptah_locks")
	c.Assert(err, qt.ErrorIs, sentinel)
	c.Assert(err, qt.ErrorMatches, "this user may not use the YDB coordination node /local/ptah_locks")
}
