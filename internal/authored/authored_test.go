package authored_test

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
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

// valued stands for a refusal whose text quotes a declared value.
type valued struct{ value string }

func (v *valued) Error() string {
	return fmt.Sprintf("invalid interval %q: takes an ISO 8601 duration", v.value)
}
func (v *valued) Unquoted() string { return "invalid interval: takes an ISO 8601 duration" }

// Declared gives a source refusal the reason a host prints: the decoder's
// words around the refusal, which name the declaration by its key, and the
// refusal at the end of the chain, written without the value it quotes.
func TestDeclared_HappyPath(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		reason string
	}{
		{name: "a reason the decoder wraps", err: fmt.Errorf("topic %q: %w", "events", authored.New(`unknown key "flavor"`)),
			reason: `topic "events": unknown key "flavor"`},
		{name: "a value the refusal quotes", err: fmt.Errorf("topic %q: %w", "events", &valued{value: "DO_NOT_REPORT"}),
			reason: `topic "events": invalid interval: takes an ISO 8601 duration`},
		{name: "a refusal alone", err: &valued{value: "DO_NOT_REPORT"}, reason: "invalid interval: takes an ISO 8601 duration"},
		{name: "a key too long to repeat", err: fmt.Errorf("topic %q: %w", strings.Repeat("k", 600), &valued{value: "DO_NOT_REPORT"}),
			reason: "invalid interval: takes an ISO 8601 duration"},
		{name: "a refusal declared twice", err: authored.Declared(fmt.Errorf("topic %q: %w", "events", &valued{value: "DO_NOT_REPORT"})),
			reason: `topic "events": invalid interval: takes an ISO 8601 duration`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			err := authored.Declared(test.err)
			c.Assert(err, qt.ErrorMatches, regexp.QuoteMeta(test.err.Error()))
			c.Assert(reasons(c, authored.Status(codes.InvalidArgument, err)), qt.CmpEquals(protocmp.Transform()),
				[]*protocol.ProviderDiagnostic{{Reason: test.reason}})
		})
	}
}

// A refusal Declared cannot write without a value or a driver's text gives no
// reason, so a host prints its code alone: one with no reason at the end of
// its chain, one that joins two errors, and a quoted refusal another error
// wraps with text after it.
func TestDeclared_FailurePath(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "no error", err: nil},
		{name: "a refusal written by another package", err: fmt.Errorf("topic %q: %w", "events", errors.New(driverText))},
		{name: "two refusals joined", err: errors.Join(authored.New("a reason"), errors.New(driverText))},
		{name: "a quoted refusal with text after it", err: fmt.Errorf("%w on //ptah:schema:topic", &valued{value: "DO_NOT_REPORT"})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			err := authored.Declared(test.err)
			c.Assert(err, qt.Equals, test.err)
		})
	}
}
