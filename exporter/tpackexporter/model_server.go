package tpackexporter

import (
	"fmt"
	"net"
	"sync"
	"time"

	pb "github.com/ProjectASAP/TPack/pkg/tpackmodel/proto"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// modelServer implements the gRPC ModelService for streaming trained models
// to connected TPack receivers.
type modelServer struct {
	pb.UnimplementedModelServiceServer

	logger      *zap.Logger
	grpcServer  *grpc.Server
	mu          sync.Mutex
	subscribers map[uint64]chan *pb.ModelUpdate
	nextID      uint64
}

func newModelServer(logger *zap.Logger) *modelServer {
	return &modelServer{
		logger:      logger,
		subscribers: make(map[uint64]chan *pb.ModelUpdate),
	}
}

// Start launches the gRPC server on the given port.
func (s *modelServer) Start(port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("model server listen: %w", err)
	}

	s.grpcServer = grpc.NewServer()
	pb.RegisterModelServiceServer(s.grpcServer, s)

	go func() {
		s.logger.Info("Model gRPC server listening", zap.Int("port", port))
		if err := s.grpcServer.Serve(lis); err != nil {
			s.logger.Error("Model gRPC server error", zap.Error(err))
		}
	}()

	return nil
}

// Stop gracefully stops the gRPC server and closes all subscriber channels.
func (s *modelServer) Stop() {
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
	s.mu.Lock()
	for id, ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, id)
	}
	s.mu.Unlock()
}

// Broadcast sends a model update to all connected subscribers.
func (s *modelServer) Broadcast(data []byte, traceCount int) {
	update := &pb.ModelUpdate{
		ModelData:     data,
		TrainedAtUnix: time.Now().Unix(),
		TraceCount:    int32(traceCount),
	}

	s.mu.Lock()
	n := len(s.subscribers)
	for id, ch := range s.subscribers {
		select {
		case ch <- update:
		default:
			s.logger.Warn("Subscriber channel full, dropping update", zap.Uint64("subscriber", id))
		}
	}
	s.mu.Unlock()

	s.logger.Info("Broadcasting model to subscribers", zap.Int("subscriber_count", n))
}

// StreamModels implements the gRPC server-streaming RPC.
func (s *modelServer) StreamModels(_ *pb.StreamModelsRequest, stream pb.ModelService_StreamModelsServer) error {
	ch := make(chan *pb.ModelUpdate, 4)

	s.mu.Lock()
	id := s.nextID
	s.nextID++
	s.subscribers[id] = ch
	s.mu.Unlock()

	s.logger.Info("New model subscriber connected", zap.Uint64("subscriber", id))

	defer func() {
		s.mu.Lock()
		delete(s.subscribers, id)
		s.mu.Unlock()
		s.logger.Info("Model subscriber disconnected", zap.Uint64("subscriber", id))
	}()

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case update, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(update); err != nil {
				return err
			}
		}
	}
}
