// Package devdatabase provisions disposable YDB databases for the provider's
// database.dev service. A dev database is a ydbplatform/local-ydb container the
// provider starts on the machine it runs on, publishes on loopback only, and
// removes when the host releases the lease or the provider process ends.
package devdatabase

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
)

// Image is the repository the container starts from. The spec names the tag.
const Image = "ydbplatform/local-ydb"

// Database is the one database local-ydb serves.
const Database = "local"

// DefaultTag is the tag a spec that names none starts.
const DefaultTag = "latest"

// ServerPort is the port the server listens on inside the container.
const ServerPort = "2136"

// Refusal is a provisioning request the provider declines. Unavailable
// separates a runtime the provider needs but lacks from a request it does not
// serve.
type Refusal struct {
	Unavailable bool
	Reason      string
}

func (r *Refusal) Error() string { return r.Reason }

func unsupported(format string, args ...any) *Refusal {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// Spec is a parsed `docker://ydb[/<tag>[/local]]` request.
type Spec struct {
	// Image is the fully tagged image to start.
	Image string
	// Query is the encoded connection parameters of the address the host gets:
	// the defaults with the operator's written over them.
	Query string
}

// defaults make the container reachable. local-ydb advertises its node under
// the container's host name, which nothing outside the container resolves, so
// the address disables the balancer and the SDK connects to the published port.
var defaults = map[string]string{"go_balancer": "disable"}

// routingParams would point the connection away from the container the
// request provisions, or hand the container a credential it does not check.
// The first nine are the ones the host refuses on every docker address.
var routingParams = []string{
	"host", "hostaddr", "port", "user", "password", "dbname", "passfile", "service", "servicefile",
	"database", "dev_realm", "monitoring", "token", "use_env_credentials",
}

// tagPattern is a Docker image tag.
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// ParseSpec reads the operator's spec. The engine segment is matched as
// written, as the host matches it. One path segment is a tag, never a database,
// and local-ydb serves the database local alone. Every parameter must be one a
// YDB address accepts, so a typo is refused before a container starts.
func ParseSpec(raw string) (Spec, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return Spec{}, unsupported("the dev database spec is not a URL")
	}
	if parsed.Scheme != "docker" {
		return Spec{}, unsupported("the dev database spec is not a docker:// URL")
	}
	if parsed.Host != "ydb" {
		return Spec{}, unsupported("unsupported docker image %q", parsed.Host)
	}
	tag := DefaultTag
	if trimmed := strings.Trim(parsed.Path, "/"); trimmed != "" {
		segments := strings.Split(trimmed, "/")
		if len(segments) > 2 {
			return Spec{}, unsupported("the docker path has more than <tag>/<database>")
		}
		tag = segments[0]
		if len(segments) == 2 && segments[1] != Database {
			return Spec{}, unsupported("the database is not one the %s image serves: name %s", Image, Database)
		}
	}
	if !tagPattern.MatchString(tag) {
		return Spec{}, unsupported("the image tag is empty or not a Docker tag")
	}
	operator, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return Spec{}, unsupported("the docker parameters are malformed")
	}
	for _, name := range routingParams {
		if _, present := operator[name]; present {
			return Spec{}, unsupported("docker parameter %q would point the connection away from the container this spec provisions; remove it", name)
		}
	}
	merged := url.Values{}
	for key, value := range defaults {
		merged.Set(key, value)
	}
	for key, values := range operator {
		merged[key] = slices.Clone(values)
	}
	query := merged.Encode()
	if _, err := connection.Parse("ydb://127.0.0.1:" + ServerPort + "/" + Database + "?" + query); err != nil {
		return Spec{}, unsupported("the docker parameters are not accepted by a YDB address: %s", err)
	}
	return Spec{Image: Image + ":" + tag, Query: query}, nil
}

// URL is the address of the container's database published at hostPort.
func (s Spec) URL(hostPort string) string {
	return "ydb://" + hostPort + "/" + Database + "?" + s.Query
}
