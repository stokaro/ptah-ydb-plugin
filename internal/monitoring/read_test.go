package monitoring_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/monitoring"
)

// serve starts an endpoint that answers every request with handler and returns
// its address.
func serve(c *qt.C, handler http.HandlerFunc) *url.URL {
	c.Helper()
	server := httptest.NewServer(handler)
	c.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	c.Assert(err, qt.IsNil)
	return endpoint
}

// request is what an endpoint received.
type request struct {
	path, query, authorization, accept string
}

func recording(received *atomic.Pointer[request], body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		received.Store(&request{path: r.URL.Path, query: r.URL.RawQuery, authorization: r.Header.Get("Authorization"), accept: r.Header.Get("Accept")})
		_, _ = w.Write([]byte(body))
	}
}

// TestRead_SendsTheConnectionCredential pins the credential the page is read
// with: the ticket travels as YDB's bare Authorization header. A cluster that
// enforces authentication serves the page only to a user it knows.
func TestRead_SendsTheConnectionCredential(t *testing.T) {
	c := qt.New(t)
	var received atomic.Pointer[request]
	endpoint := serve(c, recording(&received, `{"Version":2}`))
	body, err := monitoring.Read(c.Context(), endpoint, "feature flags", "/viewer/json/feature_flags", "/local", "ticket-value", url.Values{"database": {"/local"}})
	c.Assert(err, qt.IsNil)
	c.Assert(string(body), qt.Equals, `{"Version":2}`)
	c.Assert(*received.Load(), qt.Equals, request{path: "/viewer/json/feature_flags", query: "database=%2Flocal", authorization: "ticket-value", accept: "application/json"})
}

// TestRead_SendsNoCredentialForAnAnonymousConnection pins the other half: an
// anonymous connection has no ticket, and no header is invented for it.
func TestRead_SendsNoCredentialForAnAnonymousConnection(t *testing.T) {
	c := qt.New(t)
	var received atomic.Pointer[request]
	endpoint := serve(c, recording(&received, `{}`))
	_, err := monitoring.Read(c.Context(), endpoint, "feature flags", "/viewer/json/feature_flags", "/local", "", nil)
	c.Assert(err, qt.IsNil)
	c.Assert(received.Load().authorization, qt.Equals, "")
}

// TestRead_ReadsThePageItNames replaces whatever path and query the endpoint
// carries with the page's own.
func TestRead_ReadsThePageItNames(t *testing.T) {
	c := qt.New(t)
	var received atomic.Pointer[request]
	endpoint := serve(c, recording(&received, `{}`))
	endpoint.Path, endpoint.RawQuery, endpoint.Fragment = "/other", "page=1", "top"
	_, err := monitoring.Read(c.Context(), endpoint, "column table", "/viewer/json/describe", "/local", "", url.Values{"path": {"/local/a b"}, "database": {"/local"}})
	c.Assert(err, qt.IsNil)
	c.Assert(received.Load().path, qt.Equals, "/viewer/json/describe")
	c.Assert(received.Load().query, qt.Equals, "database=%2Flocal&path=%2Flocal%2Fa+b")
}

// TestRead_FollowsNoRedirect refuses a page that moved: the redirect is
// answered as the status it is, and the place it points at receives nothing,
// so the credential never leaves for an endpoint the operator did not name.
func TestRead_FollowsNoRedirect(t *testing.T) {
	c := qt.New(t)
	var reached atomic.Int32
	elsewhere := serve(c, func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte(`{}`))
	})
	endpoint := serve(c, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.String()+"/viewer/json/feature_flags", http.StatusFound)
	})
	body, err := monitoring.Read(c.Context(), endpoint, "feature flags", "/viewer/json/feature_flags", "/local", "ticket-value", nil)
	c.Assert(err, qt.ErrorMatches, `read the YDB feature flags from the monitoring endpoint: 302 Found: .*`)
	c.Assert(body, qt.IsNil)
	c.Assert(reached.Load(), qt.Equals, int32(0))
}

// TestRead_RefusesAnOversizedPage bounds the page at MaxBody.
func TestRead_RefusesAnOversizedPage(t *testing.T) {
	c := qt.New(t)
	endpoint := serve(c, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", monitoring.MaxBody+1)))
	})
	body, err := monitoring.Read(c.Context(), endpoint, "column table", "/viewer/json/describe", "/local", "", nil)
	c.Assert(err, qt.ErrorMatches, `read the YDB column table from the monitoring endpoint: the page exceeds 4194304 bytes`)
	c.Assert(body, qt.IsNil)
}

// TestRead_AcceptsAPageAtTheBound reads a page of exactly MaxBody bytes.
func TestRead_AcceptsAPageAtTheBound(t *testing.T) {
	c := qt.New(t)
	endpoint := serve(c, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", monitoring.MaxBody)))
	})
	body, err := monitoring.Read(c.Context(), endpoint, "column table", "/viewer/json/describe", "/local", "", nil)
	c.Assert(err, qt.IsNil)
	c.Assert(body, qt.HasLen, monitoring.MaxBody)
}

// TestRead_RefusalRepeatsNeitherTheTicketNorTheEndpoint holds the error to
// the endpoint's own message: an endpoint that echoes the request's headers
// would otherwise repeat the credential.
func TestRead_RefusalRepeatsNeitherTheTicketNorTheEndpoint(t *testing.T) {
	c := qt.New(t)
	endpoint := serve(c, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("Failed to resolve database for " + r.Header.Get("Authorization")))
	})
	_, err := monitoring.Read(c.Context(), endpoint, "feature flags", "/viewer/json/feature_flags", "/local", "ticket-value", nil)
	c.Assert(err, qt.ErrorMatches, `read the YDB feature flags from the monitoring endpoint: 400 Bad Request: Failed to resolve database for <redacted>; `+
		`the page is read as the connection's user, who needs DESCRIBE SCHEMA on /local`)
	c.Assert(strings.Contains(err.Error(), "ticket-value"), qt.IsFalse)
	c.Assert(strings.Contains(err.Error(), endpoint.Host), qt.IsFalse)
}

// TestRead_RefusalOfAnUnreachableEndpointNamesNoAddress keeps the endpoint
// out of a transport error, which net/http would spell with the whole URL.
func TestRead_RefusalOfAnUnreachableEndpointNamesNoAddress(t *testing.T) {
	c := qt.New(t)
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint, err := url.Parse(server.URL)
	c.Assert(err, qt.IsNil)
	server.Close()
	_, err = monitoring.Read(c.Context(), endpoint, "feature flags", "/viewer/json/feature_flags", "/local", "", nil)
	c.Assert(err, qt.ErrorMatches, `read the YDB feature flags from the monitoring endpoint: the endpoint did not answer`)
}

// TestRead_RefusesWithoutAnEndpoint names the missing endpoint.
func TestRead_RefusesWithoutAnEndpoint(t *testing.T) {
	c := qt.New(t)
	body, err := monitoring.Read(c.Context(), nil, "column table", "/viewer/json/describe", "/local", "", nil)
	c.Assert(err, qt.ErrorMatches, `no monitoring endpoint to read the YDB column table from`)
	c.Assert(body, qt.IsNil)
}
