// Command replycut is a test harness: it serves a Ptah provider by passing
// every call to the provider executable REPLYCUT_PROVIDER names, and loses the
// reply of one call the way a network that drops it after the provider
// answered does. It loses the reply to the COMMIT of the transaction in which
// a statement holding the text REPLYCUT_MARK ran, after the provider committed
// it, and with REPLYCUT_READS=n the first n history reads after that, so the
// host cannot read back whether the commit landed, as the host's fixture
// provider loses them (FIXTURE_UNITS_LOSE_COMMIT, FIXTURE_UNITS_LOSE_READS).
// REPLYCUT_LOG names a file it appends one line to for each reply it loses. The integration tests start it
// in place of the provider to place a lost reply after a commit, which no
// call the host makes can ask for (stokaro/ptah-ydb-plugin#206). No release
// ships it.
//
// It speaks the provider bootstrap to both sides: the host launches it as a
// provider, and it launches the provider, pinned by REPLYCUT_PROVIDER_SHA256,
// as a host does. Every call is forwarded as it is, metadata included, so the
// host negotiates with the provider itself.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// The bootstrap the SDK's runtime speaks (provider-sdk/internal/bootstrap),
// which is internal to the SDK and is restated here.
const (
	serviceName       = "ptah-provider"
	parentEnvironment = "PTAH_PROVIDER_PARENT_PID"
)

var handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "PTAH_PROVIDER_BOOTSTRAP",
	MagicCookieValue: "ptah-provider-experimental-1",
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "replycut:", err)
		os.Exit(1)
	}
}

func run() error {
	checksum, err := hex.DecodeString(os.Getenv("REPLYCUT_PROVIDER_SHA256"))
	if err != nil || len(checksum) != sha256.Size {
		return errors.New("REPLYCUT_PROVIDER_SHA256 names no SHA-256 digest")
	}
	mark := os.Getenv("REPLYCUT_MARK")
	if mark == "" {
		return errors.New("REPLYCUT_MARK names no statement text")
	}
	command := exec.Command(os.Getenv("REPLYCUT_PROVIDER")) // #nosec G204 G702 -- the test names the provider executable, which the launch pins by its checksum
	command.Env = []string{parentEnvironment + "=" + strconv.Itoa(os.Getpid())}
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig: handshake, Plugins: map[string]plugin.Plugin{serviceName: &binding{}},
		Cmd: command, AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		SecureConfig: &plugin.SecureConfig{Checksum: checksum, Hash: sha256.New()},
		AutoMTLS:     true, SkipHostEnv: true,
		Logger: hclog.NewNullLogger(), SyncStdout: io.Discard, SyncStderr: io.Discard,
	})
	defer client.Kill()
	protocolClient, err := client.Client()
	if err != nil {
		return fmt.Errorf("start the provider: %w", err)
	}
	dispensed, err := protocolClient.Dispense(serviceName)
	if err != nil {
		return fmt.Errorf("dispense the provider service: %w", err)
	}
	conn, ok := dispensed.(*grpc.ClientConn)
	if !ok {
		return errors.New("the provider service is no gRPC connection")
	}
	reads := 0
	if raw := os.Getenv("REPLYCUT_READS"); raw != "" {
		if reads, err = strconv.Atoi(raw); err != nil || reads < 0 {
			return errors.New("REPLYCUT_READS names no count of history reads")
		}
	}
	cut := &cutter{mark: mark, reads: reads, log: os.Getenv("REPLYCUT_LOG"), marked: make(map[string]bool)}
	desc, err := forwarding(conn, cut)
	if err != nil {
		return err
	}
	plugin.Serve(&plugin.ServeConfig{HandshakeConfig: handshake, Plugins: map[string]plugin.Plugin{serviceName: &binding{desc: desc}},
		GRPCServer: plugin.DefaultGRPCServer, Logger: hclog.NewNullLogger()})
	return nil
}

// binding is the go-plugin binding of the provider service on both sides: the
// host side dials and returns the raw connection, and the provider side
// registers the forwarding service.
type binding struct {
	plugin.NetRPCUnsupportedPlugin
	desc *grpc.ServiceDesc
}

// GRPCServer registers the forwarding service.
func (b *binding) GRPCServer(_ *plugin.GRPCBroker, server *grpc.Server) error {
	server.RegisterService(b.desc, struct{}{})
	return nil
}

// GRPCClient returns the connection to the provider.
func (*binding) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	return conn, nil
}

// forwarding is the provider service whose every method passes the request
// to conn and the answer back, through cut.
func forwarding(conn *grpc.ClientConn, cut *cutter) (*grpc.ServiceDesc, error) {
	found, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(protocol.Provider_ServiceDesc.ServiceName))
	if err != nil {
		return nil, err
	}
	service, ok := found.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, errors.New("the provider service has no descriptor")
	}
	desc := &grpc.ServiceDesc{ServiceName: protocol.Provider_ServiceDesc.ServiceName, HandlerType: (*any)(nil),
		Metadata: protocol.Provider_ServiceDesc.Metadata}
	for _, method := range protocol.Provider_ServiceDesc.Methods {
		described := service.Methods().ByName(protoreflect.Name(method.MethodName))
		if described == nil {
			return nil, fmt.Errorf("method %s has no descriptor", method.MethodName)
		}
		input, err := protoregistry.GlobalTypes.FindMessageByName(described.Input().FullName())
		if err != nil {
			return nil, err
		}
		output, err := protoregistry.GlobalTypes.FindMessageByName(described.Output().FullName())
		if err != nil {
			return nil, err
		}
		full := "/" + desc.ServiceName + "/" + method.MethodName
		desc.Methods = append(desc.Methods, grpc.MethodDesc{MethodName: method.MethodName,
			Handler: func(_ any, ctx context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				request := input.New().Interface()
				if err := decode(request); err != nil {
					return nil, err
				}
				if err := cut.before(request); err != nil {
					return nil, err
				}
				if incoming, ok := metadata.FromIncomingContext(ctx); ok {
					ctx = metadata.NewOutgoingContext(ctx, incoming)
				}
				reply := output.New().Interface()
				if err := conn.Invoke(ctx, full, request, reply); err != nil {
					return nil, err
				}
				return reply, cut.after(request)
			}})
	}
	return desc, nil
}

// cutter decides which reply is lost: the reply to the COMMIT of a
// transaction a statement holding mark ran in, the first time, and the first
// reads history reads after that one.
type cutter struct {
	mark  string
	reads int
	// log is the file each lost reply is recorded in, or "".
	log string

	mu     sync.Mutex
	marked map[string]bool
	cut    bool
}

// errLost is what the host receives in place of a reply the network lost.
var errLost = status.Error(codes.Unavailable, "replycut: the connection closed before the answer arrived")

// before refuses the first reads history reads once a reply was lost, and
// records the transactions a marked statement runs in.
func (c *cutter) before(request proto.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch request := request.(type) {
	case *protocol.ReadHistoryRequest:
		if c.cut && c.reads > 0 {
			c.reads--
			c.record("lost the reply to a history read")
			return errLost
		}
	case *protocol.ExecuteRequest:
		for _, unit := range request.GetUnits() {
			if request.GetTransactionId() != "" && strings.Contains(unit.GetSql(), c.mark) {
				c.marked[request.GetTransactionId()] = true
			}
		}
	}
	return nil
}

// after loses the reply to the first COMMIT of a marked transaction, which
// the provider has carried out.
func (c *cutter) after(request proto.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	finish, ok := request.(*protocol.FinishTransactionRequest)
	if !ok || c.cut || finish.GetAction() != protocol.FinishTransactionRequest_COMMIT || !c.marked[finish.GetTransactionId()] {
		return nil
	}
	c.cut = true
	c.record("lost the reply to the COMMIT of a transaction that ran " + c.mark)
	return errLost
}

// record appends line to the log, when one is named. A record that cannot be
// written is dropped: the harness loses the reply either way.
func (c *cutter) record(line string) {
	if c.log == "" {
		return
	}
	file, err := os.OpenFile(c.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- the test names the log file in its temporary directory
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(file, line)
	_ = file.Close()
}
