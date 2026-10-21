// Package connection owns native YDB connections and their credentials.
package connection

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/balancers"
)

// Address separates the credential-free SDK address from authentication.
// Its fields are private so formatting it cannot reveal a connection URL.
type Address struct {
	dsn      string
	database string
	user     *url.Userinfo
	token    string
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
		switch key {
		case "database", "token", "go_balancer":
		default:
			return Address{}, errors.New("unsupported YDB URL parameter; supported parameters: database, token, go_balancer")
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
	if u.User != nil {
		password, ok := u.User.Password()
		if u.User.Username() == "" || !ok || password == "" {
			return Address{}, errors.New("YDB static credentials require a user and password")
		}
	}
	passed := url.Values{}
	if q.Has("go_balancer") {
		value := q.Get("go_balancer")
		if value == "" {
			return Address{}, errors.New("invalid YDB balancer")
		}
		if _, err := balancers.CreateFromConfig(value); err != nil {
			return Address{}, errors.New("invalid YDB balancer")
		}
		passed.Set("go_balancer", value)
	}
	database = "/" + database
	dsn := url.URL{Scheme: scheme, Host: net.JoinHostPort(u.Hostname(), port), Path: database, RawQuery: passed.Encode()}
	return Address{dsn: dsn.String(), database: database, user: u.User, token: token}, nil
}

// String identifies the address without exposing its credentials.
func (a Address) String() string { return a.dsn }

// GoString also keeps Go-syntax formatting free of credentials.
func (a Address) GoString() string { return a.String() }

// Database returns the database selected by both the SDK and session receipt.
func (a Address) Database() string { return a.database }

func (a Address) credentials() ydb.Option {
	if a.user != nil {
		password, _ := a.user.Password()
		return ydb.WithStaticCredentials(a.user.Username(), password)
	}
	if a.token != "" {
		return ydb.WithAccessTokenCredentials(a.token)
	}
	return ydb.WithAnonymousCredentials()
}
