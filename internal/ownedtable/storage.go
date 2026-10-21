package ownedtable

import (
	"context"
	"errors"
	"fmt"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"path"
	"strings"
)

// Check establishes absence through complete parent listings, or validates the
// existing table. An arbitrary scheme error never becomes proof of absence.
func (s Identity) Check(ctx context.Context, driver *ydb.Driver, validate func(*native.DescribeTableResult) error) (exists bool, err error) {
	exists, err = s.listed(ctx, driver)
	if err != nil || !exists {
		return exists, err
	}
	table, err := nativeapi.OpenTable(ctx, driver)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, table.Close())
		if err != nil {
			exists = false
		}
	}()
	value, err := table.Describe(ctx, s.Path())
	if err != nil {
		return false, err
	}
	if err = validate(value); err != nil {
		return false, err
	}
	return true, nil
}

func (s Identity) listed(ctx context.Context, driver *ydb.Driver) (bool, error) {
	client := Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(driver))
	parts := []string{}
	if s.Directory != "" {
		parts = strings.Split(s.Directory, "/")
	}
	parts = append(parts, s.Name)
	parent := s.Database
	for i, name := range parts {
		response, err := client.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: parent})
		if err != nil {
			return false, fmt.Errorf("list YDB storage namespace: transport failed")
		}
		var value Ydb_Scheme.ListDirectoryResult
		if err := nativeapi.Operation(response.GetOperation(), &value); err != nil {
			return false, err
		}
		if value.GetSelf().GetName() != path.Base(parent) ||
			(value.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY && (i != 0 || value.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE)) ||
			len(value.Children) > 4096 {
			return false, fmt.Errorf("invalid or oversized YDB storage namespace listing")
		}
		var found *Ydb_Scheme.Entry
		seen := map[string]bool{}
		for _, entry := range value.Children {
			if !component(entry.Name) || seen[entry.Name] {
				return false, fmt.Errorf("invalid YDB storage namespace entry")
			}
			seen[entry.Name] = true
			if entry.Name == name {
				found = entry
			}
		}
		if found == nil {
			return false, nil
		}
		expected := Ydb_Scheme.Entry_DIRECTORY
		if i == len(parts)-1 {
			expected = Ydb_Scheme.Entry_TABLE
		}
		if found.Type != expected {
			return false, fmt.Errorf("YDB storage path has an incompatible object kind")
		}
		parent = path.Join(parent, name)
	}
	return true, nil
}

// Prepare creates a missing table with its marker in one native request.
// Attempted distinguishes a failure after create dispatch from a refusal.
func (s Identity) Prepare(ctx context.Context, driver *ydb.Driver, request *Ydb_Table.CreateTableRequest, validate func(*native.DescribeTableResult) error, beforeCreate func(context.Context) error) (attempted bool, err error) {
	exists, err := s.Check(ctx, driver, validate)
	if err != nil || exists {
		return false, err
	}
	table, err := nativeapi.OpenTable(ctx, driver)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, table.Close()) }()
	if err := beforeCreate(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	attempted = true
	if err := table.Create(ctx, request); err != nil {
		return true, err
	}
	described, err := table.Describe(ctx, s.Path())
	if err != nil {
		return true, err
	}
	return true, validate(described)
}
