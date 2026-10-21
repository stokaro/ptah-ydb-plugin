package yql

// TokenType distinguishes YQL lexical forms before semantic interpretation.
type TokenType uint8

const (
	TokenEOF TokenType = iota
	TokenWhitespace
	TokenIdentifier
	TokenString
	TokenSemicolon
	TokenOperator
	TokenComment
	TokenUnknown
)

// Token preserves a lexical form and its exact byte range in the source.
type Token struct {
	Type       TokenType
	Value      string
	Start, End int
}
