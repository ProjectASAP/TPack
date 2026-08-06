package tpackexporter

import "fmt"

// Config defines configuration for the TPack exporter.
type Config struct {
	// Buffer management
	FlushIntervalSeconds int `mapstructure:"flush_interval_seconds"`
	MaxBufferedTraces    int `mapstructure:"max_buffered_traces"`

	// Storage configuration
	StorageType string `mapstructure:"storage_type"` // "filesystem"
	OutputPath  string `mapstructure:"output_path"`

	PrimaryAttributes   []string `mapstructure:"primary_attributes"`
	DependentAttributes []string `mapstructure:"dependent_attributes"`

	// gRPC model server port (0 = disabled)
	ModelServerPort int `mapstructure:"model_server_port"`

	// Model parameters
	MaxDepth           int32 `mapstructure:"max_depth"`
	MaxChildren        int32 `mapstructure:"max_children"`
	StratifiedSampling bool  `mapstructure:"stratified_sampling"`
}

func defaultConfig() *Config {
	return &Config{
		FlushIntervalSeconds: 120,
		MaxBufferedTraces:    100_000,
		StorageType:          "filesystem",
		OutputPath:           "",
		MaxDepth:             100,
		MaxChildren:          5000,
		StratifiedSampling:   true,
	}
}

// Validate checks the configuration for invalid values that would cause
// runtime panics or silent misbehavior. Called automatically by the
// OTel Collector framework before Start().
func (c *Config) Validate() error {
	if c.FlushIntervalSeconds <= 0 {
		return fmt.Errorf("flush_interval_seconds must be > 0 (got %d)", c.FlushIntervalSeconds)
	}
	if c.MaxBufferedTraces <= 0 {
		return fmt.Errorf("max_buffered_traces must be > 0 (got %d)", c.MaxBufferedTraces)
	}
	if c.ModelServerPort < 0 || c.ModelServerPort > 65535 {
		return fmt.Errorf("model_server_port must be in [0, 65535] (got %d)", c.ModelServerPort)
	}
	if c.ModelServerPort == 0 && c.OutputPath == "" {
		return fmt.Errorf("at least one of model_server_port or output_path must be set; otherwise trained models are discarded")
	}
	if c.MaxDepth <= 0 {
		return fmt.Errorf("max_depth must be > 0 (got %d)", c.MaxDepth)
	}
	if c.MaxChildren <= 0 {
		return fmt.Errorf("max_children must be > 0 (got %d)", c.MaxChildren)
	}
	return nil
}
