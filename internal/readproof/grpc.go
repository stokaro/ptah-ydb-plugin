package readproof

import (
	"context"
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/draft/Ydb_View_V1"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_View"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// Catalog reads object kinds and stored view queries from the session driver.
type Catalog struct { Driver *ydb.Driver }

func (c Catalog) Database() string { return c.Driver.Name() }

func (c Catalog) EntryType(ctx context.Context, absolute string) (Ydb_Scheme.Entry_Type, error) {
	response, err := Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(c.Driver)).DescribePath(ctx, &Ydb_Scheme.DescribePathRequest{Path: absolute})
	if err != nil { return Ydb_Scheme.Entry_TYPE_UNSPECIFIED, fmt.Errorf("describe query source: transport failed") }
	var result Ydb_Scheme.DescribePathResult
	if err := nativeapi.Operation(response.GetOperation(), &result); err != nil { return Ydb_Scheme.Entry_TYPE_UNSPECIFIED, err }
	return result.GetSelf().GetType(), nil
}

func (c Catalog) ViewQuery(ctx context.Context, absolute string) (string, error) {
	response, err := Ydb_View_V1.NewViewServiceClient(ydb.GRPCConn(c.Driver)).DescribeView(ctx, &Ydb_View.DescribeViewRequest{Path: absolute})
	if err != nil { return "", fmt.Errorf("describe query view: transport failed") }
	var result Ydb_View.DescribeViewResult
	if err := nativeapi.Operation(response.GetOperation(), &result); err != nil { return "", err }
	return result.GetQueryText(), nil
}
