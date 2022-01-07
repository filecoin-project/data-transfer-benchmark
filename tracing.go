package main

import (
	"strings"

	"github.com/testground/sdk-go/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
)

// When sending directly to the collector, agent options are ignored.
// The collector endpoint is an HTTP or HTTPs URL.
// The agent endpoint is a thrift/udp protocol and should be given
// as a string like "hostname:port". The agent can also be configured
// with separate host and port variables.
func jaegerOptsFromEnv(runenv *runtime.RunEnv) jaeger.EndpointOption {
	if collectorEndpoint := runenv.StringParam("jaeger_collector_endpoint"); collectorEndpoint != "null" {
		options := []jaeger.CollectorEndpointOption{jaeger.WithEndpoint(collectorEndpoint)}
		if user := runenv.StringParam("jaeger_username"); user != "null" {
			if password := runenv.StringParam("jaeger_password"); password != "null" {
				options = append(options, jaeger.WithUsername(user))
				options = append(options, jaeger.WithPassword(password))
			} else {
				runenv.RecordMessage("jaeger username supplied with no password. authentication will not be used.")
			}
		}
		runenv.RecordMessage("jaeger tracess will send to collector %s", collectorEndpoint)
		return jaeger.WithCollectorEndpoint(options...)
	}

	if host := runenv.StringParam("jaeger_agent_host"); host != "null" {
		options := []jaeger.AgentEndpointOption{jaeger.WithAgentHost(host)}
		var ep string
		if port := runenv.StringParam("jaeger_agent_port"); port != "null" {
			options = append(options, jaeger.WithAgentPort(port))
			ep = strings.Join([]string{host, port}, ":")
		} else {
			ep = strings.Join([]string{host, "6831"}, ":")
		}
		runenv.RecordMessage("jaeger traces will be sent to agent %s", ep)
		return jaeger.WithAgentEndpoint(options...)
	}
	return nil
}

// setupJaegerTracing reads jaeger options from the manifest and redirects traces to
// jaeger if present

func setupJaegerTracing(runenv *runtime.RunEnv, serviceName string) (*tracesdk.TracerProvider, error) {
	jaegerEndpoint := jaegerOptsFromEnv(runenv)
	if jaegerEndpoint == nil {
		return nil, nil
	}
	je, err := jaeger.New(jaegerEndpoint)
	if err != nil {
		runenv.RecordMessage("failed to create the jaeger exporter", "error", err)
		return nil, err
	}
	tp := tracesdk.NewTracerProvider(
		// Always be sure to batch in production.
		tracesdk.WithBatcher(je),
		// Record information about this application in an Resource.
		tracesdk.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(serviceName),
		)),
		tracesdk.WithSampler(tracesdk.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	return tp, nil
}
