package connection

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ydb-platform/ydb-go-sdk/v3/balancers"
)

// The query parameters of a YDB address besides database, token and
// use_env_credentials. The SDK ignores a parameter it does not know without a
// word, so a parameter is passed on only when the address names it here.
const (
	paramBalancer          = "go_balancer"
	paramLegacyBalancer    = "balancer"
	paramQueryMode         = "go_query_mode"
	paramLegacyQueryMode   = "query_mode"
	paramDefaultIdempotent = "go_default_idempotent"
	paramPrefetchParts     = "prefetch_query_result_parts"
	paramMonitoring        = "monitoring"
	paramFakeTx            = "go_fake_tx"
	paramQueryBind         = "go_query_bind"
)

// acceptedParameters lists, in the order a refusal names them, the parameters
// an address may carry.
var acceptedParameters = []string{
	"database", paramMonitoring, "token", "use_env_credentials", paramBalancer, paramLegacyBalancer,
	paramQueryMode, paramLegacyQueryMode, paramDefaultIdempotent, paramPrefetchParts,
}

// sdkParameters checks the parameters the SDK reads and returns them as the
// SDK's address carries them. A refusal names the parameter and never its
// value, which may be a secret in a mistyped key.
func sdkParameters(query url.Values) (url.Values, error) {
	passed := url.Values{}
	for _, key := range slices.Sorted(maps.Keys(query)) {
		value := query.Get(key)
		switch key {
		case paramBalancer, paramLegacyBalancer:
			// The SDK falls back to its default balancer on a value it cannot
			// parse and says nothing.
			if _, err := balancers.CreateFromConfig(value); err != nil {
				return nil, fmt.Errorf("parameter %s is not a balancer ydb-go-sdk knows: use disable, single, random_choice, round_robin or a JSON balancer config", key)
			}
		case paramQueryMode, paramLegacyQueryMode:
			if value != "query" {
				return nil, fmt.Errorf("parameter %s is refused: the provider runs its reads and DDL through the query service; use %s=query or leave it out", key, key)
			}
		case paramDefaultIdempotent:
			if _, err := strconv.ParseBool(value); err != nil {
				return nil, fmt.Errorf("parameter %s is not a boolean", key)
			}
		case paramPrefetchParts:
			if parts, err := strconv.Atoi(value); err != nil || parts < 0 {
				return nil, fmt.Errorf("parameter %s is not a non-negative integer", key)
			}
		case paramFakeTx:
			return nil, fmt.Errorf("parameter %s is refused: a schema is read in a snapshot read-only transaction, and a faked transaction would read outside it", key)
		case paramQueryBind:
			return nil, fmt.Errorf("parameter %s is refused: the provider binds parameters itself, and the SDK's binders rewrite a ? inside a YQL literal", key)
		}
		switch key {
		case paramBalancer, paramLegacyBalancer, paramDefaultIdempotent, paramPrefetchParts:
			passed.Set(key, value)
		}
	}
	if passed.Has(paramBalancer) && passed.Has(paramLegacyBalancer) {
		return nil, fmt.Errorf("parameters %s and %s both choose a balancer; give one", paramBalancer, paramLegacyBalancer)
	}
	return passed, nil
}

// monitoringEndpoint reads the monitoring parameter, or returns nil when the
// query has none. The value is the endpoint and nothing more: a path, a query,
// a fragment or a user is refused. A refusal names at most the scheme and the
// host, which carry no secret.
func monitoringEndpoint(query url.Values) (*url.URL, error) {
	if !query.Has(paramMonitoring) {
		return nil, nil
	}
	value := query.Get(paramMonitoring)
	if value == "" {
		return nil, errors.New("the monitoring parameter is empty: write monitoring=http://host:8765")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, errors.New("the monitoring parameter is not a URL: write monitoring=http://host:8765")
	}
	endpoint := &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return nil, errors.New("the monitoring parameter names no http:// or https:// endpoint: write monitoring=http://host:8765")
	case parsed.Hostname() == "":
		return nil, errors.New("the monitoring parameter names no host: write monitoring=http://host:8765")
	case parsed.User != nil:
		return nil, errors.New("the monitoring parameter carries a user; name the endpoint only, as monitoring=http://host:8765")
	case strings.Trim(parsed.EscapedPath(), "/") != "" || parsed.RawQuery != "" || parsed.Fragment != "":
		return nil, errors.New("the monitoring parameter names a page; name the endpoint only, as monitoring=http://host:8765")
	}
	return endpoint, nil
}
