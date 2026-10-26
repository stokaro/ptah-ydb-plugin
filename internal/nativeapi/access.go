package nativeapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// ErrAccessPathNotVisible marks a path whose description answered SCHEME_ERROR.
// It establishes neither absence nor permission to inspect that path.
var ErrAccessPathNotVisible = errors.New("the access path does not exist or is not visible")

// AccessReader reads direct permissions and ownership through the scheme API.
// It returns the server's entry without projecting away access metadata.
func AccessReader(driver *ydb.Driver) func(context.Context, string) (*Ydb_Scheme.Entry, error) {
	client := Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(driver))
	return func(ctx context.Context, absolute string) (*Ydb_Scheme.Entry, error) {
		response, err := client.DescribePath(ctx, &Ydb_Scheme.DescribePathRequest{Path: absolute})
		if err != nil {
			return nil, fmt.Errorf("describe YDB access path: transport failed")
		}
		if operation := response.GetOperation(); operation.GetReady() && operation.GetStatus() == Ydb.StatusIds_SCHEME_ERROR {
			return nil, ErrAccessPathNotVisible
		}
		var result Ydb_Scheme.DescribePathResult
		if err := Operation(response.GetOperation(), &result); err != nil {
			return nil, err
		}
		if result.GetSelf() == nil {
			return nil, fmt.Errorf("YDB access description has no object")
		}
		return result.GetSelf(), nil
	}
}
