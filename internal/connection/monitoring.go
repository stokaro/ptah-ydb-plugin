package connection

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/stokaro/ptah-ydb-plugin/internal/monitoring"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// ErrNoMonitoring reports a session whose address names no monitoring
// endpoint, so a page only that endpoint serves cannot be read.
var ErrNoMonitoring = errors.New("the YDB address names no monitoring endpoint: add monitoring=http(s)://host:port")

// ticketHeader is the gRPC metadata key a YDB client presents its credential
// under: the token a static user's login returned, an access token as it was
// given, or the token an environment credential produced.
const ticketHeader = "x-ydb-auth-ticket"

// ticketProbe holds the credential the call Ticket makes carried.
type ticketProbe struct {
	ticket string
}

// ticketProbeKey is the context key a ticketProbe travels under.
type ticketProbeKey struct{}

// recordTicket is a unary interceptor on every connection Open makes. It reads
// the credential only from a call whose context carries a ticketProbe, which
// only Ticket's call does, and passes every call on unchanged.
func recordTicket(ctx context.Context, method string, request, reply any, conn *grpc.ClientConn, invoker grpc.UnaryInvoker, options ...grpc.CallOption) error {
	if probe, ok := ctx.Value(ticketProbeKey{}).(*ticketProbe); ok {
		if outgoing, ok := metadata.FromOutgoingContext(ctx); ok {
			if values := outgoing.Get(ticketHeader); len(values) > 0 {
				probe.ticket = values[len(values)-1]
			}
		}
	}
	return invoker(ctx, method, request, reply, conn, options...)
}

// Ticket returns the credential the connection presents to the server, as the
// server receives it, or "" for a connection the address gives no credential.
//
// The YDB SDK builds the credential inside the driver and never hands it out:
// a static user's token comes from a login the driver makes, and an
// environment credential from a provider it builds. So Ticket asks the server
// who the connection is, and reads the credential the SDK attached to that
// call. The monitoring endpoint reads the same credential from an
// Authorization header.
//
// The ticket is a secret. A caller sends it and never prints it.
func (c *Connection) Ticket(ctx context.Context) (string, error) {
	if !c.authenticated {
		return "", nil
	}
	probe := &ticketProbe{}
	if _, err := c.Driver.Discovery().WhoAmI(context.WithValue(ctx, ticketProbeKey{}, probe)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", errors.New("ask YDB which user the connection is: the call failed")
	}
	return probe.ticket, nil
}

// HasMonitoring reports whether the address names a monitoring endpoint.
func (c *Connection) HasMonitoring() bool { return c.monitoring != nil }

// ReadMonitoring reads one JSON page of the monitoring endpoint the address
// names with the connection's own credential; see monitoring.Read for the
// bounds. Without an endpoint it returns ErrNoMonitoring. A credential that
// travels over TLS is never sent to a plain http:// endpoint.
func (c *Connection) ReadMonitoring(ctx context.Context, resource, page string, query url.Values) ([]byte, error) {
	if c.monitoring == nil {
		return nil, ErrNoMonitoring
	}
	ticket, err := c.ticket(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the YDB %s: %w", resource, err)
	}
	if err := sendable(ticket, c.secure, c.monitoring); err != nil {
		return nil, fmt.Errorf("read the YDB %s: %w", resource, err)
	}
	return monitoring.Read(ctx, c.monitoring, resource, page, c.database, ticket, query)
}

// sendable refuses to send a credential that travels over TLS to a plain
// http:// endpoint, where anyone on the path reads it. The refusal names the
// scheme to use and never the endpoint.
func sendable(ticket string, secure bool, endpoint *url.URL) error {
	if ticket != "" && secure && endpoint.Scheme != "https" {
		return errors.New("the connection's credential travels over TLS, and the provider does not send it to a plain http:// monitoring endpoint; name the endpoint with https://")
	}
	return nil
}

// Database returns the database the connection opened, the one the monitoring
// pages are read for. A session on a dev realm reads the realm's database.
func (c *Connection) Database() string { return c.database }
