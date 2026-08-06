package tpackreceiver

import "fmt"

// Config defines configuration for the TPack receiver.
type Config struct {
	// Source type: "filesystem" (read model from file) or "grpc" (stream from exporter)
	SourceType string `mapstructure:"source_type"`

	// Filesystem source
	InputPath string `mapstructure:"input_path"` // Path to compressed model files

	// gRPC source
	ModelServerEndpoint string `mapstructure:"model_server_endpoint"` // e.g. "otelcol-compress:9090"

	// Generation parameters
	GenerationBatchSize int `mapstructure:"generation_batch_size"`

	// Continuous generation mode
	ContinuousGeneration bool `mapstructure:"continuous_generation"`
}

func defaultConfig() *Config {
	return &Config{
		SourceType:           "filesystem",
		InputPath:            "",
		ModelServerEndpoint:  "",
		GenerationBatchSize:  100,
		ContinuousGeneration: false,
	}
}

// Validate checks the configuration. Called automatically by the
// OTel Collector framework before Start().
func (c *Config) Validate() error {
	switch c.SourceType {
	case "filesystem":
		if c.InputPath == "" {
			return fmt.Errorf("input_path is required when source_type is \"filesystem\"")
		}
	case "grpc":
		if c.ModelServerEndpoint == "" {
			return fmt.Errorf("model_server_endpoint is required when source_type is \"grpc\"")
		}
	default:
		return fmt.Errorf("source_type must be \"filesystem\" or \"grpc\" (got %q)", c.SourceType)
	}
	return nil
}
