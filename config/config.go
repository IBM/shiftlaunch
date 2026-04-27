package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// AgentDaemonConfig holds the global agent settings loaded via Viper
type AgentDaemonConfig struct {
	Network  NetworkConfig `mapstructure:"network"`
	Paths    PathConfig    `mapstructure:"paths"`
	Timeouts TimeoutConfig `mapstructure:"timeouts"`
}

type NetworkConfig struct {
	HTTPPort int `mapstructure:"http_port"`
}

type PathConfig struct {
	WorkspaceDir   string `mapstructure:"workspace_dir"`
	DNSmasqConfDir string `mapstructure:"dnsmasq_conf_dir"`
	HAProxyConfDir string `mapstructure:"haproxy_conf_dir"`
	HTTPDDocRoot   string `mapstructure:"httpd_doc_root"`
	TFTPRoot       string `mapstructure:"tftp_root"`
	InstallDevice  string `mapstructure:"install_device"`
}

type TimeoutConfig struct {
	HMCApiRetries      int `mapstructure:"hmc_api_retries"`
	DownloadTimeoutSec int `mapstructure:"download_timeout_sec"`
}

// Load initializes Viper, sets defaults, and silently loads agent.yaml if present
func Load() (*AgentDaemonConfig, error) {
	v := viper.New()

	// 1. Set Sensible Defaults
	v.SetDefault("network.http_port", 8080)
	v.SetDefault("paths.workspace_dir", "/opt/shiftlaunch/clusters")
	v.SetDefault("paths.dnsmasq_conf_dir", "/etc/dnsmasq.d")
	v.SetDefault("paths.haproxy_conf_dir", "/etc/haproxy/conf.d")
	v.SetDefault("paths.httpd_doc_root", "/var/www/html")
	v.SetDefault("paths.tftp_root", "/var/lib/tftpboot")
	v.SetDefault("paths.install_device", "/dev/sda")
	v.SetDefault("timeouts.hmc_api_retries", 3)
	v.SetDefault("timeouts.download_timeout_sec", 1800)

	// 2. Tell Viper what file to look for
	v.SetConfigName("agent") // Looks for agent.yaml
	v.SetConfigType("yaml")

	// 3. Search ONLY in the local working directory
	v.AddConfigPath(".")

	// 4. Enable Environment Variable overrides (e.g., SHIFTLAUNCH_NETWORK_HTTP_PORT=9090)
	v.SetEnvPrefix("SHIFTLAUNCH")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// 5. Attempt to read the config file silently
	if err := v.ReadInConfig(); err != nil {
		// Ignore the error ONLY if the file was simply not found
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("fatal error parsing agent.yaml: %w", err)
		}
		// Silently continue and use defaults if agent.yaml is missing
	}

	// 6. Unmarshal the merged config into our struct
	var config AgentDaemonConfig
	if err := v.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("unable to decode configuration into struct: %w", err)
	}

	// 7. Validate that critical fields are populated
	if config.Paths.WorkspaceDir == "" {
		config.Paths.WorkspaceDir = "/opt/shiftlaunch/clusters"
	}
	if config.Paths.TFTPRoot == "" {
		config.Paths.TFTPRoot = "/var/lib/tftpboot"
	}
	if config.Paths.InstallDevice == "" {
		config.Paths.InstallDevice = "/dev/sda"
	}
	if config.Paths.DNSmasqConfDir == "" {
		config.Paths.DNSmasqConfDir = "/etc/dnsmasq.d"
	}
	if config.Paths.HAProxyConfDir == "" {
		config.Paths.HAProxyConfDir = "/etc/haproxy/conf.d"
	}
	if config.Paths.HTTPDDocRoot == "" {
		config.Paths.HTTPDDocRoot = "/var/www/html"
	}
	if config.Network.HTTPPort == 0 {
		config.Network.HTTPPort = 8080
	}
	if config.Timeouts.HMCApiRetries == 0 {
		config.Timeouts.HMCApiRetries = 3
	}
	if config.Timeouts.DownloadTimeoutSec == 0 {
		config.Timeouts.DownloadTimeoutSec = 1800
	}

	return &config, nil
}