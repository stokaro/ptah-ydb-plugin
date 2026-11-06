package backend

import (
	"context"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// scannedKinds maps the scanner's token types to the contract's token kinds.
var scannedKinds = map[yql.TokenType]protocol.SQLToken_Kind{
	yql.TokenWhitespace: protocol.SQLToken_WHITESPACE,
	yql.TokenIdentifier: protocol.SQLToken_IDENTIFIER,
	yql.TokenString:     protocol.SQLToken_STRING,
	yql.TokenSemicolon:  protocol.SQLToken_SEMICOLON,
	yql.TokenOperator:   protocol.SQLToken_OPERATOR,
	yql.TokenComment:    protocol.SQLToken_COMMENT,
	yql.TokenUnknown:    protocol.SQLToken_UNKNOWN,
}

// ScanSQL returns the tokens of one YQL text by the rules of internal/yql, the
// rules the host's built-in YDB engine read YQL by. A string carries the text
// it stands for and an identifier the name it stands for, where YDB reads
// one. A translation setting at the head of the text is UNKNOWN, so a host
// reader that removes comments keeps it.
//
// SEMICOLON is the separator between two statements of the text, so a
// semicolon that ends a statement inside a body that holds statements -- a
// DEFINE ACTION or DEFINE SUBQUERY, a DO BEGIN block or a lambda -- is an
// OPERATOR, as internal/yqlquery cuts a migration body. The host splits a text
// at its SEMICOLON tokens, and the built-in engine kept such a body whole:
// without this, ptah-compat migrate apply listed `DO BEGIN UPSERT ...` and the
// statements inside it as separate applied statements.
func (*Provider) ScanSQL(ctx context.Context, request *protocol.ScanSQLRequest) (*protocol.SQLScan, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	tokens := yql.Scan(request.GetText(), request.GetHead())
	result := &protocol.SQLScan{Tokens: make([]*protocol.SQLToken, 0, len(tokens))}
	var bodies yql.Bodies
	for _, token := range tokens {
		kind := scannedKinds[token.Type]
		switch token.Type {
		case yql.TokenIdentifier:
			bodies.Word(strings.ToUpper(token.Value))
		case yql.TokenOperator:
			bodies.Symbol(token.Value)
		case yql.TokenSemicolon:
			bodies.Separate()
			if bodies.Inside() {
				kind = protocol.SQLToken_OPERATOR
			} else {
				bodies = yql.Bodies{}
			}
		}
		// #nosec G115 -- the SDK bounds a request below 4 MiB, so every offset fits.
		scanned := &protocol.SQLToken{Kind: kind, End: uint32(token.End)}
		if value, ok := tokenValue(token); ok {
			scanned.Value = &value
		}
		result.Tokens = append(result.Tokens, scanned)
	}
	return result, nil
}

// tokenValue is what a string or an identifier token stands for, and whether
// YDB reads one.
func tokenValue(token yql.Token) (string, bool) {
	switch token.Type {
	case yql.TokenString:
		return yql.StringValue(token.Value)
	case yql.TokenIdentifier:
		return yql.IdentifierValue(token.Value)
	default:
		return "", false
	}
}

var _ provider.SQLScanner = (*Provider)(nil)
