// Command native-descriptors exports the pinned YDB dependencies for protoc.
package main

import (
	"fmt"
	"os"
	"strings"

	_ "github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	_ "github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: native-descriptors OUTPUT")
	}
	set := &descriptorpb.FileDescriptorSet{}
	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		definition := protodesc.ToFileDescriptorProto(file)
		if strings.HasPrefix(file.Path(), "protos/") {
			if definition.Options == nil {
				definition.Options = &descriptorpb.FileOptions{}
			}
			definition.Options.GoPackage = proto.String("github.com/ydb-platform/ydb-go-genproto/protos/" + strings.ReplaceAll(string(file.Package()), ".", "_"))
		}
		set.File = append(set.File, definition)
		return true
	})
	data, err := proto.Marshal(set)
	if err != nil {
		return err
	}
	return os.WriteFile(os.Args[1], data, 0o600)
}
