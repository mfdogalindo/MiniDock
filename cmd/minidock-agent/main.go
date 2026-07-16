// minidock-agent establishes an outbound mTLS gRPC control stream. It has no
// command execution capability in this initial release.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/julieta/minidock/internal/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	var controlAddress, serverName, nodeID, nodeName, version, certificate, privateKey, caPath, capabilities string
	flag.StringVar(&controlAddress, "control", os.Getenv("MINIDOCK_AGENT_CONTROL_ADDRESS"), "WireGuard address of the MiniDock control plane")
	flag.StringVar(&serverName, "server-name", os.Getenv("MINIDOCK_AGENT_CONTROL_SERVER_NAME"), "TLS DNS name of the control plane")
	flag.StringVar(&nodeID, "node-id", os.Getenv("MINIDOCK_AGENT_NODE_ID"), "stable node identifier")
	flag.StringVar(&nodeName, "node-name", os.Getenv("MINIDOCK_AGENT_NODE_NAME"), "operator-visible node name")
	flag.StringVar(&version, "version", "dev", "agent version")
	flag.StringVar(&certificate, "tls-certificate", os.Getenv("MINIDOCK_AGENT_TLS_CERTIFICATE_PATH"), "agent TLS certificate")
	flag.StringVar(&privateKey, "tls-private-key", os.Getenv("MINIDOCK_AGENT_TLS_PRIVATE_KEY_PATH"), "agent TLS private key")
	flag.StringVar(&caPath, "tls-ca", os.Getenv("MINIDOCK_AGENT_TLS_CA_PATH"), "control-plane CA")
	flag.StringVar(&capabilities, "capabilities", "runtime-observation", "comma-separated advertised capabilities")
	flag.Parse()
	if controlAddress == "" || serverName == "" || nodeID == "" || nodeName == "" {
		log.Fatal("control, server-name, node-id and node-name are required")
	}
	tlsConfig, err := agent.ClientTLSConfig(certificate, privateKey, caPath, serverName)
	if err != nil {
		log.Fatal(err)
	}
	agent.RegisterCodec()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connection, err := grpc.NewClient(controlAddress, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithDefaultCallOptions(grpc.ForceCodec(agent.JSONCodec())))
	if err != nil {
		log.Fatalf("connect control plane: %v", err)
	}
	defer connection.Close()
	stream, err := agent.NewControlClient(connection).Connect(ctx)
	if err != nil {
		log.Fatalf("open control stream: %v", err)
	}
	message := &agent.AgentMessage{Kind: "hello", NodeID: nodeID, Name: nodeName, Version: version, Capabilities: splitCapabilities(capabilities)}
	if err := stream.Send(message); err != nil {
		log.Fatalf("register node: %v", err)
	}
	accepted, err := stream.Recv()
	if err != nil || accepted.Kind != "accepted" {
		log.Fatalf("control plane rejected node: %v %s", err, accepted.Detail)
	}
	interval := time.Duration(accepted.HeartbeatSeconds) * time.Second
	if interval < 5*time.Second {
		interval = 30 * time.Second
	}
	log.Printf("registered node %s; heartbeat every %s", nodeID, interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := stream.Send(&agent.AgentMessage{Kind: "heartbeat", NodeID: nodeID}); err != nil {
				log.Fatalf("send heartbeat: %v", err)
			}
		}
	}
}

func splitCapabilities(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
