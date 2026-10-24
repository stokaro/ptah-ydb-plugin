package yql

import "strings"

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

// IsWord reports an identifier token spelled value, compared with Unicode case
// folding.
func (t Token) IsWord(value string) bool {
	return t.Type == TokenIdentifier && strings.EqualFold(t.Value, value)
}

// IsOperator reports an operator token spelled value.
func (t Token) IsOperator(value string) bool {
	return t.Type == TokenOperator && strings.EqualFold(t.Value, value)
}
