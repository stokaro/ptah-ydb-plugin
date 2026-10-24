// Package monitoring reads JSON pages from the monitoring endpoint a YDB
// address names in its monitoring parameter: the feature flags of a database
// and the scheme description of a column table. Both reads share one client,
// so neither can follow a redirect or read an unbounded page, and both present
// the database connection's own credential.
//
// The rules are the host's embedded engine's (internal/ydbmonitor in Ptah).
package monitoring

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// timeout bounds one read. The client applies it whatever the caller's context
// says, and the earlier of the two ends the read.
const timeout = 30 * time.Second

// MaxBody bounds the page the provider reads. A database lists a few hundred
// flags, about 15 KB, and a column table's description is smaller.
const MaxBody = 4 << 20

// maxEcho bounds how much of a refusing page an error repeats: enough for the
// endpoint's own message, and not the whole of whatever answered.
const maxEcho = 256

// client follows no redirect. The page is read from the endpoint the operator
// named, and a redirect to another host is answered as the status it is rather
// than followed to a place the provider was not pointed at, with the
// credential.
var client = &http.Client{
	Timeout: timeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Read fetches one JSON page from endpoint. path is the page, such as
// /viewer/json/feature_flags, and query its parameters; the endpoint's own path,
// query and fragment are replaced. A nonempty ticket is sent as YDB's bare
// Authorization header. A redirect is refused, the page is bounded by MaxBody,
// and an error repeats at most the start of a refusing page with the ticket
// taken out. resource names the page in diagnostics. An error never repeats
// the endpoint or the ticket.
func Read(ctx context.Context, endpoint *url.URL, resource, path, database, ticket string, query url.Values) ([]byte, error) {
	if endpoint == nil {
		return nil, fmt.Errorf("no monitoring endpoint to read the YDB %s from", resource)
	}
	page := *endpoint
	page.User, page.Path, page.RawPath, page.Fragment = nil, path, "", ""
	page.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, page.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("read the YDB %s from the monitoring endpoint: the request cannot be built", resource)
	}
	request.Header.Set("Accept", "application/json")
	if ticket != "" {
		request.Header.Set("Authorization", ticket)
	}
	response, err := client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("read the YDB %s from the monitoring endpoint: %s", resource, transportReason(err))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read the YDB %s from the monitoring endpoint: the page was cut off", resource)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read the YDB %s from the monitoring endpoint: %s: %s%s", resource, response.Status, echo(body, ticket), rightsHint(response.StatusCode, ticket, database))
	}
	if len(body) > MaxBody {
		return nil, fmt.Errorf("read the YDB %s from the monitoring endpoint: the page exceeds %d bytes", resource, MaxBody)
	}
	return body, nil
}

// transportReason says why a request reached no answer without repeating the
// URL, which net/http writes into its errors.
func transportReason(err error) string {
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return "no answer within " + timeout.String()
	}
	return "the endpoint did not answer"
}

// echo is the start of a refusing page, as an error repeats it. An endpoint
// that repeats the request's headers would repeat the ticket, so the ticket is
// taken out before the page is cut.
func echo(body []byte, ticket string) string {
	text := strings.TrimSpace(string(body))
	if ticket != "" {
		text = strings.ReplaceAll(text, ticket, "<redacted>")
	}
	if len(text) <= maxEcho {
		return text
	}
	return strings.ToValidUTF8(text[:maxEcho], "") + "..."
}

// rightsHint says what a 400 means when the page was read with a credential:
// measured by the host on 26.2.1.14, the endpoint answers `Failed to resolve
// database` to a user without DESCRIBE SCHEMA on the database, the same user
// reads the page once granted it, and a cluster that does not enforce
// authentication checks no anonymous read at all.
func rightsHint(status int, ticket, database string) string {
	if status != http.StatusBadRequest || ticket == "" {
		return ""
	}
	return fmt.Sprintf("; the page is read as the connection's user, who needs DESCRIBE SCHEMA on %s", database)
}
