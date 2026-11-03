package nativeapi

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// Secret checks presence before and after one scheme-secret operation. Values
// never appear in its inputs; Execute resolves the approved reference.
type Secret struct {
	Scheme  Ydb_Scheme_V1.SchemeServiceClient
	Execute func(context.Context, string) error
}

// NewSecret binds a scheme client and an approved-input statement runner.
func NewSecret(driver *ydb.Driver, execute func(context.Context, string) error) *Secret {
	return &Secret{Scheme: Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(driver)), Execute: execute}
}

// Apply checks presence, dispatches once, and confirms presence or absence.
// Rotation additionally confirms unchanged access. Neither presence nor an
// access check can detect another writer rotating an unreadable value.
func (s *Secret) Apply(ctx context.Context, database string, value *protocol.Envelope, beforeWrite func(context.Context) error) (bool, error) {
	operation, err := schema.NormalizeSecretOperation(&protocol.ValueInput{Id: "execute", Representation: protocol.ValueInput_OPERATION, Value: value})
	if err != nil {
		return false, err
	}
	owner := proto.CloneOf(operation.NormalizedOwner)
	if owner.Database != "" && owner.Database != database {
		return false, fmt.Errorf("secret operation targets another database")
	}
	owner.Database = database
	if err := schema.ValidateNativeOwner(owner); err != nil {
		return false, err
	}
	before, after, err := schema.SecretOperands(schema.SecretOperationChange(operation.Value))
	if err != nil {
		return false, err
	}
	display := secrets.Display(owner.Schema, owner.Name)
	held, err := s.read(ctx, owner)
	if err != nil {
		return false, refused("read secret "+display, err)
	}
	if (before == nil) != (held == nil) {
		return false, &Refusal{Err: fmt.Errorf("secret %s changed since the plan read it", display)}
	}
	absolute := path.Join(database, owner.Schema, owner.Name)
	statement := secrets.DropStatement("", absolute)
	if after != nil {
		intent, err := schema.DesiredSecret(after)
		if err != nil {
			return false, err
		}
		statement = secrets.CreateStatement("", absolute, intent.ValueEnv)
		if before != nil {
			statement = secrets.AlterStatement("", absolute, intent.ValueEnv)
		}
	}
	if err := beforeWrite(ctx); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := s.Execute(ctx, statement); err != nil {
		return true, err
	}
	current, err := s.read(ctx, owner)
	if err != nil {
		return true, err
	}
	if (after == nil) != (current == nil) {
		return true, fmt.Errorf("secret presence after execution was not confirmed")
	}
	if before != nil && after != nil && !proto.Equal(held, current) {
		return true, fmt.Errorf("secret access changed during rotation")
	}
	return true, nil
}

// read lists the secret's directory and describes the secret's access, or
// returns nil where the directory holds no entry of its name, or does not
// exist; see [ListDirectory]. CREATE SECRET creates the directories it needs.
func (s *Secret) read(ctx context.Context, owner *protocol.Reference) (*Ydb_Scheme.Entry, error) {
	directory := path.Join(owner.Database, owner.Schema)
	listing, err := ListDirectory(ctx, SchemeLister(s.Scheme), owner.Database, owner.Schema)
	if err != nil {
		return nil, err
	}
	kind := listing.GetSelf().GetType()
	if listing.GetSelf().GetName() != path.Base(directory) || (kind != Ydb_Scheme.Entry_DIRECTORY && (owner.Schema != "" || kind != Ydb_Scheme.Entry_DATABASE)) ||
		len(listing.Children) > 4096 {
		return nil, fmt.Errorf("invalid secret namespace")
	}
	names := make(map[string]bool)
	var found *Ydb_Scheme.Entry
	for _, entry := range listing.Children {
		if entry.GetName() == "" || names[entry.GetName()] || strings.ContainsAny(entry.GetName(), "/\\\x00") || entry.Name == "." || entry.Name == ".." {
			return nil, fmt.Errorf("invalid secret namespace entry")
		}
		names[entry.Name] = true
		if entry.Name == owner.Name {
			found = entry
		}
	}
	if found == nil {
		return nil, nil
	}
	if found.Type != Ydb_Scheme.Entry_SECRET {
		return nil, fmt.Errorf("secret path has a different occupant")
	}
	described, err := s.Scheme.DescribePath(ctx, &Ydb_Scheme.DescribePathRequest{Path: path.Join(directory, owner.Name)})
	if err != nil {
		return nil, fmt.Errorf("describe secret access: transport failed")
	}
	var result Ydb_Scheme.DescribePathResult
	if err := Operation(described.GetOperation(), &result); err != nil {
		return nil, err
	}
	if err := schema.Known(&result); err != nil {
		return nil, err
	}
	if result.GetSelf().GetName() != owner.Name || result.GetSelf().GetType() != Ydb_Scheme.Entry_SECRET {
		return nil, fmt.Errorf("secret description has a different identity")
	}
	// Compare access only. Runtime size and creation metadata do not describe it.
	entry := &Ydb_Scheme.Entry{Owner: result.Self.Owner, Permissions: result.Self.Permissions, EffectivePermissions: result.Self.EffectivePermissions}
	for _, permissions := range [][]*Ydb_Scheme.Permissions{entry.Permissions, entry.EffectivePermissions} {
		seen := make(map[string]bool)
		for _, permission := range permissions {
			if permission.GetSubject() == "" || seen[permission.GetSubject()] {
				return nil, fmt.Errorf("invalid secret access entry")
			}
			seen[permission.Subject] = true
			slices.Sort(permission.PermissionNames)
		}
		slices.SortFunc(permissions, func(a, b *Ydb_Scheme.Permissions) int { return strings.Compare(a.Subject, b.Subject) })
	}
	return entry, nil
}
