//go:build integration

package integration_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A declaration of a topic, a changefeed, an external data source or table,
// a resource pool or classifier, a streaming query, an async replication or a
// transfer that the provider refuses while it decodes the source says why, as
// the built-in engine at host 74de0b65b did on 25.1.4.7 and 26.2.1.14: the
// object, the attribute and what it takes. The provider printed the code
// alone, `provider decode source failed (InvalidArgument)`. The reason leaves
// out the value the declaration wrote (each row's DO_NOT_REPORT), which the
// built-in quoted. A Go directive that leaves out a required attribute is
// refused in the words of the host's annotation parser, and the host names the
// directive's place after the reason.
func TestHostSaysWhyItRefusesANativeDeclaration_FailurePath(t *testing.T) {
	const refused = "provider decode source failed (InvalidArgument): "
	tests := []struct {
		name, format, text, want string
	}{
		{name: "a YAML topic", format: "yaml", text: "topics:\n  events:\n    schema: {dir}\n    retention_period: DO_NOT_REPORT\n",
			want: refused + `topic "events": invalid retention_period: takes an ISO 8601 duration such as PT12H or P1D`},
		{name: "a Go topic", format: "go", text: goRefused(`//ptah:schema:topic name="events" schema="{dir}" retention_period="DO_NOT_REPORT"`),
			want: refused + `invalid retention_period: takes an ISO 8601 duration such as PT12H or P1D on //ptah:schema:topic at Refused`},
		{name: "a YAML changefeed", format: "yaml",
			text: "tables:\n  items:\n    columns:\n      id: {type: Uint64, primary: true}\n    changefeeds:\n      f:\n        mode: UPDATES\n        format: JSON\n" +
				"        consumers:\n          c: {important: DO_NOT_REPORT}\n",
			want: refused + `table "items": changefeed "f": consumer "c": invalid important: takes true or false`},
		{name: "a YAML external data source", format: "yaml",
			text: "external_data_sources:\n  s:\n    schema: {dir}\n    source_type: ObjectStorage\n    location: https://DO_NOT_REPORT/\n",
			want: refused + `external data source "s": invalid auth_method: a data source needs an auth_method, such as NONE or BASIC`},
		{name: "a Go external data source", format: "go",
			text: goRefused(`//ptah:schema:externaldatasource name="s" schema="{dir}" source_type="ObjectStorage" location="https://DO_NOT_REPORT/"`),
			want: refused + `missing required annotation attribute "auth_method" on //ptah:schema:externaldatasource at Refused`},
		{name: "a YAML external table", format: "yaml", text: "external_tables:\n  t:\n    schema: {dir}\n    data_source: s\n    location: DO_NOT_REPORT/\n",
			want: refused + `external table "t": invalid columns: an external table needs a column`},
		{name: "a YAML resource pool", format: "yaml", text: "resource_pools:\n  batch:\n    total_cpu_limit_percent_per_node: DO_NOT_REPORT\n",
			want: refused + `resource pool "batch": invalid total_cpu_limit_percent_per_node: takes a percentage from 0 to 100; leave it out for no limit`},
		{name: "a Go classifier", format: "go", text: goRefused(`//ptah:schema:resourcepool:classifier name="c" resource_pool="default" member_name="DO_NOT_REPORT"`),
			want: refused + `missing required annotation attribute "rank" on //ptah:schema:resourcepool:classifier at Refused`},
		{name: "a YAML streaming query", format: "yaml", text: "streaming_queries:\n  q:\n    schema: {dir}\n    text: \"DROP TABLE DO_NOT_REPORT;\"\n",
			want: refused + `streaming query "q": streaming query text must contain data statements only`},
		{name: "a YAML async replication", format: "yaml",
			text: "async_replications:\n  mirror:\n    schema: {dir}\n    connection_string: grpc://root:DO_NOT_REPORT@primary:2136/?database=/prod\n    items:\n      - {source: a, target: ra}\n",
			want: refused + `async replication "mirror": invalid connection_string: carries credentials, which a replication reads from a secret; ` +
				`name the secret with token_secret_name or user and password_secret_name`},
		{name: "a Go transfer", format: "go",
			text: goRefused(`//ptah:schema:transfer name="ingest" schema="{dir}" source="tp" target="t" using="($m) -> { return []; }" flush_interval="PT7.5S"`),
			want: refused + `invalid flush_interval: YDB keeps whole seconds, and drops the fraction of this one on //ptah:schema:transfer at Refused`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			_, directory, _ := inspectionFixture(c)

			output, err := hostApplySource(c, directory, test.format, strings.ReplaceAll(test.text, "{dir}", directory))

			c.Assert(err, qt.IsNotNil)
			c.Assert(output, qt.Contains, test.want)
			c.Assert(output, qt.Not(qt.Contains), "DO_NOT_REPORT")
		})
	}
}

// goRefused is a Go file whose struct Refused carries directive.
func goRefused(directive string) string {
	return "package entities\n\n" + directive + "\ntype Refused struct{}\n"
}
