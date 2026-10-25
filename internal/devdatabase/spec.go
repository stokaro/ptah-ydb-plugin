// Package devdatabase provisions disposable YDB databases for the provider's
// database.dev service. A dev database is a ydbplatform/local-ydb container the
// provider starts on the machine it runs on, publishes on loopback only, and
// removes when the host releases the lease or the provider process ends.
package devdatabase

import (
	"fmt"
	"net/url"
	"path"
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

// Spec is a parsed `docker://ydb[/<tag>[/local]]` or
// `docker+ydb://[<host>/]<image>[:<tag>][/local]` request.
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

// ParseSpec reads the operator's spec, in either form the operator types.
//
// `docker://ydb[/<tag>[/local]]` starts ydbplatform/local-ydb. The engine
// segment is matched as written, as the host matches it, and one path segment
// is a tag, never a database.
//
// `docker+ydb://[<host>/]<image>[:<tag>][/local]` starts the image the spec
// names, read as the host reads the image form of its other engines: the last
// path segment is the database when there are two or more and it holds no
// colon, the segments before it are the image, a host other than `_` leads the
// image, and an image with no tag is `latest`. The image has to serve what
// local-ydb serves: a database the provider connects to without a credential.
//
// In both forms the database is local, the one database local-ydb serves, and
// every parameter must be one a YDB address accepts, so a typo is refused
// before a container starts.
func ParseSpec(raw string) (Spec, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return Spec{}, unsupported("the dev database spec is not a URL")
	}
	var image string
	switch parsed.Scheme {
	case "docker":
		image, err = engineImage(parsed)
	case "docker+ydb":
		image, err = namedImage(parsed)
	default:
		return Spec{}, unsupported("the dev database spec is not a docker:// or docker+ydb:// URL")
	}
	if err != nil {
		return Spec{}, err
	}
	query, err := parameters(parsed)
	if err != nil {
		return Spec{}, err
	}
	return Spec{Image: image, Query: query}, nil
}

// engineImage reads the image of a `docker://ydb[/<tag>[/local]]` spec.
func engineImage(parsed *url.URL) (string, error) {
	if parsed.Host != "ydb" {
		return "", unsupported("unsupported docker image %q", parsed.Host)
	}
	tag := DefaultTag
	if trimmed := strings.Trim(parsed.Path, "/"); trimmed != "" {
		segments := strings.Split(trimmed, "/")
		if len(segments) > 2 {
			return "", unsupported("the docker path has more than <tag>/<database>")
		}
		tag = segments[0]
		if len(segments) == 2 && segments[1] != Database {
			return "", unsupported("the database is not one the %s image serves: name %s", Image, Database)
		}
	}
	if !tagPattern.MatchString(tag) {
		return "", unsupported("the image tag is empty or not a Docker tag")
	}
	return Image + ":" + tag, nil
}

// namedImage reads the image of a `docker+ydb://` spec.
func namedImage(parsed *url.URL) (string, error) {
	segments := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	database := ""
	if last := len(segments) - 1; last > 0 && !strings.Contains(segments[last], ":") {
		database, segments = segments[last], segments[:last]
	}
	image := strings.TrimSuffix(path.Join(segments...), ":")
	if parsed.Host != "" && parsed.Host != "_" {
		image = path.Join(parsed.Host, image)
	}
	if image == "" || image == "." {
		return "", unsupported("the docker+ydb spec names no image")
	}
	if database != "" && database != Database {
		return "", unsupported("the database is not one a YDB dev image serves: name %s", Database)
	}
	if !imagePattern.MatchString(image) {
		return "", unsupported("the docker+ydb spec does not name a Docker image")
	}
	if !strings.Contains(path.Base(image), ":") {
		image += ":" + DefaultTag
	}
	return image, nil
}

// imagePattern is a Docker image reference: a repository of lowercase path
// components, a registry host with an optional port before them, and an
// optional tag.
var imagePattern = regexp.MustCompile(`^([a-z0-9.-]+(:[0-9]+)?/)?[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?$`)

// parameters merges the operator's parameters over the defaults, refusing
// one that would point the connection away from the container and one a YDB
// address does not accept.
func parameters(parsed *url.URL) (string, error) {
	operator, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", unsupported("the docker parameters are malformed")
	}
	for _, name := range routingParams {
		if _, present := operator[name]; present {
			return "", unsupported("docker parameter %q would point the connection away from the container this spec provisions; remove it", name)
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
		return "", unsupported("the docker parameters are not accepted by a YDB address: %s", err)
	}
	return query, nil
}

// URL is the address of the container's database published at hostPort.
func (s Spec) URL(hostPort string) string {
	return "ydb://" + hostPort + "/" + Database + "?" + s.Query
}
