package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// Runner is the exact SQL connection or transaction selected by the session.
type Runner interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Check establishes absence through complete parent listings, or validates the
// existing table. An arbitrary scheme error never becomes proof of absence.
func (s Store) Check(ctx context.Context, driver *ydb.Driver) (exists bool, err error) {
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
	if err = s.ValidateLayout(value); err != nil {
		return false, err
	}
	return true, nil
}

func (s Store) listed(ctx context.Context, driver *ydb.Driver) (bool, error) {
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
			return false, fmt.Errorf("list YDB history namespace: transport failed")
		}
		var value Ydb_Scheme.ListDirectoryResult
		if err := nativeapi.Operation(response.GetOperation(), &value); err != nil {
			return false, err
		}
		if value.GetSelf().GetName() != path.Base(parent) ||
			(value.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY && (i != 0 || value.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE)) ||
			len(value.Children) > 4096 {
			return false, fmt.Errorf("invalid or oversized YDB history namespace listing")
		}
		var found *Ydb_Scheme.Entry
		seen := map[string]bool{}
		for _, entry := range value.Children {
			if !component(entry.Name) || seen[entry.Name] {
				return false, fmt.Errorf("invalid YDB history namespace entry")
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
			return false, fmt.Errorf("YDB history path has an incompatible object kind")
		}
		parent = path.Join(parent, name)
	}
	return true, nil
}

// Prepare creates a missing table with its marker in one native request.
// Attempted distinguishes a failure after create dispatch from a refusal.
func (s Store) Prepare(ctx context.Context, driver *ydb.Driver) (attempted bool, err error) {
	exists, err := s.Check(ctx, driver)
	if err != nil || exists {
		return false, err
	}
	table, err := nativeapi.OpenTable(ctx, driver)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, table.Close()) }()
	attempted = true
	if err := table.Create(ctx, s.CreateRequest()); err != nil {
		return true, err
	}
	described, err := table.Describe(ctx, s.Path())
	if err != nil {
		return true, err
	}
	return true, s.ValidateLayout(described)
}

// Read returns one consistent bounded query result, never a truncated prefix.
func (s Store) Read(ctx context.Context, runner Runner, limit uint32) (result []*protocol.HistoryRevision, err error) {
	rows, err := runner.QueryContext(ctx, "SELECT version, revision FROM "+s.QuotedPath()+" ORDER BY version LIMIT "+strconv.FormatUint(uint64(limit)+1, 10))
	if err != nil {
		return nil, fmt.Errorf("read YDB history: query failed")
	}
	defer func() {
		err = errors.Join(err, rows.Close())
		if err != nil {
			result = nil
		}
	}()
	size := 0
	for rows.Next() {
		if len(result) >= int(limit) {
			return nil, fmt.Errorf("YDB history exceeds revision limit")
		}
		var payload []byte
		var version string
		if s.Format == protocol.HistoryScope_NATIVE {
			var numeric int64
			err = rows.Scan(&numeric, &payload)
			version = strconv.FormatInt(numeric, 10)
		} else {
			err = rows.Scan(&version, &payload)
		}
		if err != nil {
			return nil, fmt.Errorf("read YDB history: invalid row")
		}
		record, err := s.Decode(version, payload)
		if err != nil {
			return nil, err
		}
		size += proto.Size(record) + 8
		if size > MaxSnapshotBytes {
			return nil, fmt.Errorf("YDB history exceeds snapshot size limit")
		}
		result = append(result, record)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("read YDB history: stream failed")
	}
	return result, nil
}

// Mutate compares all expected facts and writes on one serializable transaction.
// The caller owns commit. RowsAffected is not an existence witness in YDB.
func (s Store) Mutate(ctx context.Context, runner Runner, request *protocol.MutateHistoryRequest) (protocol.HistoryResult_Outcome, error) {
	version := request.GetReplacement().GetVersion()
	if request.Expected != nil {
		version = request.Expected.Version
	}
	key, err := s.Key(version)
	if err != nil {
		return protocol.HistoryResult_REFUSED, err
	}
	declaration := "DECLARE $version AS " + s.KeyType() + "; "
	var payload []byte
	err = runner.QueryRowContext(ctx, declaration+"SELECT revision FROM "+s.QuotedPath()+" WHERE version = $version", sql.Named("version", key)).Scan(&payload)
	var current *protocol.HistoryRevision
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return protocol.HistoryResult_UNKNOWN, fmt.Errorf("compare YDB history: query failed")
	default:
		current, err = s.Decode(version, payload)
		if err != nil {
			return protocol.HistoryResult_REFUSED, err
		}
	}
	if !proto.Equal(current, request.Expected) {
		return protocol.HistoryResult_CONFLICT, nil
	}
	query := declaration + "DELETE FROM " + s.QuotedPath() + " WHERE version = $version"
	args := []any{sql.Named("version", key)}
	if request.Replacement != nil {
		payload, err = s.Encode(request.Replacement)
		if err != nil {
			return protocol.HistoryResult_REFUSED, err
		}
		query = declaration + "DECLARE $revision AS String; UPSERT INTO " + s.QuotedPath() + " (version, revision) VALUES ($version, $revision)"
		args = append(args, sql.Named("revision", payload))
	}
	if _, err := runner.ExecContext(ctx, query, args...); err != nil {
		return protocol.HistoryResult_UNKNOWN, fmt.Errorf("write YDB history: query failed")
	}
	return protocol.HistoryResult_APPLIED, nil
}
