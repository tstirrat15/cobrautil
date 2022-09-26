package otel

import (
	"context"
	"fmt"
	"net/url"
	"runtime/debug"
	"strings"

	"github.com/go-logr/logr"
	"github.com/jzelinskie/cobrautil/v2"
	"github.com/jzelinskie/stringz"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/contrib/propagators/ot"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
)

// ConfigureFunc is a function used to configure this CobraUtil
type ConfigureFunc = func(cu *CobraUtil)

// New creates a configuration that exposes RegisterFlags and RunE
// to integrate with cobra
func New(flagPrefix, serviceName string, configurations ...ConfigureFunc) *CobraUtil {
	cu := CobraUtil{
		flagPrefix:  flagPrefix,
		serviceName: serviceName,
		preRunLevel: 1,
		logger:      logr.Discard(),
	}
	for _, configure := range configurations {
		configure(&cu)
	}
	return &cu
}

// CobraUtil carries the configuration for a otel CobraRunFunc
type CobraUtil struct {
	flagPrefix  string
	serviceName string
	logger      logr.Logger
	preRunLevel int
}

// RegisterOpenTelemetryFlags adds the following flags for use with
// OpenTelemetryPreRunE:
// - "$PREFIX-provider"
// - "$PREFIX-endpoint"
// - "$PREFIX-service-name"
func RegisterOpenTelemetryFlags(flags *pflag.FlagSet, flagPrefix, serviceName string) {
	bi, _ := debug.ReadBuildInfo()
	serviceName = stringz.DefaultEmpty(serviceName, bi.Main.Path)
	prefixed := cobrautil.PrefixJoiner(stringz.DefaultEmpty(flagPrefix, "otel"))

	flags.String(prefixed("provider"), "none", `OpenTelemetry provider for tracing ("none", "jaeger, otlphttp", "otlpgrpc")`)
	flags.String(prefixed("endpoint"), "", "OpenTelemetry collector endpoint - the endpoint can also be set by using enviroment variables")
	flags.String(prefixed("service-name"), serviceName, "service name for trace data")
	flags.String(prefixed("trace-propagator"), "w3c", `OpenTelemetry trace propagation format ("b3", "w3c", "ottrace"). Add multiple propagators separated by comma.`)
	flags.Bool(prefixed("insecure"), false, `connect to the OpenTelemetry collector in plaintext`)

	// Legacy flags! Will eventually be dropped!
	flags.String("otel-jaeger-endpoint", "", "OpenTelemetry collector endpoint - the endpoint can also be set by using enviroment variables")
	if err := flags.MarkHidden("otel-jaeger-endpoint"); err != nil {
		panic("failed to mark flag hidden: " + err.Error())
	}
	flags.String("otel-jaeger-service-name", serviceName, "service name for trace data")
	if err := flags.MarkHidden("otel-jaeger-service-name"); err != nil {
		panic("failed to mark flag hidden: " + err.Error())
	}
}

// OpenTelemetryRunE returns a Cobra run func that configures the
// corresponding otel provider from a command.
//
// The required flags can be added to a command by using
// RegisterOpenTelemetryFlags()
func OpenTelemetryRunE(flagPrefix string, preRunLevel int) cobrautil.CobraRunFunc {
	return New(flagPrefix, "").RunE()
}

// RegisterFlags adds the following flags for use with
// OpenTelemetryPreRunE:
// - "$PREFIX-provider"
// - "$PREFIX-endpoint"
// - "$PREFIX-service-name"
func (cu CobraUtil) RegisterFlags(flags *pflag.FlagSet) {
	RegisterOpenTelemetryFlags(flags, cu.flagPrefix, cu.serviceName)
}

// RunE returns a Cobra run func that configures the
// corresponding otel provider from a command.
//
// The required flags can be added to a command by using
// RegisterOpenTelemetryFlags().
func (cu CobraUtil) RunE() cobrautil.CobraRunFunc {
	prefixed := cobrautil.PrefixJoiner(stringz.DefaultEmpty(cu.flagPrefix, "otel"))
	return func(cmd *cobra.Command, args []string) error {
		if cobrautil.IsBuiltinCommand(cmd) {
			return nil // No-op for builtins
		}

		provider := strings.ToLower(cobrautil.MustGetString(cmd, prefixed("provider")))
		serviceName := cobrautil.MustGetString(cmd, prefixed("service-name"))
		endpoint := cobrautil.MustGetString(cmd, prefixed("endpoint"))
		insecure := cobrautil.MustGetBool(cmd, prefixed("insecure"))
		propagators := strings.Split(cobrautil.MustGetString(cmd, prefixed("trace-propagator")), ",")
		var noLogger logr.Logger
		if cu.logger != noLogger {
			otel.SetLogger(cu.logger)
		}

		var exporter trace.SpanExporter
		var err error

		// If endpoint is not set, the clients are configured via the OpenTelemetry environment variables or
		// default values.
		// See: https://github.com/open-telemetry/opentelemetry-go/tree/main/exporters/otlp/otlptrace#environment-variables
		// or https://github.com/open-telemetry/opentelemetry-go/tree/main/exporters/jaeger#environment-variables
		switch provider {
		case "none":
			// Nothing.
		case "jaeger":
			// Legacy flags! Will eventually be dropped!
			endpoint = stringz.DefaultEmpty(endpoint, cobrautil.MustGetString(cmd, "otel-jaeger-endpoint"))
			serviceName = stringz.Default(serviceName, cobrautil.MustGetString(cmd, "otel-jaeger-service-name"), "", cmd.Flags().Lookup(prefixed("service-name")).DefValue)

			var opts []jaeger.CollectorEndpointOption

			if endpoint != "" {
				parsed, err := url.Parse(endpoint)
				if err != nil {
					return fmt.Errorf("failed to parse endpoint: %w", err)
				}
				if (insecure && parsed.Scheme == "https") || (!insecure && parsed.Scheme == "http") {
					return fmt.Errorf("endpoint schema is %s but insecure flag is set to %t", parsed.Scheme, insecure)
				}
				opts = append(opts, jaeger.WithEndpoint(endpoint))
			}

			exporter, err = jaeger.New(jaeger.WithCollectorEndpoint(opts...))
			if err != nil {
				return err
			}

			if err := initOtelTracer(exporter, serviceName, propagators); err != nil {
				return err
			}
		case "otlphttp":
			var opts []otlptracehttp.Option
			if endpoint != "" {
				opts = append(opts, otlptracehttp.WithEndpoint(endpoint))
			}
			if insecure {
				opts = append(opts, otlptracehttp.WithInsecure())
			}
			exporter, err = otlptrace.New(context.Background(), otlptracehttp.NewClient(opts...))
			if err != nil {
				return err
			}

			if err := initOtelTracer(exporter, serviceName, propagators); err != nil {
				return err
			}
		case "otlpgrpc":
			var opts []otlptracegrpc.Option
			if endpoint != "" {
				opts = append(opts, otlptracegrpc.WithEndpoint(endpoint))
			}
			if insecure {
				opts = append(opts, otlptracegrpc.WithInsecure())
			}

			exporter, err = otlptrace.New(context.Background(), otlptracegrpc.NewClient(opts...))
			if err != nil {
				return err
			}

			if err := initOtelTracer(exporter, serviceName, propagators); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown tracing provider: %s", provider)
		}

		cu.logger.V(cu.preRunLevel).
			Info("setup opentelemetry tracing", "provider", provider,
				"endpoint", endpoint,
				"service", serviceName,
				"insecure", insecure)
		return nil
	}
}

func WithLogger(logger logr.Logger) ConfigureFunc {
	return func(cu *CobraUtil) {
		cu.logger = logger
	}
}

func initOtelTracer(exporter trace.SpanExporter, serviceName string, propagators []string) error {
	res, err := resource.New(
		context.Background(),
		resource.WithAttributes(semconv.ServiceNameKey.String(serviceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return err
	}

	otel.SetTracerProvider(trace.NewTracerProvider(
		trace.WithSampler(trace.AlwaysSample()),
		trace.WithBatcher(exporter),
		trace.WithResource(res),
	))
	setTracePropagators(propagators)

	return nil
}

// setTextMapPropagator sets the OpenTelemetry trace propagation format.
// Currently it supports b3, ot-trace and w3c.
func setTracePropagators(propagators []string) {
	var tmPropagators []propagation.TextMapPropagator

	for _, p := range propagators {
		switch p {
		case "b3":
			tmPropagators = append(tmPropagators, b3.New())
		case "ottrace":
			tmPropagators = append(tmPropagators, ot.OT{})
		case "w3c":
			fallthrough
		default:
			tmPropagators = append(tmPropagators, propagation.Baggage{})      // W3C baggage support
			tmPropagators = append(tmPropagators, propagation.TraceContext{}) // W3C for compatibility with other tracing system
		}
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(tmPropagators...))
}
