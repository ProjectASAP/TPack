package tpackreceiver

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
)

const (
	typeStr   = "tpack"
	stability = component.StabilityLevelDevelopment
)

// NewFactory creates a new factory for the TPack receiver.
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		receiver.WithTraces(createTracesReceiver, stability),
	)
}

func createDefaultConfig() component.Config {
	return defaultConfig()
}

func createTracesReceiver(
	_ context.Context,
	params receiver.Settings,
	cfg component.Config,
	consumer consumer.Traces,
) (receiver.Traces, error) {
	rCfg := cfg.(*Config)
	return newTPackReceiver(rCfg, params.Logger, consumer), nil
}
