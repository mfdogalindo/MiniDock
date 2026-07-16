package agent

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// ServerTLSConfig loads a strict mTLS configuration. WireGuard confines the
// network path; mTLS still supplies the application identity binding.
func ServerTLSConfig(certificatePath, keyPath, clientCAPath string) (*tls.Config, error) {
	if certificatePath == "" || keyPath == "" || clientCAPath == "" {
		return nil, fmt.Errorf("agent TLS certificate, key and client CA paths are required")
	}
	certificate, err := tls.LoadX509KeyPair(certificatePath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load agent server certificate: %w", err)
	}
	pem, err := os.ReadFile(clientCAPath)
	if err != nil {
		return nil, fmt.Errorf("read agent client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("agent client CA contains no certificate")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}, nil
}

// ClientTLSConfig loads the agent's mutually authenticated TLS identity.
func ClientTLSConfig(certificatePath, keyPath, serverCAPath, serverName string) (*tls.Config, error) {
	if certificatePath == "" || keyPath == "" || serverCAPath == "" || serverName == "" {
		return nil, fmt.Errorf("control TLS certificate, key, CA and server name are required")
	}
	certificate, err := tls.LoadX509KeyPair(certificatePath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load agent client certificate: %w", err)
	}
	pem, err := os.ReadFile(serverCAPath)
	if err != nil {
		return nil, fmt.Errorf("read control server CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("control server CA contains no certificate")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, RootCAs: pool, ServerName: serverName}, nil
}
