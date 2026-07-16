package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestCreateTunnelUsesRemoteConfigurationAndFetchesToken(t *testing.T) {
	var createBody map[string]any
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer api-secret" {
			t.Fatalf("missing API authorization")
		}
		switch r.URL.Path {
		case "/accounts/account/cfd_tunnel":
			if r.Method != http.MethodPost {
				t.Fatalf("create method = %s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatal(err)
			}
			return jsonResponse(`{"success":true,"result":{"id":"tunnel-id"}}`), nil
		case "/accounts/account/cfd_tunnel/tunnel-id/token":
			return jsonResponse(`{"success":true,"result":"tunnel-secret"}`), nil
		default:
			t.Fatalf("unexpected API path %s", r.URL.Path)
		}
		return nil, nil
	})}

	client := &CloudflareClient{httpClient: httpClient, baseURL: "https://api.example.test"}
	id, token, err := client.CreateOrGetTunnel(context.Background(), "api-secret", "account", "minidock-tunnel")
	if err != nil {
		t.Fatal(err)
	}
	if id != "tunnel-id" || token != "tunnel-secret" {
		t.Fatalf("CreateOrGetTunnel() = %q, %q", id, token)
	}
	if createBody["config_src"] != "cloudflare" {
		t.Fatalf("tunnel config source = %#v", createBody["config_src"])
	}
}

func TestConfigureTunnelIngressPublishesOnlyPublicDomainsAndEndsIn404(t *testing.T) {
	var payload struct {
		Config struct {
			Ingress []struct {
				Hostname string `json:"hostname"`
				Service  string `json:"service"`
			} `json:"ingress"`
		} `json:"config"`
	}
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPut || r.URL.Path != "/accounts/account/cfd_tunnel/tunnel-id/configurations" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return jsonResponse(`{"success":true,"result":{}}`), nil
	})}

	client := &CloudflareClient{httpClient: httpClient, baseURL: "https://api.example.test"}
	err := client.ConfigureTunnelIngress(context.Background(), "api-secret", "account", "tunnel-id", []string{
		"Demo.Example.com.", "demo.example.com", "demo.local", "localhost",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Config.Ingress) != 2 {
		t.Fatalf("ingress rules = %#v", payload.Config.Ingress)
	}
	if first := payload.Config.Ingress[0]; first.Hostname != "demo.example.com" || first.Service != "http://caddy:80" {
		t.Fatalf("public ingress = %#v", first)
	}
	if fallback := payload.Config.Ingress[1]; fallback.Hostname != "" || fallback.Service != "http_status:404" {
		t.Fatalf("fallback ingress = %#v", fallback)
	}
}
