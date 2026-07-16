package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/julieta/minidock/internal/store"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

var nodeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type ControlPlane struct {
	Store             *store.Store
	HeartbeatInterval time.Duration
}

func (p ControlPlane) Connect(stream Control_ConnectServer) error {
	if p.Store == nil {
		return errors.New("control plane store is required")
	}
	identity, err := certificateFingerprint(stream.Context())
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receive agent hello: %w", err)
	}
	if first.Kind != "hello" || !nodeIDPattern.MatchString(first.NodeID) || first.Name == "" || first.Version == "" {
		return errors.New("first agent message must be a valid hello")
	}
	if err := p.upsert(stream.Context(), first, identity); err != nil {
		return err
	}
	interval := p.HeartbeatInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if err := stream.Send(&ControlMessage{Kind: "accepted", HeartbeatSeconds: int(interval.Seconds())}); err != nil {
		return err
	}
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receive agent message: %w", err)
		}
		if message.Kind != "heartbeat" || message.NodeID != first.NodeID {
			return errors.New("invalid agent heartbeat")
		}
		message.Name, message.Version, message.Capabilities = first.Name, first.Version, first.Capabilities
		if err := p.upsert(stream.Context(), message, identity); err != nil {
			return err
		}
	}
}

func (p ControlPlane) upsert(ctx context.Context, message *AgentMessage, fingerprint string) error {
	return p.Store.UpsertNode(ctx, store.Node{ID: message.NodeID, Name: message.Name, Version: message.Version, Capabilities: message.Capabilities, CertificateFingerprint: fingerprint})
}

func certificateFingerprint(ctx context.Context) (string, error) {
	info, ok := peer.FromContext(ctx)
	if !ok {
		return "", errors.New("agent peer information is missing")
	}
	tlsInfo, ok := info.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return "", errors.New("agent mTLS certificate is required")
	}
	sum := sha256.Sum256(tlsInfo.State.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}
