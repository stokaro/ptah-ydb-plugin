package ydbdecl

import (
	"fmt"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
)

// RequireAttributes refuses a Go directive that leaves out an attribute it
// needs, or writes one blank, in the words of the host's annotation parser:
// `missing required annotation attribute "rank"`. names are the attributes
// the directive needs, in the order the host's directive lists them, and the
// first one missing is named. The refusal is a reason a host prints (see
// authored); the host names the directive's place after it.
func RequireAttributes(values map[string]string, names ...string) error {
	for _, name := range names {
		if strings.TrimSpace(values[name]) == "" {
			return authored.New(fmt.Sprintf("missing required annotation attribute %q", name))
		}
	}
	return nil
}
