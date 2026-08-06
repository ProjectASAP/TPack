// Package main implements a custom OpenTelemetry Collector that includes
// TPack trace compressor (exporter) and decompressor (receiver) components.
//
// This binary can be built manually or generated via the OpenTelemetry
// Collector Builder (OCB) using the adjacent builder-config.yaml.
//
// Example collector config:
//
//	service:
//	  pipelines:
//	    traces/compress:
//	      receivers: [otlp]
//	      exporters: [tpack]
//	    traces/decompress:
//	      receivers: [tpack]
//	      exporters: [otlp]
package main

import (
	"fmt"
	"os"

	"github.com/ProjectASAP/TPack/exporter/tpackexporter"
	"github.com/ProjectASAP/TPack/receiver/tpackreceiver"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/envprovider"
	"go.opentelemetry.io/collector/confmap/provider/fileprovider"
	"go.opentelemetry.io/collector/confmap/provider/yamlprovider"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
	"go.opentelemetry.io/collector/service/telemetry/otelconftelemetry"
)

func main() {
	info := component.BuildInfo{
		Command:     "otelcol-tpack",
		Description: "OpenTelemetry Collector with TPack trace compression",
		Version:     "0.1.0",
	}

	set := otelcol.CollectorSettings{
		BuildInfo: info,
		Factories: components,
		ConfigProviderSettings: otelcol.ConfigProviderSettings{
			ResolverSettings: confmap.ResolverSettings{
				ProviderFactories: []confmap.ProviderFactory{
					fileprovider.NewFactory(),
					envprovider.NewFactory(),
					yamlprovider.NewFactory(),
				},
				DefaultScheme: "env",
			},
		},
	}

	cmd := otelcol.NewCommand(set)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func components() (otelcol.Factories, error) {
	var err error
	factories := otelcol.Factories{}

	factories.Receivers, err = otelcol.MakeFactoryMap(
		tpackreceiver.NewFactory(),
		otlpreceiver.NewFactory(),
	)
	if err != nil {
		return otelcol.Factories{}, err
	}

	factories.Exporters, err = otelcol.MakeFactoryMap(
		tpackexporter.NewFactory(),
		otlpexporter.NewFactory(),
	)
	if err != nil {
		return otelcol.Factories{}, err
	}

	factories.Telemetry = otelconftelemetry.NewFactory()

	return factories, nil
}
