package monitoring_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/monitoring"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
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
	c := testcheck.New(t)
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
	c := testcheck.New(t)
	var received atomic.Pointer[request]
	endpoint := serve(c, recording(&received, `{}`))
	_, err := monitoring.Read(c.Context(), endpoint, "feature flags", "/viewer/json/feature_flags", "/local", "", nil)
	c.Assert(err, qt.IsNil)
	c.Assert(received.Load().authorization, qt.Equals, "")
}

// TestRead_ReadsThePageItNames replaces whatever path and query the endpoint
// carries with the page's own.
func TestRead_ReadsThePageItNames(t *testing.T) {
	c := testcheck.New(t)
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
	c := testcheck.New(t)
	var reached atomic.Int32
	elsewhere := serve(c, func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte(`{}`))
	})
	endpoint := serve(c, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.String()+"/viewer/json/feature_flags", http.StatusFound)
	})
	body, err := monitoring.Read(c.Context(), endpoint, "feature flags", "/viewer/json/feature_flags", "/local", "ticket-value", nil)
	c.Assert(err, qt.ErrorMatches, `read the YDB feature flags from the monitoring endpoint: 302 Found; the endpoint answered: .*`)
	c.Assert(body, qt.IsNil)
	c.Assert(reached.Load(), qt.Equals, int32(0))
}

// TestRead_RefusesAnOversizedPage bounds the page at MaxBody.
func TestRead_RefusesAnOversizedPage(t *testing.T) {
	c := testcheck.New(t)
	endpoint := serve(c, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", monitoring.MaxBody+1)))
	})
	body, err := monitoring.Read(c.Context(), endpoint, "column table", "/viewer/json/describe", "/local", "", nil)
	c.Assert(err, qt.ErrorMatches, `read the YDB column table from the monitoring endpoint: the page exceeds 4194304 bytes`)
	c.Assert(body, qt.IsNil)
}

// TestRead_AcceptsAPageAtTheBound reads a page of exactly MaxBody bytes.
func TestRead_AcceptsAPageAtTheBound(t *testing.T) {
	c := testcheck.New(t)
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
	c := testcheck.New(t)
	endpoint := serve(c, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("Failed to resolve database for " + r.Header.Get("Authorization"))) // #nosec G705 -- the test server echoes the header on purpose: the test holds Read to redacting it
	})
	_, err := monitoring.Read(c.Context(), endpoint, "feature flags", "/viewer/json/feature_flags", "/local", "ticket-value", nil)
	c.Assert(err, qt.ErrorMatches, `read the YDB feature flags from the monitoring endpoint: 400 Bad Request; `+
		`the page is read as the connection's user, who needs DESCRIBE SCHEMA on /local; the endpoint answered: Failed to resolve database for <redacted>`)
	c.Assert(err.Error(), qt.Not(qt.Contains), "ticket-value")
	c.Assert(err.Error(), qt.Not(qt.Contains), endpoint.Host)
}

// TestRead_RefusalOfAnUnreachableEndpointNamesNoAddress keeps the endpoint
// out of a transport error, which net/http would spell with the whole URL.
func TestRead_RefusalOfAnUnreachableEndpointNamesNoAddress(t *testing.T) {
	c := testcheck.New(t)
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint, err := url.Parse(server.URL)
	c.Assert(err, qt.IsNil)
	server.Close()
	_, err = monitoring.Read(c.Context(), endpoint, "feature flags", "/viewer/json/feature_flags", "/local", "", nil)
	c.Assert(err, qt.ErrorMatches, `read the YDB feature flags from the monitoring endpoint: the endpoint did not answer`)
}

// TestRead_RefusesWithoutAnEndpoint names the missing endpoint.
func TestRead_RefusesWithoutAnEndpoint(t *testing.T) {
	c := testcheck.New(t)
	body, err := monitoring.Read(c.Context(), nil, "column table", "/viewer/json/describe", "/local", "", nil)
	c.Assert(err, qt.ErrorMatches, `no monitoring endpoint to read the YDB column table from`)
	c.Assert(body, qt.IsNil)
}

// TestRead_RefusalsAreReasons holds every refusal to a reason the host
// prints, and keeps what the endpoint answered out of it: an endpoint can
// answer with anything, a redirect with the address it points at.
func TestRead_RefusalsAreReasons(t *testing.T) {
	refusing := func(status int, page string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(page))
		}
	}
	tests := []struct {
		name, ticket string
		handler      http.HandlerFunc
		want         string
	}{
		{name: "a user without the right", ticket: "ticket-value", handler: refusing(http.StatusBadRequest, "Failed to resolve database"),
			want: "read the YDB feature flags from the monitoring endpoint: 400 Bad Request; the page is read as the connection's user, who needs DESCRIBE SCHEMA on /local"},
		{name: "an anonymous read refused", handler: refusing(http.StatusBadRequest, "Failed to resolve database"),
			want: "read the YDB feature flags from the monitoring endpoint: 400 Bad Request"},
		{name: "a page that moved", handler: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://elsewhere.example/viewer/json/feature_flags", http.StatusFound)
		}, want: "read the YDB feature flags from the monitoring endpoint: 302 Found"},
		{name: "an oversized page", handler: refusing(http.StatusOK, strings.Repeat("x", monitoring.MaxBody+1)),
			want: "read the YDB feature flags from the monitoring endpoint: the page exceeds 4194304 bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			_, err := monitoring.Read(c.Context(), serve(c, test.handler), "feature flags", "/viewer/json/feature_flags", "/local", test.ticket, nil)
			var reason *authored.Error
			c.Assert(err, qt.ErrorAs, &reason)
			c.Assert(reason.Error(), qt.Equals, test.want)
		})
	}
}

// TestRead_RefusalWithoutAnAnswerIsAReason covers the refusals no endpoint
// answers: none named, and one that does not listen.
func TestRead_RefusalWithoutAnAnswerIsAReason(t *testing.T) {
	c := testcheck.New(t)
	server := httptest.NewServer(http.NotFoundHandler())
	silent, err := url.Parse(server.URL)
	c.Assert(err, qt.IsNil)
	server.Close()
	for _, test := range []struct {
		name     string
		endpoint *url.URL
		want     string
	}{
		{name: "no endpoint", want: "no monitoring endpoint to read the YDB feature flags from"},
		{name: "an endpoint that does not answer", endpoint: silent, want: "read the YDB feature flags from the monitoring endpoint: the endpoint did not answer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			_, err := monitoring.Read(c.Context(), test.endpoint, "feature flags", "/viewer/json/feature_flags", "/local", "", nil)
			var reason *authored.Error
			c.Assert(err, qt.ErrorAs, &reason)
			c.Assert(reason.Error(), qt.Equals, test.want)
		})
	}
}
