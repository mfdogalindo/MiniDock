// Package agent defines the versioned, deliberately narrow control stream
// shared by the control plane and minidock-agent. It uses gRPC with an explicit
// JSON codec to keep this first compatibility boundary reviewable without a
// generated-code toolchain. The service name and message fields are stable API.
package agent

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
)

const ServiceName = "minidock.agent.v1.Control"

type jsonCodec struct{}

func (jsonCodec) Name() string                           { return "minidock-json" }
func (jsonCodec) Marshal(value any) ([]byte, error)      { return json.Marshal(value) }
func (jsonCodec) Unmarshal(data []byte, value any) error { return json.Unmarshal(data, value) }

// RegisterCodec is safe to call multiple times. Both binaries must use it;
// there is no protobuf fallback that could accidentally widen the protocol.
func RegisterCodec()            { encoding.RegisterCodec(jsonCodec{}) }
func JSONCodec() encoding.Codec { return jsonCodec{} }

type AgentMessage struct {
	Kind         string   `json:"kind"`
	NodeID       string   `json:"node_id"`
	Name         string   `json:"name,omitempty"`
	Version      string   `json:"version,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type ControlMessage struct {
	Kind             string `json:"kind"`
	HeartbeatSeconds int    `json:"heartbeat_seconds,omitempty"`
	Detail           string `json:"detail,omitempty"`
}

type ControlServer interface {
	Connect(Control_ConnectServer) error
}

type Control_ConnectServer interface {
	Send(*ControlMessage) error
	Recv() (*AgentMessage, error)
	grpc.ServerStream
}

type controlConnectServer struct{ grpc.ServerStream }

func (s *controlConnectServer) Send(message *ControlMessage) error {
	return s.ServerStream.SendMsg(message)
}
func (s *controlConnectServer) Recv() (*AgentMessage, error) {
	message := new(AgentMessage)
	return message, s.ServerStream.RecvMsg(message)
}

func RegisterControlServer(server *grpc.Server, implementation ControlServer) {
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: ServiceName,
		HandlerType: (*ControlServer)(nil),
		Streams: []grpc.StreamDesc{{StreamName: "Connect", Handler: func(srv any, stream grpc.ServerStream) error {
			return srv.(ControlServer).Connect(&controlConnectServer{stream})
		}, ServerStreams: true, ClientStreams: true}},
	}, implementation)
}

type ControlClient interface {
	Connect(context.Context, ...grpc.CallOption) (Control_ConnectClient, error)
}
type Control_ConnectClient interface {
	Send(*AgentMessage) error
	Recv() (*ControlMessage, error)
	grpc.ClientStream
}
type controlClient struct{ client grpc.ClientConnInterface }
type controlConnectClient struct{ grpc.ClientStream }

func NewControlClient(client grpc.ClientConnInterface) ControlClient {
	return &controlClient{client: client}
}
func (c *controlClient) Connect(ctx context.Context, options ...grpc.CallOption) (Control_ConnectClient, error) {
	stream, err := c.client.NewStream(ctx, &grpc.ServiceDesc{Streams: []grpc.StreamDesc{{StreamName: "Connect", ServerStreams: true, ClientStreams: true}}}.Streams[0], "/"+ServiceName+"/Connect", options...)
	if err != nil {
		return nil, err
	}
	return &controlConnectClient{stream}, nil
}
func (c *controlConnectClient) Send(message *AgentMessage) error {
	return c.ClientStream.SendMsg(message)
}
func (c *controlConnectClient) Recv() (*ControlMessage, error) {
	message := new(ControlMessage)
	return message, c.ClientStream.RecvMsg(message)
}
