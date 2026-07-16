package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"path/filepath"
	"testing"

	"github.com/julieta/minidock/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

type testControlStream struct {
	ctx      context.Context
	incoming []*AgentMessage
	sent     []*ControlMessage
}

func (s *testControlStream) Context() context.Context     { return s.ctx }
func (s *testControlStream) SetHeader(metadata.MD) error  { return nil }
func (s *testControlStream) SendHeader(metadata.MD) error { return nil }
func (s *testControlStream) SetTrailer(metadata.MD)       {}
func (s *testControlStream) SendMsg(any) error            { return nil }
func (s *testControlStream) RecvMsg(any) error            { return nil }
func (s *testControlStream) Send(message *ControlMessage) error {
	s.sent = append(s.sent, message)
	return nil
}
func (s *testControlStream) Recv() (*AgentMessage, error) {
	if len(s.incoming) == 0 {
		return nil, io.EOF
	}
	message := s.incoming[0]
	s.incoming = s.incoming[1:]
	return message, nil
}

var _ Control_ConnectServer = (*testControlStream)(nil)
var _ grpc.ServerStream = (*testControlStream)(nil)

func TestControlPlaneRegistersHelloAndHeartbeat(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("agent-certificate")}}}}})
	stream := &testControlStream{ctx: ctx, incoming: []*AgentMessage{
		{Kind: "hello", NodeID: "edge-01", Name: "Edge 01", Version: "v0.1.0", Capabilities: []string{"runtime-observation"}},
		{Kind: "heartbeat", NodeID: "edge-01"},
	}}
	if err := (ControlPlane{Store: database}).Connect(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.sent) != 1 || stream.sent[0].Kind != "accepted" {
		t.Fatalf("control response = %#v", stream.sent)
	}
	nodes, err := database.Nodes(context.Background())
	if err != nil || len(nodes) != 1 || nodes[0].ID != "edge-01" {
		t.Fatalf("persisted nodes = %#v, %v", nodes, err)
	}
}
