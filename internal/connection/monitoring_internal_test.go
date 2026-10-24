package connection

// White-box testing required: the credential a monitoring read sends comes
// from the SDK driver, which only a live server can stand behind, so the test
// supplies the ticket and the endpoint a connection Open would hold.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	qt "github.com/frankban/quicktest"
)

// monitored returns a connection whose monitoring endpoint answers every page
// with body, recording the Authorization header it received.
func monitored(c *qt.C, secure bool, ticket string, received *atomic.Pointer[string]) *Connection {
	c.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		received.Store(&header)
		_, _ = w.Write([]byte(`{}`))
	}))
	c.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	c.Assert(err, qt.IsNil)
	return &Connection{database: "/local", monitoring: endpoint, secure: secure, authenticated: ticket != "",
		ticket: func(context.Context) (string, error) { return ticket, nil }}
}

// TestReadMonitoring_SendsTheConnectionCredential reads a page with the
// credential the connection presents to the database.
func TestReadMonitoring_SendsTheConnectionCredential(t *testing.T) {
	c := qt.New(t)
	var received atomic.Pointer[string]
	opened := monitored(c, false, "ticket-value", &received)
	body, err := opened.ReadMonitoring(c.Context(), "feature flags", "/viewer/json/feature_flags", url.Values{"database": {"/local"}})
	c.Assert(err, qt.IsNil)
	c.Assert(string(body), qt.Equals, `{}`)
	c.Assert(*received.Load(), qt.Equals, "ticket-value")
}

// TestReadMonitoring_RefusesAPlainEndpointForATLSCredential keeps a credential
// that travels over TLS off a plain http:// endpoint, before any request.
func TestReadMonitoring_RefusesAPlainEndpointForATLSCredential(t *testing.T) {
	c := qt.New(t)
	var received atomic.Pointer[string]
	opened := monitored(c, true, "ticket-value", &received)
	body, err := opened.ReadMonitoring(c.Context(), "feature flags", "/viewer/json/feature_flags", nil)
	c.Assert(err, qt.ErrorMatches, `read the YDB feature flags: the connection's credential travels over TLS, and the provider does not send it to a plain http:// monitoring endpoint; name the endpoint with https://`)
	c.Assert(body, qt.IsNil)
	c.Assert(received.Load(), qt.IsNil)
}

// TestReadMonitoring_AnonymousTLSConnectionReadsAPlainEndpoint has no
// credential to protect, as the host's embedded engine reads it.
func TestReadMonitoring_AnonymousTLSConnectionReadsAPlainEndpoint(t *testing.T) {
	c := qt.New(t)
	var received atomic.Pointer[string]
	opened := monitored(c, true, "", &received)
	_, err := opened.ReadMonitoring(c.Context(), "feature flags", "/viewer/json/feature_flags", nil)
	c.Assert(err, qt.IsNil)
	c.Assert(*received.Load(), qt.Equals, "")
}

// TestReadMonitoring_RefusesWithoutAnEndpoint returns the sentinel callers
// turn into a refusal that names the parameter.
func TestReadMonitoring_RefusesWithoutAnEndpoint(t *testing.T) {
	c := qt.New(t)
	body, err := (&Connection{database: "/local"}).ReadMonitoring(c.Context(), "column table", "/viewer/json/describe", nil)
	c.Assert(err, qt.ErrorIs, ErrNoMonitoring)
	c.Assert(body, qt.IsNil)
}

func TestSendable(t *testing.T) {
	https, err := url.Parse("https://host:8765")
	qt.New(t).Assert(err, qt.IsNil)
	plain, err := url.Parse("http://host:8765")
	qt.New(t).Assert(err, qt.IsNil)
	for _, test := range []struct {
		name     string
		ticket   string
		secure   bool
		endpoint *url.URL
		refused  bool
	}{
		{name: "TLS credential to https", ticket: "t", secure: true, endpoint: https},
		{name: "TLS credential to http", ticket: "t", secure: true, endpoint: plain, refused: true},
		{name: "plain credential to http", ticket: "t", endpoint: plain},
		{name: "anonymous TLS to http", secure: true, endpoint: plain},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(sendable(test.ticket, test.secure, test.endpoint) != nil, qt.Equals, test.refused)
		})
	}
}
