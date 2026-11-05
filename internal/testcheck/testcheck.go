// Package testcheck builds the quicktest checkers the provider's tests assert
// through. They format a failed value the way quicktest does, except that a
// protobuf message, or a slice of them, is printed as protobuf text.
//
// quicktest prints one message through its String method, but a slice of
// messages through kr/pretty, which walks every message's reflection state. A
// failed qt.HasLen on a slice of comparison results printed 21 million lines,
// and on CI it ran until the runner stopped the job, so the live suite read as
// hung for hours over an assertion that failed in seconds.
package testcheck

import (
	"reflect"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

// New returns a checker for t that formats failures with Format.
func New(t testing.TB) *qt.C {
	c := qt.New(t)
	c.SetFormat(Format)
	return c
}

// Format formats v for a test failure. A protobuf message is printed as
// protobuf text, a slice or array of messages as one such text per element,
// and any other value as quicktest.Format prints it.
func Format(v any) string {
	if message, ok := v.(proto.Message); ok {
		return messageText(message)
	}
	value := reflect.ValueOf(v)
	if !value.IsValid() || (value.Kind() != reflect.Slice && value.Kind() != reflect.Array) ||
		!value.Type().Elem().Implements(reflect.TypeFor[proto.Message]()) {
		return qt.Format(v)
	}
	var text strings.Builder
	text.WriteString(value.Type().String() + "{\n")
	for i := range value.Len() {
		element, _ := reflect.TypeAssert[proto.Message](value.Index(i))
		text.WriteString("    " + strings.ReplaceAll(messageText(element), "\n", "\n    ") + ",\n")
	}
	text.WriteString("}")
	return text.String()
}

func messageText(message proto.Message) string {
	if message == nil || !message.ProtoReflect().IsValid() {
		return "nil"
	}
	text := strings.TrimSpace(prototext.MarshalOptions{Multiline: true}.Format(message))
	return reflect.TypeOf(message).String() + "{\n    " + strings.ReplaceAll(text, "\n", "\n    ") + "\n}"
}
