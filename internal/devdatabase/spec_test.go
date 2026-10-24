package devdatabase_test

import (
	"cmp"
	"encoding/json"
	"errors"
	"os"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/devdatabase"
)

type hostRow struct {
	Spec    string `json:"spec"`
	Outcome string `json:"outcome"`
	Image   string `json:"image"`
	URL     string `json:"url"`
}

// divergences are the specs this package refuses and the host accepts.
var divergences = map[string]string{
	"docker://postgres/16/dev":                                            "the host provisions other engines itself; only the ydb engine is this provider's",
	"docker://ydb/26.2.1.14/local?token=t":                                "a token would travel back in the reply, and local-ydb takes no credential",
	"docker://ydb/26.2.1.14/local?unknown=1":                              "the host hands the parameter to a connection that refuses it later; this package refuses it before a container starts",
	"docker://ydb/26.2.1.14/local?go_balancer=nonsense":                   "refused before a container starts, not when the host connects",
	"docker://ydb/26.2.1.14/local?go_fake_tx=true":                        "refused before a container starts, not when the host connects",
	"docker://ydb/26.2.1.14/local?go_balancer=disable&go_balancer=single": "a repeated parameter is refused before a container starts",
	"docker://ydb/-oops/local":                                            "a tag that is not a Docker tag is refused",
}

func readCorpus(c *qt.C) []hostRow {
	raw, err := os.ReadFile("testdata/oracle/host.json")
	c.Assert(err, qt.IsNil)
	var rows []hostRow
	c.Assert(json.Unmarshal(raw, &rows), qt.IsNil)
	return rows
}

func TestParseSpecAgreesWithHost_Verdict(t *testing.T) {
	c := qt.New(t)
	rows := readCorpus(c)
	c.Assert(len(rows) >= 30, qt.IsTrue, qt.Commentf("the corpus holds %d rows", len(rows)))
	for _, row := range rows {
		t.Run(row.Spec, func(t *testing.T) {
			c := qt.New(t)
			want := cmp.Or(map[bool]string{true: "refused"}[divergences[row.Spec] != ""], row.Outcome)

			_, err := devdatabase.ParseSpec(row.Spec)

			c.Assert(map[bool]string{true: "refused", false: "ok"}[err != nil], qt.Equals, want)
		})
	}
}

func TestParseSpecAgreesWithHost_Address(t *testing.T) {
	c := qt.New(t)
	var agreed []hostRow
	for _, row := range readCorpus(c) {
		if _, diverges := divergences[row.Spec]; row.Outcome == "ok" && !diverges {
			agreed = append(agreed, row)
		}
	}
	c.Assert(len(agreed) >= 10, qt.IsTrue, qt.Commentf("the corpus holds %d accepted rows", len(agreed)))
	for _, row := range agreed {
		t.Run(row.Spec, func(t *testing.T) {
			c := qt.New(t)

			spec, err := devdatabase.ParseSpec(row.Spec)

			c.Assert(err, qt.IsNil)
			c.Assert(spec.Image, qt.Equals, row.Image)
			c.Assert(spec.URL("127.0.0.1:12136"), qt.Equals, row.URL)
		})
	}
}

func TestParseSpec_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, spec, want string }{
		{"other engine", "docker://postgres/16/dev", `unsupported docker image "postgres"`},
		{"engine in upper case", "docker://YDB/26.2.1.14", `unsupported docker image "YDB"`},
		{"another database", "docker://ydb/26.2.1.14/dev", `the database is not one the ydbplatform/local-ydb image serves: name local`},
		{"too many segments", "docker://ydb/26.2.1.14/local/extra", `the docker path has more than <tag>/<database>`},
		{"token", "docker://ydb/26.2.1.14?token=test-secret", `docker parameter "token" would point the connection away from the container this spec provisions; remove it`},
		{"database parameter", "docker://ydb/26.2.1.14?database=/other", `docker parameter "database" would point the connection away from the container this spec provisions; remove it`},
		{"realm", "docker://ydb/26.2.1.14?dev_realm=r1", `docker parameter "dev_realm" would point .*`},
		{"unknown parameter", "docker://ydb/26.2.1.14?test-secret=1", `the docker parameters are not accepted by a YDB address: unsupported YDB URL parameter.*`},
		{"bad balancer", "docker://ydb/26.2.1.14?go_balancer=nonsense", `the docker parameters are not accepted by a YDB address: .*`},
		{"bad tag", "docker://ydb/-oops", `the image tag is empty or not a Docker tag`},
		{"not docker", "ydb://ydb/26.2.1.14", `the dev database spec is not a docker:// or docker\+ydb:// URL`},
		{"another engine's image form", "docker+postgres://_/postgres:17/dev", `the dev database spec is not a docker:// or docker\+ydb:// URL`},
		{"malformed", "docker://ydb/26.2.1.14?a%zz=1", `the docker parameters are malformed`},
		{"image form without an image", "docker+ydb://_/", `the docker\+ydb spec names no image`},
		{"image form with another database", "docker+ydb://_/ydbplatform/local-ydb:26.2.1.14/dev", `the database is not one a YDB dev image serves: name local`},
		{"image form with a path past the database", "docker+ydb://_/ydbplatform/local-ydb:26.2.1.14/local/extra", `the database is not one a YDB dev image serves: name local`},
		{"image form that names no image", "docker+ydb://_/Not An Image", `the docker\+ydb spec does not name a Docker image`},
		{"image form token", "docker+ydb://_/ydbplatform/local-ydb:26.2.1.14?token=test-secret", `docker parameter "token" would point .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			spec, err := devdatabase.ParseSpec(test.spec)

			var refusal *devdatabase.Refusal
			c.Assert(errors.As(err, &refusal), qt.IsTrue)
			c.Assert(refusal.Unavailable, qt.IsFalse)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(err.Error(), qt.Not(qt.Contains), "test-secret=")
			c.Assert(spec, qt.Equals, devdatabase.Spec{})
		})
	}
}

func TestParseSpec_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, spec, image, address string }{
		{"no path", "docker://ydb", "ydbplatform/local-ydb:latest", "ydb://127.0.0.1:1/local?go_balancer=disable"},
		{"tag", "docker://ydb/26.2.1.14", "ydbplatform/local-ydb:26.2.1.14", "ydb://127.0.0.1:1/local?go_balancer=disable"},
		{"operator wins", "docker://ydb/25.1.4.7/local?go_balancer=single&go_default_idempotent=true", "ydbplatform/local-ydb:25.1.4.7", "ydb://127.0.0.1:1/local?go_balancer=single&go_default_idempotent=true"},
		{"image form", "docker+ydb://_/ydbplatform/local-ydb:24.3/local", "ydbplatform/local-ydb:24.3", "ydb://127.0.0.1:1/local?go_balancer=disable"},
		{"image form without a database", "docker+ydb://_/ydbplatform/local-ydb:26.2.1.14", "ydbplatform/local-ydb:26.2.1.14", "ydb://127.0.0.1:1/local?go_balancer=disable"},
		{"image form with an empty host", "docker+ydb:///ydbplatform/local-ydb:26.2.1.14/local", "ydbplatform/local-ydb:26.2.1.14", "ydb://127.0.0.1:1/local?go_balancer=disable"},
		{"image form with a registry", "docker+ydb://cr.yandex/yc/yandex-docker-local-ydb:24.1/local", "cr.yandex/yc/yandex-docker-local-ydb:24.1", "ydb://127.0.0.1:1/local?go_balancer=disable"},
		{"image form without a tag", "docker+ydb://_/mirror/local-ydb/local", "mirror/local-ydb:latest", "ydb://127.0.0.1:1/local?go_balancer=disable"},
		{"image form in upper case", "DOCKER+YDB://_/ydbplatform/local-ydb:26.2.1.14/local?go_balancer=single", "ydbplatform/local-ydb:26.2.1.14", "ydb://127.0.0.1:1/local?go_balancer=single"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			spec, err := devdatabase.ParseSpec(test.spec)

			c.Assert(err, qt.IsNil)
			c.Assert(spec.Image, qt.Equals, test.image)
			c.Assert(spec.URL("127.0.0.1:1"), qt.Equals, test.address)
		})
	}
}
