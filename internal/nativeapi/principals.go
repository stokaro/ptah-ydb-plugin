package nativeapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// ErrPrincipalsRefused marks the specific access refusal of the system views
// that hold principals. A schema inspection can retain its object snapshot
// while reporting principal knowledge as uninspected.
var ErrPrincipalsRefused = errors.New("the server refused to report its users and groups")

// Principals holds database-wide users, groups and memberships. Password
// values and hashes are deliberately absent from every query and result.
type Principals struct {
	Users       []User
	Groups      []string
	Memberships []Membership
}

// User reports the persistent login setting of one YDB user.
type User struct {
	Name    string
	Enabled bool
}

// Membership reports one user or group that belongs to a group.
type Membership struct {
	Group  string
	Member string
}

// ReadPrincipals reads named nonsecret fields from the authentication system
// views. An incomplete row, a duplicate principal or membership, an oversized
// result, or a failed iteration returns no principals. Only the measured
// ABORTED/AccessDenied response is classified as a visibility refusal.
func ReadPrincipals(ctx context.Context, db *sql.DB, database string) (result Principals, err error) {
	defer func() {
		if err != nil {
			result = Principals{}
			if ydb.IsOperationError(err, Ydb.StatusIds_ABORTED) && strings.Contains(err.Error(), "AccessDenied") {
				err = errors.Join(ErrPrincipalsRefused, err)
			}
		}
	}()
	view := func(name string) string { return ydbtype.QuoteIdentifier(path.Join(database, ".sys", name)) }
	seen := make(map[string]bool)
	err = principalRows(ctx, db, "SELECT Sid, IsEnabled FROM "+view("auth_users"), func(rows *sql.Rows) error {
		var name sql.NullString
		var enabled sql.NullBool
		if err := rows.Scan(&name, &enabled); err != nil {
			return err
		}
		if !name.Valid || name.String == "" || !enabled.Valid || seen[name.String] {
			return fmt.Errorf("YDB reports an incomplete or duplicate user")
		}
		seen[name.String] = true
		result.Users = append(result.Users, User{Name: name.String, Enabled: enabled.Bool})
		return nil
	})
	if err != nil {
		return Principals{}, err
	}
	err = principalRows(ctx, db, "SELECT Sid FROM "+view("auth_groups"), func(rows *sql.Rows) error {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if !name.Valid || name.String == "" || seen[name.String] {
			return fmt.Errorf("YDB reports an incomplete or duplicate group")
		}
		seen[name.String] = true
		result.Groups = append(result.Groups, name.String)
		return nil
	})
	if err != nil {
		return Principals{}, err
	}
	members := make(map[Membership]bool)
	err = principalRows(ctx, db, "SELECT GroupSid, MemberSid FROM "+view("auth_group_members"), func(rows *sql.Rows) error {
		var group, member sql.NullString
		if err := rows.Scan(&group, &member); err != nil {
			return err
		}
		edge := Membership{Group: group.String, Member: member.String}
		if !group.Valid || !member.Valid || edge.Group == "" || edge.Member == "" || members[edge] {
			return fmt.Errorf("YDB reports an incomplete or duplicate membership")
		}
		members[edge] = true
		result.Memberships = append(result.Memberships, edge)
		return nil
	})
	return result, err
}

func principalRows(ctx context.Context, db *sql.DB, query string, scan func(*sql.Rows) error) (err error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	count := 0
	for rows.Next() {
		count++
		if count > 4096 {
			return fmt.Errorf("YDB principal view exceeds the inspection limit")
		}
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
