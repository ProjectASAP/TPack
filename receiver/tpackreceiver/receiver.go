package tpackreceiver

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/ProjectASAP/TPack/pkg/tpackmodel"
	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

type tpackReceiver struct {
	cfg      *Config
	logger   *zap.Logger
	cancel   context.CancelFunc
	consumer consumer.Traces

	mu    sync.RWMutex
	state *tpackmodel.TPackModelState

	// modelQueue buffers incoming model states from gRPC so that the
	// generation loop processes every model instead of only the latest.
	modelQueue chan *tpackmodel.TPackModelState
}

func newTPackReceiver(cfg *Config, logger *zap.Logger, consumer consumer.Traces) *tpackReceiver {
	return &tpackReceiver{
		cfg:      cfg,
		logger:   logger,
		consumer: consumer,
	}
}

func (r *tpackReceiver) Start(ctx context.Context, host component.Host) error {
	genCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	switch r.cfg.SourceType {
	case "grpc":
		r.logger.Info("Starting TPack receiver in gRPC mode",
			zap.String("endpoint", r.cfg.ModelServerEndpoint))
		r.modelQueue = make(chan *tpackmodel.TPackModelState, 64)
		go r.grpcSubscribeLoop(genCtx)
		go r.generateLoop(genCtx)
	default: // "filesystem"
		r.logger.Info("Starting TPack receiver in filesystem mode",
			zap.String("input_path", r.cfg.InputPath))
		if err := r.loadModelsFromFile(); err != nil {
			cancel()
			return fmt.Errorf("failed to load models: %w", err)
		}
		go r.generateLoop(genCtx)
	}

	return nil
}

func (r *tpackReceiver) Shutdown(ctx context.Context) error {
	r.logger.Info("Shutting down TPack receiver")
	if r.cancel != nil {
		r.cancel()
	}
	return nil
}

// loadModelsFromFile loads TPack models from a protobuf file on disk.
func (r *tpackReceiver) loadModelsFromFile() error {
	if r.cfg.InputPath == "" {
		return fmt.Errorf("input_path is required")
	}

	data, err := os.ReadFile(r.cfg.InputPath)
	if err != nil {
		return fmt.Errorf("read model file: %w", err)
	}

	return r.loadModelsFromData(data)
}

// loadModelsFromData deserializes model data and swaps the active state.
func (r *tpackReceiver) loadModelsFromData(data []byte) error {
	state := tpackmodel.NewTPackModelState(tpackmodel.DefaultConfig())

	models := &pb.TPackModels{}
	if err := proto.Unmarshal(data, models); err != nil {
		return fmt.Errorf("unmarshal protobuf: %w", err)
	}
	if err := state.LoadFromProto(models); err != nil {
		return fmt.Errorf("load model state: %w", err)
	}

	r.mu.Lock()
	r.state = state
	r.mu.Unlock()

	// Enqueue for generation loop (gRPC mode only).
	if r.modelQueue != nil {
		select {
		case r.modelQueue <- state:
		default:
			r.logger.Warn("Model queue full, dropping oldest model")
			<-r.modelQueue
			r.modelQueue <- state
		}
	}

	r.logger.Info("Loaded TPack models",
		zap.Int32("vocab_size", state.NodeEncoder.VocabSize()),
	)
	return nil
}

// getState returns the current model state (thread-safe).
func (r *tpackReceiver) getState() *tpackmodel.TPackModelState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state
}

// grpcSubscribeLoop connects to the exporter's model server and receives
// model updates. It automatically reconnects with exponential backoff.
func (r *tpackReceiver) grpcSubscribeLoop(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := r.grpcSubscribeOnce(ctx)
		if ctx.Err() != nil {
			return
		}

		r.logger.Warn("Model stream disconnected, reconnecting",
			zap.Error(err), zap.Duration("backoff", backoff))

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, maxBackoff)
	}
}

func (r *tpackReceiver) grpcSubscribeOnce(ctx context.Context) error {
	conn, err := grpc.NewClient(r.cfg.ModelServerEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial model server: %w", err)
	}
	defer conn.Close()

	client := pb.NewModelServiceClient(conn)
	stream, err := client.StreamModels(ctx, &pb.StreamModelsRequest{})
	if err != nil {
		return fmt.Errorf("subscribe to models: %w", err)
	}

	r.logger.Info("Connected to model server", zap.String("endpoint", r.cfg.ModelServerEndpoint))

	for {
		update, err := stream.Recv()
		if err == io.EOF {
			return fmt.Errorf("server closed stream")
		}
		if err != nil {
			return fmt.Errorf("recv model update: %w", err)
		}

		r.logger.Info("Received model from server",
			zap.Int("size_bytes", len(update.ModelData)),
			zap.Int32("trace_count", update.TraceCount),
		)

		if err := r.loadModelsFromData(update.ModelData); err != nil {
			r.logger.Error("Failed to load model from server", zap.Error(err))
			continue
		}
	}
}

func (r *tpackReceiver) generateLoop(ctx context.Context) {
	if r.modelQueue != nil {
		r.generateLoopQueued(ctx)
	} else {
		r.generateLoopPolled(ctx)
	}
}

// generateLoopQueued consumes models from the queue (gRPC mode).
// Each model is generated and emitted immediately with original timestamps.
func (r *tpackReceiver) generateLoopQueued(ctx context.Context) {
	r.logger.Info("Waiting for first model from gRPC stream",
		zap.String("endpoint", r.cfg.ModelServerEndpoint))
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case state := <-r.modelQueue:
			if first {
				r.logger.Info("First model received; starting generation")
				first = false
			}
			r.generateAndEmit(ctx, state)
		}
	}
}

// generateLoopPolled polls r.state for models (filesystem mode).
func (r *tpackReceiver) generateLoopPolled(ctx context.Context) {
	// Wait for first model to be available
	for {
		if state := r.getState(); state != nil {
			break
		}
		r.logger.Info("Waiting for model...")
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		state := r.getState()
		if state == nil {
			time.Sleep(time.Second)
			continue
		}

		r.generateAndEmit(ctx, state)

		if !r.cfg.ContinuousGeneration {
			r.logger.Info("Single generation complete, stopping")
			return
		}
	}
}

// generateAndEmit generates traces from a model state and pushes them
// to the consumer. Traces carry the original timestamps from the training
// data, so the trace backend places them at the correct time.
func (r *tpackReceiver) generateAndEmit(ctx context.Context, state *tpackmodel.TPackModelState) {
	allTraces, totalSpans := r.generateTraces(state)
	if len(allTraces) == 0 {
		return
	}
	r.logger.Info("Generated traces",
		zap.Int("count", len(allTraces)),
		zap.Int("total_spans", totalSpans),
	)

	// Send in chunks to stay under gRPC's 4MB message limit.
	const chunkSize = 500
	chunk := make(map[string][]tpackmodel.GeneratedSpan, chunkSize)
	i := 0
	for traceID, spans := range allTraces {
		chunk[traceID] = spans
		i++
		if i%chunkSize == 0 {
			batch := convertAllTracesToBatch(chunk, state.NodeEncoder)
			if err := r.consumer.ConsumeTraces(ctx, batch); err != nil {
				r.logger.Error("Failed to push traces chunk", zap.Error(err))
			}
			chunk = make(map[string][]tpackmodel.GeneratedSpan, chunkSize)
		}
	}
	if len(chunk) > 0 {
		batch := convertAllTracesToBatch(chunk, state.NodeEncoder)
		if err := r.consumer.ConsumeTraces(ctx, batch); err != nil {
			r.logger.Error("Failed to push traces chunk", zap.Error(err))
		}
	}
}

// generateTraces creates traces from a model state and returns them with total span count.
func (r *tpackReceiver) generateTraces(state *tpackmodel.TPackModelState) (map[string][]tpackmodel.GeneratedSpan, int) {
	return generateAllTraces(state)
}
