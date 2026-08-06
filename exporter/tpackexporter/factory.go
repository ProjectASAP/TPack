package tpackexporter

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/exporter"
)

const (
	typeStr   = "tpack"
	stability = component.StabilityLevelDevelopment
)

// NewFactory creates a new factory for the TPack exporter.
func NewFactory() exporter.Factory {
	return exporter.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		exporter.WithTraces(createTracesExporter, stability),
	)
}

func createDefaultConfig() component.Config {
	return defaultConfig()
}

func createTracesExporter(
	_ context.Context,
	params exporter.Settings,
	cfg component.Config,
) (exporter.Traces, error) {
	eCfg := cfg.(*Config)
	return newTPackExporter(eCfg, params.Logger), nil
}
