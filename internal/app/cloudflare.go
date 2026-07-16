package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

type CloudflareZone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type CloudflareTunnel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	ConfigSrc string `json:"config_src"`
	CreatedAt string `json:"created_at"`
}

type CloudflareClient struct {
	httpClient *http.Client
	baseURL    string
}

type CloudflareAPI interface {
	VerifyToken(context.Context, string) error
	ListZones(context.Context, string) ([]CloudflareZone, error)
	CreateOrGetTunnel(context.Context, string, string, string) (string, string, error)
	ConfigureTunnelIngress(context.Context, string, string, string, []string) error
	TunnelStatus(context.Context, string, string, string) (string, error)
	UpsertCNAMERecord(context.Context, string, string, string, string) error
}

func NewCloudflareClient() *CloudflareClient {
	return &CloudflareClient{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    "https://api.cloudflare.com/client/v4",
	}
}

func (c *CloudflareClient) VerifyToken(ctx context.Context, apiToken string) error {
	if strings.TrimSpace(apiToken) == "" {
		return fmt.Errorf("API token vacio")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/user/tokens/verify", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error de conexion con Cloudflare: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token invalido de Cloudflare (HTTP %d)", resp.StatusCode)
	}

	var result struct {
		Success bool `json:"success"`
		Result  struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("respuesta invalida de Cloudflare: %w", err)
	}
	if !result.Success || result.Result.Status != "active" {
		return fmt.Errorf("el token de Cloudflare no esta activo")
	}
	return nil
}

func (c *CloudflareClient) ListZones(ctx context.Context, apiToken string) ([]CloudflareZone, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/zones?status=active&per_page=50", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Success bool             `json:"success"`
		Result  []CloudflareZone `json:"result"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.Success {
		if len(result.Errors) > 0 {
			return nil, fmt.Errorf("cloudflare API: %s", result.Errors[0].Message)
		}
		return nil, fmt.Errorf("error al listar zonas de Cloudflare")
	}
	return result.Result, nil
}

func (c *CloudflareClient) CreateOrGetTunnel(ctx context.Context, apiToken, accountID, tunnelName string) (tunnelID, tunnelToken string, err error) {
	if accountID == "" {
		return "", "", fmt.Errorf("account_id es requerido para crear un tunel")
	}
	url := fmt.Sprintf("%s/accounts/%s/cfd_tunnel", c.baseURL, accountID)
	reqBody, _ := json.Marshal(map[string]string{
		"name":       tunnelName,
		"config_src": "cloudflare",
	})

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Success bool `json:"success"`
		Result  struct {
			ID string `json:"id"`
		} `json:"result"`
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", err
	}

	if result.Success {
		token, tokenErr := c.TunnelToken(ctx, apiToken, accountID, result.Result.ID)
		return result.Result.ID, token, tokenErr
	}

	// Si ya existe el túnel, obtenerlo
	listReq, err := http.NewRequestWithContext(ctx, "GET", url+"?name="+neturl.QueryEscape(tunnelName)+"&is_deleted=false", nil)
	if err != nil {
		return "", "", err
	}
	listReq.Header.Set("Authorization", "Bearer "+apiToken)
	listReq.Header.Set("Content-Type", "application/json")

	listResp, err := c.httpClient.Do(listReq)
	if err != nil {
		return "", "", err
	}
	defer listResp.Body.Close()

	var listResult struct {
		Success bool `json:"success"`
		Result  []struct {
			ID        string `json:"id"`
			ConfigSrc string `json:"config_src"`
		} `json:"result"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listResult); err == nil && len(listResult.Result) > 0 {
		tID := listResult.Result[0].ID
		if listResult.Result[0].ConfigSrc != "cloudflare" {
			return "", "", fmt.Errorf("ya existe un túnel llamado %s con configuración local; cambia su nombre o elimínalo desde Cloudflare", tunnelName)
		}
		token, tokenErr := c.TunnelToken(ctx, apiToken, accountID, tID)
		return tID, token, tokenErr
	}

	if len(result.Errors) > 0 {
		return "", "", fmt.Errorf("Cloudflare Tunnel: %s", result.Errors[0].Message)
	}
	return "", "", fmt.Errorf("error al crear el tunel Cloudflare")
}

func (c *CloudflareClient) TunnelToken(ctx context.Context, apiToken, accountID, tunnelID string) (string, error) {
	url := fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s/token", c.baseURL, accountID, tunnelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		Success bool   `json:"success"`
		Result  string `json:"result"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.Success || strings.TrimSpace(result.Result) == "" {
		if len(result.Errors) > 0 {
			return "", fmt.Errorf("Cloudflare Tunnel: %s", result.Errors[0].Message)
		}
		return "", fmt.Errorf("Cloudflare no entregó el token del túnel")
	}
	return result.Result, nil
}

func (c *CloudflareClient) ConfigureTunnelIngress(ctx context.Context, apiToken, accountID, tunnelID string, hostnames []string) error {
	type ingressRule struct {
		Hostname string `json:"hostname,omitempty"`
		Service  string `json:"service"`
	}
	seen := make(map[string]struct{}, len(hostnames))
	rules := make([]ingressRule, 0, len(hostnames)+1)
	for _, hostname := range hostnames {
		hostname = cloudflareHostname(hostname)
		if !isPublicCloudflareDomain(hostname) {
			continue
		}
		if _, exists := seen[hostname]; exists {
			continue
		}
		seen[hostname] = struct{}{}
		rules = append(rules, ingressRule{Hostname: hostname, Service: "http://caddy:80"})
	}
	rules = append(rules, ingressRule{Service: "http_status:404"})
	payload, err := json.Marshal(map[string]any{"config": map[string]any{"ingress": rules}})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s/configurations", c.baseURL, accountID, tunnelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.Success {
		if len(result.Errors) > 0 {
			return fmt.Errorf("configuración del túnel: %s", result.Errors[0].Message)
		}
		return fmt.Errorf("Cloudflare rechazó la configuración del túnel")
	}
	return nil
}

func (c *CloudflareClient) TunnelStatus(ctx context.Context, apiToken, accountID, tunnelID string) (string, error) {
	url := fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s", c.baseURL, accountID, tunnelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		Success bool             `json:"success"`
		Result  CloudflareTunnel `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.Success {
		return "", fmt.Errorf("Cloudflare no pudo consultar el estado del túnel")
	}
	return result.Result.Status, nil
}

func (c *CloudflareClient) UpsertCNAMERecord(ctx context.Context, apiToken, zoneID, hostname, targetCNAME string) error {
	url := fmt.Sprintf("%s/zones/%s/dns_records", c.baseURL, zoneID)
	// Verificar si ya existe el registro DNS
	listReq, err := http.NewRequestWithContext(ctx, "GET", url+"?name="+neturl.QueryEscape(hostname)+"&type=CNAME", nil)
	if err != nil {
		return err
	}
	listReq.Header.Set("Authorization", "Bearer "+apiToken)

	listResp, err := c.httpClient.Do(listReq)
	if err != nil {
		return err
	}
	defer listResp.Body.Close()

	var listResult struct {
		Success bool `json:"success"`
		Result  []struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listResult); err != nil {
		return err
	}

	payload := map[string]interface{}{
		"type":    "CNAME",
		"name":    hostname,
		"content": targetCNAME,
		"ttl":     1, // Automatic TTL
		"proxied": true,
	}
	reqBody, _ := json.Marshal(payload)

	var req *http.Request
	if len(listResult.Result) > 0 {
		// Actualizar
		recID := listResult.Result[0].ID
		req, err = http.NewRequestWithContext(ctx, "PUT", url+"/"+recID, bytes.NewReader(reqBody))
	} else {
		// Crear
		req, err = http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	}
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var res struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}
	if !res.Success {
		if len(res.Errors) > 0 {
			return fmt.Errorf("Cloudflare DNS: %s", res.Errors[0].Message)
		}
		return fmt.Errorf("error al guardar registro CNAME en Cloudflare")
	}
	return nil
}

func VerifyDNSPropagation(ctx context.Context, domain, expectedTarget string) (resolved bool, currentTarget string, err error) {
	cname, err := net.DefaultResolver.LookupCNAME(ctx, domain)
	if err != nil {
		return false, "", err
	}
	cname = strings.TrimSuffix(cname, ".")
	expectedTarget = strings.TrimSuffix(expectedTarget, ".")

	if strings.EqualFold(cname, expectedTarget) || strings.HasSuffix(cname, ".cfargotunnel.com") {
		return true, cname, nil
	}
	return false, cname, nil
}
