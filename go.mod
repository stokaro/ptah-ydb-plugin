module github.com/stokaro/ptah-ydb-plugin

go 1.26.5

toolchain go1.27.3

require (
	github.com/frankban/quicktest v1.14.6
	github.com/google/go-cmp v0.7.0
	github.com/stokaro/ptah/provider-sdk v0.5.0
	github.com/ydb-platform/ydb-go-genproto v0.0.0-20260810123728-f0c151ab31b9
	github.com/ydb-platform/ydb-go-sdk-auth-environ v0.5.3
	github.com/ydb-platform/ydb-go-sdk/v3 v3.155.1
	golang.org/x/sys v0.49.0
	google.golang.org/grpc v1.86.0-dev
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/fatih/color v1.19.0 // indirect
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/go-hclog v1.6.3 // indirect
	github.com/hashicorp/go-plugin v1.8.0 // indirect
	github.com/hashicorp/yamux v0.1.2 // indirect
	github.com/jonboulle/clockwork v0.5.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-colorable v0.1.16 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/oklog/run v1.2.0 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/yandex-cloud/go-genproto v0.0.0-20240819112322-98a264d392f6 // indirect
	github.com/ydb-platform/ydb-go-yc v0.12.5 // indirect
	github.com/ydb-platform/ydb-go-yc-metadata v0.6.2 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261019180522-ce908d29299b // indirect
)

tool google.golang.org/protobuf/cmd/protoc-gen-go
