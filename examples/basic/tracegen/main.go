// tracegen generates random multi-service, multi-depth distributed traces.
package main

import (
	"context"
	"flag"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Service dependency graph.
var graph = []struct {
	name string
	ops  []string
	down []string
}{
	{"api-gateway", []string{"GET /users", "GET /products", "POST /orders", "GET /orders/{id}", "DELETE /orders/{id}"}, []string{"user-service", "product-service", "order-service"}},
	{"user-service", []string{"getUser", "listUsers", "updateUser", "deleteUser"}, []string{"auth-service", "cache-service", "db-service"}},
	{"product-service", []string{"getProduct", "searchProducts", "listCategories"}, []string{"cache-service", "db-service", "search-service"}},
	{"order-service", []string{"createOrder", "getOrder", "cancelOrder"}, []string{"payment-service", "inventory-service", "db-service"}},
	{"payment-service", []string{"processPayment", "refund", "getStatus"}, []string{"db-service"}},
	{"inventory-service", []string{"checkStock", "reserveStock", "releaseStock"}, []string{"db-service", "cache-service"}},
	{"auth-service", []string{"validateToken", "refreshToken"}, []string{"db-service", "cache-service"}},
	{"search-service", []string{"query", "index", "suggest"}, []string{"db-service"}},
	{"cache-service", []string{"GET", "SET", "DEL"}, nil},
	{"db-service", []string{"SELECT", "INSERT", "UPDATE"}, nil},
}

var roots = []string{"api-gateway"}

type svc struct {
	ops    []string
	down   []string
	tracer trace.Tracer
}

func main() {
	endpoint := flag.String("endpoint", "localhost:4317", "OTLP gRPC endpoint")
	rate := flag.Float64("rate", 5, "Traces per second")
	dur := flag.Duration("duration", time.Hour, "Run duration")
	maxDepth := flag.Int("max-depth", 4, "Max call depth")
	errRate := flag.Float64("error-rate", 0.05, "Error probability per span")
	seed := flag.Int64("seed", 0, "Random seed (0=time-based)")
	flag.Parse()

	if *seed == 0 {
		*seed = time.Now().UnixNano()
	}
	rng := rand.New(rand.NewSource(*seed))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(*endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		log.Fatalf("exporter: %v", err)
	}

	// One TracerProvider per service so each gets its own Resource (service.name).
	svcs := make(map[string]*svc)
	var tps []*sdktrace.TracerProvider
	for _, g := range graph {
		res, _ := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(g.name)))
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
		)
		tps = append(tps, tp)
		svcs[g.name] = &svc{ops: g.ops, down: g.down, tracer: tp.Tracer(g.name)}
	}

	gen := &generator{svcs: svcs, rng: rng, maxDepth: *maxDepth, errRate: *errRate}

	interval := time.Duration(float64(time.Second) / *rate)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	deadline := time.After(*dur)
	n := 0

	log.Printf("Sending traces at %.1f/s to %s (depth<=%d, err=%.0f%%)",
		*rate, *endpoint, *maxDepth, *errRate*100)

	for {
		select {
		case <-ctx.Done():
			goto done
		case <-deadline:
			goto done
		case <-tick.C:
			gen.generateTrace(ctx)
			n++
			if n%100 == 0 {
				log.Printf("sent %d traces", n)
			}
		}
	}
done:
	log.Printf("shutting down after %d traces", n)
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tp := range tps {
		tp.Shutdown(shutCtx)
	}
}

type generator struct {
	svcs     map[string]*svc
	rng      *rand.Rand
	maxDepth int
	errRate  float64
}

func (g *generator) generateTrace(ctx context.Context) {
	root := roots[g.rng.Intn(len(roots))]
	g.generateSpan(ctx, root, 0)
}

func (g *generator) generateSpan(ctx context.Context, name string, depth int) {
	s := g.svcs[name]
	op := s.ops[g.rng.Intn(len(s.ops))]

	ctx, sp := s.tracer.Start(ctx, op)
	defer sp.End()

	statusCode := 200
	if g.rng.Float64() < g.errRate {
		sp.SetStatus(codes.Error, "simulated error")
		switch g.rng.Intn(3) {
		case 0:
			statusCode = 500
		case 1:
			statusCode = 502
		default:
			statusCode = 503
		}
	}
	sp.SetAttributes(attribute.Int("http.status_code", statusCode))

	if depth < g.maxDepth && len(s.down) > 0 {
		// Pick a random number of downstream calls (0..len), biased fewer at depth.
		n := g.rng.Intn(len(s.down) + 1)
		if depth > 0 && g.rng.Float64() < 0.3 {
			n = 0
		}

		perm := g.rng.Perm(len(s.down))
		for i := 0; i < n; i++ {
			// Small gap between sequential child calls
			time.Sleep(time.Duration(50+g.rng.Intn(200)) * time.Microsecond)
			g.generateSpan(ctx, s.down[perm[i]], depth+1)
		}
	}

	// Simulate work
	time.Sleep(time.Duration(100+g.rng.Intn(500)) * time.Microsecond)
}
