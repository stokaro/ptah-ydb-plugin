package backend

// White-box testing required: what a refusal of a statement with bound values
// becomes is decided in inputError, between the driver's error and the unit's
// outcome, and only a live YDB server produces the driver's error through an
// exported call. The live tests under integration/ cover the refusal end to
// end.

import (
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Issue"

	"github.com/stokaro/ptah-ydb-plugin/internal/secretvalue"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbfailure"
)

// refusal is an operation status YDB answered with, as the SDK reports one.
type refusal struct {
	code   int32
	issues []*Ydb_Issue.IssueMessage
}

func (r refusal) Error() string {
	return "operation/PRECONDITION_FAILED: " + r.issues[0].GetIssues()[0].GetMessage()
}
func (r refusal) Code() int32                       { return r.code }
func (r refusal) Issues() []*Ydb_Issue.IssueMessage { return r.issues }

// A refused statement that held a bound password keeps YDB's status and
// issues, so its outcome reads as refused rather than unknown, and loses the
// password from its text and from every issue.
func TestInputErrorKeepsTheAnswerWithoutTheValue_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	expansion, err := secretvalue.Expand("CREATE USER app PASSWORD $PTAH_SECRET_PASSWORD_APP;",
		func(string) (string, bool) { return "pw-SENTINEL", true })
	c.Assert(err, qt.IsNil)
	answered := refusal{code: int32(Ydb.StatusIds_PRECONDITION_FAILED), issues: []*Ydb_Issue.IssueMessage{{
		Message: "Executing ESchemeOpAlterLogin", IssueCode: 2029,
		Issues: []*Ydb_Issue.IssueMessage{{Message: "Password pw-SENTINEL contains unacceptable characters"}},
	}}}

	redacted := inputError(expansion, answered)
	answer := ydbfailure.Read(redacted)

	c.Assert(redacted, qt.ErrorMatches, `operation/PRECONDITION_FAILED: Password \[secret\] contains unacceptable characters`)
	c.Assert(answer.Status, qt.Equals, Ydb.StatusIds_PRECONDITION_FAILED)
	c.Assert(answer.Code, qt.Equals, uint32(2029))
	c.Assert(answer.Issues, qt.DeepEquals, []string{"Executing ESchemeOpAlterLogin", "Password [secret] contains unacceptable characters"})
	c.Assert(answer.Confirmed("CREATE USER app PASSWORD $PTAH_SECRET_PASSWORD_APP"), qt.IsTrue)
	c.Assert(errors.Is(redacted, answered), qt.IsFalse)
}

// An error that is no answer of YDB's keeps its text alone, the values taken
// out.
func TestInputErrorKeepsTheAnswerWithoutTheValue_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	expansion, err := secretvalue.Expand("CREATE USER app PASSWORD $PTAH_SECRET_PASSWORD_APP;",
		func(string) (string, bool) { return "pw-SENTINEL", true })
	c.Assert(err, qt.IsNil)

	redacted := inputError(expansion, errors.New("transport lost while sending 'pw-SENTINEL'"))

	c.Assert(redacted, qt.ErrorMatches, `transport lost while sending '\[secret\]'`)
	c.Assert(ydbfailure.Read(redacted).Status, qt.Equals, Ydb.StatusIds_STATUS_CODE_UNSPECIFIED)
}

// Only a unit that defines a secret's value needs a server that keeps schema
// secrets: one that writes in a user's password alone runs on 25.1 too.
func TestDefinesSecret_HappyPath(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{name: "a user's password", query: "CREATE USER app PASSWORD $PTAH_SECRET_PASSWORD_APP;"},
		{name: "a secret", query: "CREATE SECRET s WITH (value = $PTAH_SECRET_KEY);", want: true},
		{name: "a statement with no input", query: "SELECT 1;"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(definesSecret(test.query), qt.Equals, test.want)
		})
	}
}
