package devdatabase

import (
	"context"
	"errors"
	"net/url"
	"path"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/devclean"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Realms creates and removes the dev realms of a database the operator
// names. SQL cannot create a YDB database, and a cluster's databases are
// created by its administrators, so a dev database there is a directory of the
// database, a realm: `ptah_dev/<name>`, which a session whose address names it
// treats as its whole database.
type Realms interface {
	// Create creates the realm name in the database address names.
	Create(ctx context.Context, address connection.Address, name string) error
	// Remove removes the realm name, with everything in it, and the realms'
	// directory when no realm is left in it. A realm that is gone is removed
	// already.
	Remove(ctx context.Context, address connection.Address, name string) error
}

// realmLease is a realm a lease holds: the operator's address, without a
// realm, and the realm's name.
type realmLease struct {
	address connection.Address
	name    string
}

// realmNameBytes is the entropy of a realm's name. Two runs that drew the
// same name would share a realm, and creating a directory that exists
// succeeds, so the name carries enough random bytes that a collision is not a
// practical outcome.
const realmNameBytes = 8

// enterRealm creates a realm of the run's own in the database the operator's
// address names, as the host's embedded engine does (internal/ydbrealm.Enter),
// and returns the realm's address: the operator's, with the same scheme, host
// and credentials, naming the realm. An address that names a realm already
// gets a new one in its place. The release removes the realm with everything
// in it.
func (p *Provisioner) enterRealm(ctx context.Context, raw string) (*protocol.DevDatabase, error) {
	if p.realms == nil {
		return refused(unsupported("this provider creates no dev realm"))
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return refused(unsupported("the dev realm address is not a URL"))
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return refused(unsupported("the dev realm address parameters are malformed"))
	}
	query.Del("dev_realm")
	parsed.RawQuery = query.Encode()
	database, err := connection.Parse(parsed.String())
	if err != nil {
		return refused(unsupported("the dev realm address is not a YDB address: %s", err))
	}
	name := token(realmNameBytes)
	query.Set("dev_realm", name)
	parsed.RawQuery = query.Encode()
	if _, err := connection.Parse(parsed.String()); err != nil {
		return refused(unsupported("the dev realm address is not a YDB address: %s", err))
	}
	if err := p.realms.Create(ctx, database, name); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return refused(&Refusal{Unavailable: true, Reason: "the database the dev realm address names could not take a realm: " + err.Error()})
	}
	return &protocol.DevDatabase{Lease: p.hold(lease{realm: &realmLease{address: database, name: name}}), Url: parsed.String()}, nil
}

// removeRealm removes a realm a lease holds.
func (p *Provisioner) removeRealm(ctx context.Context, held realmLease) error {
	return p.realms.Remove(ctx, held.address, held.name)
}

// NativeRealms creates and removes realms through native connections to the
// database, opened for each operation and closed after it.
type NativeRealms struct{}

// Create makes the realm's directory, and the realms' directory above it when
// it is missing.
func (NativeRealms) Create(ctx context.Context, address connection.Address, name string) (err error) {
	opened, err := connection.Open(ctx, address)
	if err != nil {
		return errors.New("the database could not be reached")
	}
	defer func() { err = errors.Join(err, opened.Close()) }()
	if err := opened.Driver.Scheme().MakeDirectory(ctx, path.Join(address.Database(), connection.RealmDirectory, name)); err != nil {
		return errors.New("the realm's directory could not be created")
	}
	return nil
}

// Remove removes the realm through the database it lives in.
func (NativeRealms) Remove(ctx context.Context, address connection.Address, name string) (err error) {
	opened, err := connection.Open(ctx, address)
	if err != nil {
		return errors.New("the database that holds the dev realm could not be reached")
	}
	defer func() { err = errors.Join(err, opened.Close()) }()
	cleaner := devclean.NewNative(opened, nil)
	return devclean.RemoveRealm(ctx, cleaner, cleaner, address.Database(), name)
}
