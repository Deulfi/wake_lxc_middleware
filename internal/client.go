package internal

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Log levels
const (
	LogLevelInfo  = "info"
	LogLevelDebug = "debug"
)

// ProxmoxClient represents a client to the Proxmox API
type ProxmoxClient struct {
	BaseURL     string
	TokenID     string
	Token       string
	HTTPClient  *http.Client
	LogLevel    string
	ValidateSSL bool
}

// NewProxmoxClient creates a new Proxmox API client
func NewProxmoxClient(apiEndpoint, tokenID, token string, validateSSL bool, logLevel string) *ProxmoxClient {
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: !validateSSL,
			},
		},
		Timeout: 30 * time.Second,
	}

	baseURL := fmt.Sprintf("%s/api2/json", apiEndpoint)
	if logLevel == LogLevelDebug {
		log.Printf("Creating new Proxmox client with base URL: %s", baseURL)
	}

	return &ProxmoxClient{
		BaseURL:     baseURL,
		TokenID:     tokenID,
		Token:       token,
		HTTPClient:  httpClient,
		LogLevel:    logLevel,
		ValidateSSL: validateSSL,
	}
}

// Do performs an HTTP request to the Proxmox API
func (c *ProxmoxClient) Do(ctx context.Context, method, path string, body interface{}, result interface{}) error {
	fullURL := c.BaseURL + path

	if c.LogLevel == LogLevelDebug {
		log.Printf("API Request: %s %s", method, fullURL)
	}

	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set required headers
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", c.TokenID, c.Token))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response body: %w", err)
		}

		if c.LogLevel == LogLevelDebug {
			log.Printf("API Response: %s", string(respBody))
		}

		err = json.Unmarshal(respBody, result)
		if err != nil {
			return fmt.Errorf("failed to unmarshal response: %w", err)
		}
	}

	return nil
}

// Get performs a GET request to the Proxmox API
func (c *ProxmoxClient) Get(ctx context.Context, path string, result interface{}) error {
	return c.Do(ctx, http.MethodGet, path, nil, result)
}

// GetVersion retrieves the Proxmox version
func (c *ProxmoxClient) GetVersion(ctx context.Context) (*Version, error) {
	var response struct {
		Data Version `json:"data"`
	}
	err := c.Get(ctx, "/version", &response)
	if err != nil {
		return nil, err
	}
	return &response.Data, nil
}

// ListLXC retrieves all LXC containers on a node
func (c *ProxmoxClient) ListLXC(ctx context.Context, nodeName string) ([]VMRef, error) {
	var response struct {
		Data []VMRef `json:"data"`
	}
	err := c.Get(ctx, fmt.Sprintf("/nodes/%s/lxc", nodeName), &response)
	if err != nil {
		return nil, err
	}
	return response.Data, nil
}

// ListQEMU retrieves all QEMU VMs on a node
func (c *ProxmoxClient) ListQEMU(ctx context.Context, nodeName string) ([]VMRef, error) {
	var response struct {
		Data []VMRef `json:"data"`
	}
	err := c.Get(ctx, fmt.Sprintf("/nodes/%s/qemu", nodeName), &response)
	if err != nil {
		return nil, err
	}
	return response.Data, nil
}

// GetVMConfig retrieves the configuration of a VM or container
func (c *ProxmoxClient) GetVMConfig(ctx context.Context, nodeName, kind string, vmID uint64) (*VMConfig, error) {
	var response struct {
		Data VMConfig `json:"data"`
	}
	err := c.Get(ctx, fmt.Sprintf("/nodes/%s/%s/%d/config", nodeName, kind, vmID), &response)
	if err != nil {
		return nil, err
	}
	return &response.Data, nil
}

// GetStatus retrieves the current status of a VM or container
func (c *ProxmoxClient) GetStatus(ctx context.Context, nodeName, kind string, vmID uint64) (string, error) {
	var response struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	err := c.Get(ctx, fmt.Sprintf("/nodes/%s/%s/%d/status/current", nodeName, kind, vmID), &response)
	if err != nil {
		return "", err
	}
	return response.Data.Status, nil
}

// StartVM sends a start command to a VM or container
func (c *ProxmoxClient) StartVM(ctx context.Context, nodeName, kind string, vmID uint64) error {
	return c.Do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/%s/%d/status/start", nodeName, kind, vmID), map[string]interface{}{}, nil)
}

// StopVM sends a stop/shutdown command to a VM or container
func (c *ProxmoxClient) StopVM(ctx context.Context, nodeName, kind string, vmID uint64, mode string) error {
	return c.Do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/%s/%d/status/%s", nodeName, kind, vmID, mode), map[string]interface{}{}, nil)
}

// GetIPAMs retrieves all configured IPAM plugins in the cluster
func (c *ProxmoxClient) GetIPAMs(ctx context.Context) ([]IPAM, error) {
	var response struct {
		Data []IPAM `json:"data"`
	}
	err := c.Get(ctx, "/cluster/sdn/ipams", &response)
	if err != nil {
		return nil, err
	}
	return response.Data, nil
}

// GetIPAMStatus retrieves the IP records held by an IPAM plugin
func (c *ProxmoxClient) GetIPAMStatus(ctx context.Context, ipamID string) (*IPAMStatus, error) {
	var response struct {
		Data []IPAMRecord `json:"data"`
	}
	err := c.Get(ctx, fmt.Sprintf("/cluster/sdn/ipams/%s/status", ipamID), &response)
	if err != nil {
		return nil, err
	}
	return &IPAMStatus{Data: response.Data}, nil
}
