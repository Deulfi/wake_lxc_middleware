// Package wake_lxc_middleware is a Traefik middleware plugin that wakes
// idle Proxmox LXC containers and QEMU VMs on request, proxies the traffic
// to the backend, and stops the container after an idle window.
package wake_lxc_middleware

import (
	"context"
	"net/http"

	"github.com/Deulfi/wake_lxc_middleware/middleware"
)

// Config the plugin configuration.
type Config struct {
	ApiEndpoint       string `json:"apiEndpoint" yaml:"apiEndpoint" toml:"apiEndpoint"`
	ApiTokenId        string `json:"apiTokenId" yaml:"apiTokenId" toml:"apiTokenId"`
	ApiToken          string `json:"apiToken" yaml:"apiToken" toml:"apiToken"`
	ApiValidateSSL    string `json:"apiValidateSSL" yaml:"apiValidateSSL" toml:"apiValidateSSL"`
	ApiLogging        string `json:"apiLogging" yaml:"apiLogging" toml:"apiLogging"`
	NodeName          string `json:"nodeName" yaml:"nodeName" toml:"nodeName"`
	DiscoveryInterval string `json:"discoveryInterval" yaml:"discoveryInterval" toml:"discoveryInterval"`
	CheckInterval     string `json:"checkInterval" yaml:"checkInterval" toml:"checkInterval"`
	ShutdownMinutes   string `json:"shutdownMinutes" yaml:"shutdownMinutes" toml:"shutdownMinutes"`
	DomainTemplate    string `json:"domainTemplate" yaml:"domainTemplate" toml:"domainTemplate"`
	StateFile         string `json:"stateFile" yaml:"stateFile" toml:"stateFile"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	cfg := middleware.CreateConfig()
	return &Config{
		ApiEndpoint:       cfg.ApiEndpoint,
		ApiTokenId:        cfg.ApiTokenId,
		ApiToken:          cfg.ApiToken,
		ApiValidateSSL:    cfg.ApiValidateSSL,
		ApiLogging:        cfg.ApiLogging,
		NodeName:          cfg.NodeName,
		DiscoveryInterval: cfg.DiscoveryInterval,
		CheckInterval:     cfg.CheckInterval,
		ShutdownMinutes:   cfg.ShutdownMinutes,
		DomainTemplate:    cfg.DomainTemplate,
		StateFile:         cfg.StateFile,
	}
}

// Middleware a plugin.
type Middleware struct {
	inner *middleware.Middleware
}

// New creates a new Middleware plugin.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	cfg := &middleware.Config{
		ApiEndpoint:       config.ApiEndpoint,
		ApiTokenId:        config.ApiTokenId,
		ApiToken:          config.ApiToken,
		ApiValidateSSL:    config.ApiValidateSSL,
		ApiLogging:        config.ApiLogging,
		NodeName:          config.NodeName,
		DiscoveryInterval: config.DiscoveryInterval,
		CheckInterval:     config.CheckInterval,
		ShutdownMinutes:   config.ShutdownMinutes,
		DomainTemplate:    config.DomainTemplate,
		StateFile:         config.StateFile,
	}
	inner, err := middleware.New(ctx, next, cfg, name)
	if err != nil {
		return nil, err
	}
	return &Middleware{inner: inner}, nil
}

// ServeHTTP handles the request.
func (m *Middleware) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	m.inner.ServeHTTP(rw, req)
}
