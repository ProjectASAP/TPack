package tpackexporter

import (
	"context"
	"fmt"
	"time"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	"github.com/ProjectASAP/TPack/pkg/tpackmodel/otlpconv"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

type tpackExporter struct {
	cfg             *Config
	logger          *zap.Logger
	gentConfig      tpackmodel.TPackConfig
	primaryAttributes  []string
	dependentAttributes []string
	buffer          *traceBuffer
	storage         Storage
	modelServer     *modelServer
	cancel          context.CancelFunc
}

func newTPackExporter(cfg *Config, logger *zap.Logger) *tpackExporter {
	gentConfig := tpackmodel.TPackConfig{
		MaxDepth:                  cfg.MaxDepth,
		MaxChildren:               cfg.MaxChildren,
		MaxGMMComponents:          3,
		MinSamplesForGMM:          2,
		StratifiedSampling:        cfg.StratifiedSampling,
		RejectSamplingMaxAttempts: 10,
		RejectSamplingEnabled:     true,
		UseDurationBounds:         true,
		OffsetValue:               "ratio",
		OffsetModel:               "regression",
		TopologyMode:              "edge",
		RandomSeed:                42,
	}

	featureCols := cfg.PrimaryAttributes
	if len(featureCols) == 0 {
		featureCols = tpackmodel.DefaultFeatureColumns
	}

	var store Storage
	if cfg.OutputPath != "" {
		store = newFilesystemStorage(cfg.OutputPath)
	}

	return &tpackExporter{
		cfg:             cfg,
		logger:          logger,
		gentConfig:      gentConfig,
		primaryAttributes:  featureCols,
		dependentAttributes: cfg.DependentAttributes,
		buffer:          newTraceBuffer(cfg.MaxBufferedTraces),
		storage:         store,
	}
}

func (e *tpackExporter) Start(ctx context.Context, host component.Host) error {
	e.logger.Info("Starting TPack exporter",
		zap.String("output_path", e.cfg.OutputPath),
	)

	// Start gRPC model server if configured
	if e.cfg.ModelServerPort > 0 {
		e.modelServer = newModelServer(e.logger)
		if err := e.modelServer.Start(e.cfg.ModelServerPort); err != nil {
			return fmt.Errorf("start model server: %w", err)
		}
	}

	flushCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel

	// Start periodic flush check
	go e.flushLoop(flushCtx)

	return nil
}

func (e *tpackExporter) Shutdown(ctx context.Context) error {
	e.logger.Info("Shutting down TPack exporter")
	if e.cancel != nil {
		e.cancel()
	}

	// Final flush
	e.doFlush()

	// Stop model server
	if e.modelServer != nil {
		e.modelServer.Stop()
	}
	return nil
}

// ConsumeTraces receives traces from the OTel pipeline and buffers them.
func (e *tpackExporter) ConsumeTraces(_ context.Context, td ptrace.Traces) error {
	traces := otlpconv.FromPdata(td, e.primaryAttributes, e.dependentAttributes)
	if len(traces) == 0 {
		return nil
	}

	isFull := e.buffer.add(traces)
	if isFull {
		e.logger.Info("Buffer full, triggering flush",
			zap.Int("buffer_size", e.buffer.size()))
		e.doFlush()
	}

	return nil
}

func (e *tpackExporter) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(e.cfg.FlushIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if e.buffer.size() > 0 {
				e.logger.Info("Periodic flush triggered",
					zap.Int("buffer_size", e.buffer.size()))
				e.doFlush()
			}
		}
	}
}

func (e *tpackExporter) doFlush() {
	traces := e.buffer.flush()
	if len(traces) == 0 {
		return
	}

	totalSpans := 0
	for _, t := range traces {
		totalSpans += len(t.Spans)
	}
	e.logger.Info("Flushing traces for training",
		zap.Int("trace_count", len(traces)),
		zap.Int("total_spans", totalSpans),
		zap.Float64("avg_spans_per_trace", float64(totalSpans)/float64(max(len(traces), 1))),
	)

	// Train models via the shared tpackmodel pipeline.
	state, err := tpackmodel.TrainBucket(traces, e.gentConfig, e.primaryAttributes, e.dependentAttributes)
	if err != nil {
		e.logger.Error("Training failed", zap.Error(err))
		return
	}

	// Serialize
	data, err := state.Marshal()
	if err != nil {
		e.logger.Error("Failed to serialize model", zap.Error(err))
		return
	}

	e.logger.Info("Model serialized", zap.Int("size_bytes", len(data)))

	// Store to filesystem (if configured)
	if e.storage != nil {
		if err := e.storage.Store(data); err != nil {
			e.logger.Error("Failed to store model", zap.Error(err))
		}
	}

	// Broadcast to connected receivers via gRPC
	if e.modelServer != nil {
		e.modelServer.Broadcast(data, len(traces))
	}

	e.logger.Info("Model stored successfully")
}

// Capabilities returns the consumer capabilities.
func (e *tpackExporter) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}
