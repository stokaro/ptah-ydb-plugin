package yql

import (
	"context"
	"strings"
	"unicode/utf8"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const maxAnalysisBytes = 3 << 20
const maxAnalysisSources = 256
const maxSourceTokens = 65536

// Analyze reads the whole batch offline. A lexical or framing error returns no
// prefix. Native safety facts never infer that an unknown statement is safe.
func Analyze(ctx context.Context, request *protocol.AnalyzeSQLRequest) (*protocol.SQLAnalysis, error) {
	if request.GetEngine() != "ydb" {
		return nil, status.Error(codes.InvalidArgument, "YDB SQL analysis requires engine ydb")
	}
	if len(request.Sources) > maxAnalysisSources || proto.Size(request) > maxAnalysisBytes {
		return nil, status.Error(codes.ResourceExhausted, "YDB SQL analysis batch exceeds its limit")
	}
	result := &protocol.SQLAnalysis{Complete: true}
	for _, source := range request.Sources {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		file, err := analyzeSource(ctx, source)
		if err != nil {
			return nil, err
		}
		result.Files = append(result.Files, file)
		if proto.Size(result) > maxAnalysisBytes {
			return nil, status.Error(codes.ResourceExhausted, "YDB SQL analysis response exceeds its limit")
		}
	}
	if err := provider.ValidateSQLAnalysis(request, result); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}

func analyzeSource(ctx context.Context, source string) (*protocol.SQLFile, error) {
	if !utf8.ValidString(source) || strings.ContainsRune(source, 0) {
		return nil, invalidSource()
	}
	stream := reader{input: source}
	file := &protocol.SQLFile{}
	bodies := Bodies{}
	start, startLine, line := -1, 0, 1
	var words, original []string
	tokenCount := 0
	emit := func(end int) {
		safety, reason := safetyOf(words)
		file.Statements = append(file.Statements, &protocol.SQLStatement{Start: uint32(start), End: uint32(end), Line: uint32(startLine), Words: words, SourceWords: original, Canonical: strings.Join(words, " "), Safety: safety, Reason: reason})
		start = -1
		words = nil
		original = nil
		bodies = Bodies{}
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		token := stream.NextToken()
		if token.Type == TokenEOF {
			break
		}
		tokenLine := line
		line += strings.Count(token.Value, "\n")
		if token.Type == TokenWhitespace {
			continue
		}
		if err := validateToken(token); err != nil {
			return nil, err
		}
		if start < 0 {
			start, startLine = token.Start, tokenLine
		}
		switch token.Type {
		case TokenComment, TokenUnknown:
			continue
		case TokenSemicolon:
			bodies.Separate()
			if !bodies.Inside() {
				if len(words) > 0 {
					emit(token.End)
				}
				continue
			}
		case TokenIdentifier:
			bodies.Word(strings.ToUpper(token.Value))
		case TokenOperator:
			if token.Value == "}" && bodies.braces == 0 {
				return nil, invalidSource()
			}
			bodies.Symbol(token.Value)
		default:
			// A literal separates keyword pairs just as punctuation does.
			bodies.Separate()
		}
		tokenCount++
		if tokenCount > maxSourceTokens {
			return nil, status.Error(codes.ResourceExhausted, "YDB SQL source exceeds its token limit")
		}
		word := token.Value
		if token.Type != TokenString && !strings.HasPrefix(word, "`") {
			word = strings.ToUpper(word)
		}
		words = append(words, word)
		original = append(original, token.Value)
	}
	if bodies.Inside() {
		return nil, invalidSource()
	}
	if start >= 0 {
		if len(words) > 0 {
			emit(len(source))
		} else if len(file.Statements) > 0 {
			file.Statements[len(file.Statements)-1].End = uint32(len(source))
		} else {
			return nil, status.Error(codes.InvalidArgument, "YDB SQL analysis requires an executable statement beside comments or terminators")
		}
	}
	return file, nil
}

func validateToken(token Token) error {
	switch token.Type {
	case TokenString:
		if _, ok := StringValue(token.Value); !ok {
			return invalidSource()
		}
	case TokenIdentifier:
		if _, ok := IdentifierValue(token.Value); !ok {
			return invalidSource()
		}
	case TokenComment:
		if strings.HasPrefix(token.Value, "/*") && !strings.HasSuffix(token.Value, "*/") {
			return invalidSource()
		}
		// Splitting can move a later comment to a new query head. Refuse settings
		// everywhere so an ANSI directive cannot change the server's lexical mode.
		if strings.HasPrefix(token.Value, "--!") && strings.TrimSpace(token.Value) != "--!syntax_v1" {
			return unsupportedSetting()
		}
	case TokenUnknown:
		if strings.TrimSpace(token.Value) != "--!syntax_v1" {
			return unsupportedSetting()
		}
	}
	return nil
}
func invalidSource() error {
	return status.Error(codes.InvalidArgument, "YDB SQL has invalid or incomplete lexical structure")
}
func unsupportedSetting() error {
	return status.Error(codes.InvalidArgument, "YDB SQL analysis supports only the default lexer and syntax_v1 translation setting")
}
