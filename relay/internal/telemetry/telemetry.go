package telemetry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"google.golang.org/grpc/credentials"
)

// Config holds telemetry configuration.
type Config struct {
	Endpoint    string  // OTLP gRPC endpoint (e.g. "traces.example.com:443"). Empty disables telemetry.
	TLSCertPath string  // Client certificate PEM for mTLS. Both cert and key required for mTLS.
	TLSKeyPath  string  // Client private key PEM for mTLS.
	ServiceName string  // OTel service name (default: "pushward-relay").
	Environment string  // Deployment environment (e.g. "production", "development").
	SampleRate  float64 // Sampling rate 0.0-1.0 (default: 1.0 = sample everything).
}

// Init initialises the OpenTelemetry trace provider and returns a shutdown
// function. When endpoint is empty, telemetry is disabled and a noop shutdown
// is returned with zero overhead.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }

	if cfg.Endpoint == "" {
		slog.Info("telemetry disabled (no endpoint configured)")
		return noop, nil
	}

	if cfg.ServiceName == "" {
		cfg.ServiceName = "pushward-relay"
	}
	// Only an out-of-range rate falls back to 1.0; an explicit 0 is honored as
	// "sample nothing" (the config layer defaults unset to 1.0).
	if cfg.SampleRate < 0 || cfg.SampleRate > 1.0 {
		cfg.SampleRate = 1.0
	}

	if (cfg.TLSCertPath == "") != (cfg.TLSKeyPath == "") {
		return noop, fmt.Errorf("telemetry: tls_cert_path and tls_key_path must both be set or both empty")
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}

	if cfg.TLSCertPath != "" && cfg.TLSKeyPath != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertPath, cfg.TLSKeyPath)
		if err != nil {
			return noop, fmt.Errorf("loading client certificate: %w", err)
		}
		tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
		opts = append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		opts = append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(&tls.Config{})))
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return noop, fmt.Errorf("creating OTLP exporter: %w", err)
	}

	// Include the default detectors (telemetry.sdk.*, host/process/runtime,
	// OTEL_RESOURCE_ATTRIBUTES/OTEL_SERVICE_NAME) so traces carry standard
	// metadata; WithAttributes is applied last so the explicit service/env win.
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithProcessRuntimeName(),
		resource.WithProcessRuntimeVersion(),
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.DeploymentEnvironmentName(cfg.Environment),
		),
	)
	// A single failing detector (e.g. host lookup unavailable in a restricted
	// container) returns ErrPartialResource with a still-valid partial resource;
	// degrade to it rather than disabling telemetry entirely. Only a nil resource
	// is fatal.
	if err != nil {
		if errors.Is(err, resource.ErrPartialResource) && res != nil {
			slog.Warn("partial OTel resource; some detectors failed", "error", err)
		} else {
			return noop, fmt.Errorf("creating OTel resource: %w", err)
		}
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(tp)
	// Propagate both W3C TraceContext and Baggage so upstream baggage (e.g. a
	// tenant or feature-flag attribute) is not silently dropped at the edge.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	slog.Info("telemetry enabled",
		"endpoint", cfg.Endpoint,
		"service", cfg.ServiceName,
		"environment", cfg.Environment,
		"sample_rate", cfg.SampleRate,
		"mtls", cfg.TLSCertPath != "",
	)

	return tp.Shutdown, nil
}
