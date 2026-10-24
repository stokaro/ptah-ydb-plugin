package migrationlint

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The recognizers in this file read a statement's words the way the host's
// internal/lintkit reads them, so the facts and the rules that read them find
// the same names the host's rules every dialect shares find.

// hasWordPrefix reports whether the word sequence starts with prefix.
func hasWordPrefix(w []string, prefix ...string) bool {
	if len(w) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if w[i] != p {
			return false
		}
	}
	return true
}

// hasWordSeq reports whether seq appears consecutively anywhere in w.
func hasWordSeq(w []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(w); i++ {
		if hasWordPrefix(w[i:], seq...) {
			return true
		}
	}
	return false
}

// identLike reports whether a word can name a column or table: a quoted name,
// or a bare word starting with a letter, an underscore or a digit. A
// single-quoted string literal is a value, never a name.
func identLike(word string) bool {
	if word == "" {
		return false
	}
	switch word[0] {
	case '`', '"':
		return true
	case '\'':
		return false
	}
	r, _ := utf8.DecodeRuneInString(word)
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// skipIfExists advances past an IF EXISTS at w[j].
func skipIfExists(w []string, j int) int {
	if j+1 < len(w) && w[j] == "IF" && w[j+1] == "EXISTS" {
		return j + 2
	}
	return j
}

// isAlterTable reports whether the words begin with ALTER TABLE.
func isAlterTable(w []string) bool {
	return hasWordPrefix(w, "ALTER", "TABLE")
}

// clauseStarts returns the word positions where the clauses of an ALTER TABLE
// begin: the first word after the table and each word after a comma outside
// parentheses.
func clauseStarts(w []string) []int {
	j := 2
	for j < len(w) {
		if w[j] == "ONLY" {
			j++
			continue
		}
		if next := skipIfExists(w, j); next != j {
			j = next
			continue
		}
		break
	}
	if ref, next := tableRefAt(w, w, j); ref.Normalized != "" {
		j = next
		if j < len(w) && w[j] == "*" {
			j++
		}
	}
	starts := []int{j}
	depth := 0
	for k := j; k < len(w); k++ {
		switch w[k] {
		case "(":
			depth++
		case ")":
			if depth > 0 {
				depth--
			}
		case ",":
			if depth == 0 {
				starts = append(starts, k+1)
			}
		}
	}
	return starts
}

// clauseEnd returns the position of the comma outside parentheses that ends
// the clause starting at start, or the end of the words.
func clauseEnd(w []string, start int) int {
	depth := 0
	for i := start; i < len(w); i++ {
		switch w[i] {
		case "(":
			depth++
		case ")":
			if depth > 0 {
				depth--
			}
		case ",":
			if depth == 0 {
				return i
			}
		}
	}
	return len(w)
}

// createdTableRef returns the folded reference of the table a CREATE TABLE
// creates, or "" for any other statement.
func createdTableRef(w []string) string {
	if len(w) == 0 || w[0] != "CREATE" {
		return ""
	}
	j := 1
	for j < len(w) {
		switch w[j] {
		case "GLOBAL", "LOCAL", "TEMPORARY", "TEMP", "UNLOGGED":
			j++
			continue
		}
		break
	}
	if j >= len(w) || w[j] != "TABLE" {
		return ""
	}
	j++
	if j+2 < len(w) && w[j] == "IF" && w[j+1] == "NOT" && w[j+2] == "EXISTS" {
		j += 3
	}
	ref, _ := tableRefAt(w, w, j)
	return ref.Normalized
}

// tableReference is a table a statement names: as the author spelled it, and
// folded for comparison.
type tableReference struct {
	Name       string
	Normalized string
}

// tableRefAt reads a possibly qualified table reference at w[j] and returns it
// and the position past its end.
func tableRefAt(w, source []string, j int) (tableReference, int) {
	identifier, next, ok := identifierAt(w, source, j)
	if !ok {
		return tableReference{}, j
	}
	display := []string{identifier.Name}
	normalized := []string{identifier.Normalized}
	j = next
	for j < len(w) && w[j] == "." {
		identifier, next, ok = identifierAt(w, source, j+1)
		if !ok {
			break
		}
		display = append(display, identifier.Name)
		normalized = append(normalized, identifier.Normalized)
		j = next
	}
	return tableReference{Name: strings.Join(display, "."), Normalized: strings.Join(normalized, ".")}, j
}

// identifierAt reads one identifier at index: a bare or quoted word, or a
// bracketed name.
func identifierAt(w, source []string, index int) (tableReference, int, bool) {
	if index >= len(w) {
		return tableReference{}, index, false
	}
	if identLike(w[index]) {
		return tableReference{Name: sourceWordAt(w, source, index), Normalized: normalizeIdent(w[index])}, index + 1, true
	}
	if w[index] != "[" {
		return tableReference{}, index, false
	}
	end := index + 1
	for end < len(w) && w[end] != "]" {
		end++
	}
	if end == index+1 || end >= len(w) {
		return tableReference{}, index, false
	}
	return tableReference{
		Name:       "[" + strings.Join(source[index+1:end], " ") + "]",
		Normalized: strings.ToUpper(strings.Join(w[index+1:end], " ")),
	}, end + 1, true
}

// alterTableReference is the table an ALTER TABLE alters, past ONLY and IF
// EXISTS; the zero value for any other statement.
func alterTableReference(w, source []string) tableReference {
	if !hasWordPrefix(w, "ALTER", "TABLE") {
		return tableReference{}
	}
	j := 2
	for j < len(w) {
		if w[j] == "ONLY" {
			j++
			continue
		}
		if next := skipIfExists(w, j); next != j {
			j = next
			continue
		}
		break
	}
	ref, _ := tableRefAt(w, source, j)
	return ref
}

// sourceWordAt is the word at index as the author spelled it, or the
// uppercased word where the spelling is missing.
func sourceWordAt(w, source []string, index int) string {
	if index < len(source) {
		return source[index]
	}
	return w[index]
}

// normalizeIdent strips a name's quotes and uppercases it for comparison.
func normalizeIdent(word string) string {
	return strings.ToUpper(strings.Trim(word, "`\""))
}

// refersToCreated reports whether ref names one of the created tables,
// comparing whole references when both are qualified and last components
// otherwise.
func refersToCreated(created map[string]bool, ref string) bool {
	if ref == "" {
		return false
	}
	if created[ref] {
		return true
	}
	last := ref[strings.LastIndex(ref, ".")+1:]
	for c := range created {
		if c[strings.LastIndex(c, ".")+1:] != last {
			continue
		}
		if !strings.Contains(ref, ".") || !strings.Contains(c, ".") {
			return true
		}
	}
	return false
}

// droppedTablesNotCreated returns the tables a DROP TABLE names that were not
// created earlier, and whether every target was read.
func droppedTablesNotCreated(w, source []string, created map[string]bool) ([]tableReference, bool) {
	var unsafe []tableReference
	j := skipIfExists(w, 2)
	for {
		if j < len(w) && w[j] == "ONLY" {
			j++
		}
		ref, next := tableRefAt(w, source, j)
		if ref.Normalized == "" {
			return unsafe, false
		}
		if !refersToCreated(created, ref.Normalized) {
			unsafe = append(unsafe, ref)
		}
		if next < len(w) && w[next] == "*" {
			next++
		}
		if next < len(w) && w[next] == "," {
			j = next + 1
			continue
		}
		return unsafe, true
	}
}

// keyColumnList reads ( a, b (10), c DESC, (expr) ) at w[j] and returns the
// names it lists, in order, and the position past the closing parenthesis. A
// key part that is an expression contributes no name.
func keyColumnList(w, source []string, j int) ([]string, int, bool) {
	if j >= len(w) || w[j] != "(" {
		return nil, j, false
	}
	var columns []string
	depth := 0
	partStart := true
	for k := j; k < len(w); k++ {
		switch w[k] {
		case "(":
			depth++
			partStart = depth == 1
			continue
		case ")":
			depth--
			if depth == 0 {
				return columns, k + 1, len(columns) > 0
			}
		case ",":
			if depth == 1 {
				partStart = true
			}
		default:
			if depth == 1 && partStart && identLike(w[k]) {
				columns = append(columns, sourceWordAt(w, source, k))
			}
			partStart = false
		}
	}
	return nil, j, false
}
