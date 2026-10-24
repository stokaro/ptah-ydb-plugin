// Package connection owns native YDB connections and their credentials.
package connection

import (
	"errors"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	environ "github.com/ydb-platform/ydb-go-sdk-auth-environ"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// Address separates the credential-free SDK address from authentication.
// Its fields are private so formatting it cannot reveal a connection URL.
type Address struct {
	dsn        string
	database   string
	user       *url.Userinfo
	token      string
	useEnv     bool
	monitoring *url.URL
}

// Parse validates the provider's connection address before any network call.
// Unknown, repeated, and malformed parameters are refused. Diagnostics never
// repeat input values, including malformed URLs with embedded credentials.
func Parse(raw string) (Address, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Fragment != "" || u.Hostname() == "" {
		return Address{}, errors.New("invalid YDB connection URL")
	}
	scheme, port := "grpc", "2136"
	switch strings.ToLower(u.Scheme) {
	case "ydb":
	case "ydbs":
		scheme, port = "grpcs", "2135"
	default:
		return Address{}, errors.New("YDB requires a ydb:// or ydbs:// address")
	}
	if u.Port() != "" {
		port = u.Port()
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return Address{}, errors.New("invalid YDB port")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return Address{}, errors.New("invalid YDB URL parameters")
	}
	for key, values := range q {
		if len(values) != 1 {
			return Address{}, errors.New("YDB URL parameters must appear once")
		}
		if !slices.Contains(acceptedParameters, key) {
			return Address{}, errors.New("unsupported YDB URL parameter; supported parameters: " + strings.Join(acceptedParameters, ", "))
		}
	}
	database := strings.Trim(u.Path, "/")
	if q.Has("database") {
		value := strings.Trim(q.Get("database"), "/")
		if value == "" || (database != "" && database != value) {
			return Address{}, errors.New("YDB URL database path and parameter must identify the same nonempty database")
		}
		database = value
	}
	if database == "" {
		return Address{}, errors.New("YDB URL must name a database")
	}
	for _, part := range strings.Split(database, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
			return Address{}, errors.New("invalid YDB database path")
		}
	}
	token := q.Get("token")
	if q.Has("token") && (strings.TrimSpace(token) == "" || u.User != nil) {
		return Address{}, errors.New("provide exactly one nonempty YDB credential source")
	}
	useEnv := false
	if q.Has("use_env_credentials") {
		// A bare parameter is on, the way ydb-go-sdk-auth-environ reads it; a
		// value is a strict boolean.
		useEnv = true
		if raw := strings.TrimSpace(q.Get("use_env_credentials")); raw != "" {
			enabled, err := strconv.ParseBool(raw)
			if err != nil {
				return Address{}, errors.New("use_env_credentials must be a boolean")
			}
			useEnv = enabled
		}
		if useEnv && (u.User != nil || q.Has("token")) {
			return Address{}, errors.New("provide exactly one nonempty YDB credential source")
		}
	}
	if u.User != nil {
		password, ok := u.User.Password()
		if u.User.Username() == "" || !ok || password == "" {
			return Address{}, errors.New("YDB static credentials require a user and password")
		}
	}
	passed, err := sdkParameters(q)
	if err != nil {
		return Address{}, err
	}
	monitoring, err := monitoringEndpoint(q)
	if err != nil {
		return Address{}, err
	}
	database = "/" + database
	dsn := url.URL{Scheme: scheme, Host: net.JoinHostPort(u.Hostname(), port), Path: database, RawQuery: passed.Encode()}
	return Address{dsn: dsn.String(), database: database, user: u.User, token: token, useEnv: useEnv, monitoring: monitoring}, nil
}

// Monitoring returns the cluster's monitoring endpoint the URL names, an http://
// or https:// address with no path, or nil when it names none. The provider
// protocol has no way to refine the advertised capabilities from a cluster's
// feature flags, so nothing reads the endpoint yet.
func (a Address) Monitoring() *url.URL {
	if a.monitoring == nil {
		return nil
	}
	endpoint := *a.monitoring
	return &endpoint
}

// String identifies the address without exposing its credentials.
func (a Address) String() string { return a.dsn }

// GoString also keeps Go-syntax formatting free of credentials.
func (a Address) GoString() string { return a.String() }

// Database returns the database selected by both the SDK and session receipt.
func (a Address) Database() string { return a.database }

// credentials resolves the one credential source the address names. An address
// that names none connects anonymously on purpose.
func (a Address) credentials() (ydb.Option, error) {
	if a.user != nil {
		password, _ := a.user.Password()
		return ydb.WithStaticCredentials(a.user.Username(), password), nil
	}
	if a.token != "" {
		return ydb.WithAccessTokenCredentials(a.token), nil
	}
	if a.useEnv {
		if err := CheckEnvironment(os.LookupEnv); err != nil {
			return nil, err
		}
		return environ.WithEnvironCredentials(), nil
	}
	return ydb.WithAnonymousCredentials(), nil
}

// environmentCredentials are the variables ydb-go-sdk-auth-environ reads, in
// the order it reads them.
var environmentCredentials = []string{
	"YDB_SERVICE_ACCOUNT_KEY_CREDENTIALS",
	"YDB_SERVICE_ACCOUNT_KEY_FILE_CREDENTIALS",
	"YDB_METADATA_CREDENTIALS",
	"YDB_ACCESS_TOKEN_CREDENTIALS",
	"YDB_STATIC_CREDENTIALS_USER",
	"YDB_OAUTH2_KEY_FILE",
	"YDB_ANONYMOUS_CREDENTIALS",
}

// CheckEnvironment refuses the environments ydb-go-sdk-auth-environ would read
// as anonymous without saying so: none of its variables set, a static user
// without the password or the endpoint it needs, and a metadata switch set to
// anything but the 1 it reads. The messages name variables, never values.
func CheckEnvironment(lookup func(string) (string, bool)) error {
	set := slices.ContainsFunc(environmentCredentials, func(name string) bool {
		_, ok := lookup(name)
		return ok
	})
	if !set {
		return errors.New("use_env_credentials is set and no credential variable is: set one of " + strings.Join(environmentCredentials, ", "))
	}
	if value, ok := lookup("YDB_METADATA_CREDENTIALS"); ok && value != "1" {
		return errors.New("YDB_METADATA_CREDENTIALS is ignored by the YDB SDK, which reads only 1")
	}
	if _, ok := lookup("YDB_STATIC_CREDENTIALS_USER"); ok {
		var missing []string
		for _, name := range []string{"YDB_STATIC_CREDENTIALS_PASSWORD", "YDB_STATIC_CREDENTIALS_ENDPOINT"} {
			if _, ok := lookup(name); !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return errors.New("YDB_STATIC_CREDENTIALS_USER is set without " + strings.Join(missing, " and ") + ", and the YDB SDK would connect anonymously instead")
		}
	}
	return nil
}
