/*
Copyright 2021 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/cloudevents/sdk-go/observability/opentelemetry/v2/client"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/trace"
	"knative.dev/eventing/pkg/observability"
	eventingotel "knative.dev/eventing/pkg/observability/otel"
	"knative.dev/eventing/pkg/observability/resource"
	"knative.dev/pkg/observability/metrics"
	"knative.dev/pkg/observability/tracing"
)

/*
Example Output:

☁️  cloudevents.Event
Validation: valid
Context Attributes,
  specversion: 1.0
  type: dev.knative.eventing.samples.heartbeat
  source: https://knative.dev/eventing-contrib/cmd/heartbeats/#event-test/mypod
  id: 2b72d7bf-c38f-4a98-a433-608fbcdd2596
  time: 2019-10-18T15:23:20.809775386Z
  contenttype: application/json
Extensions,
  beats: true
  heart: yes
  the: 42
Data,
  {
    "id": 2,
    "label": ""
  }
*/

// display prints the given Event in a human-readable format.
func display(event cloudevents.Event) {
	fmt.Printf("☁️  cloudevents.Event\n%s", event)
}

func main() {
	run(context.Background())
}

func run(ctx context.Context) {

	requestLoggingEnabled, _ := strconv.ParseBool(os.Getenv("REQUEST_LOGGING_ENABLED"))
	if requestLoggingEnabled {
		log.Println("Request logging enabled, request logging is not recommended for production since it might log sensitive information")
	}

	c, err := client.NewClientHTTP(
		[]cehttp.Option{
			cehttp.WithMiddleware(healthzMiddleware),
			cehttp.WithMiddleware(requestLoggingMiddleware(requestLoggingEnabled)),
		}, nil,
	)
	if err != nil {
		log.Fatal("Failed to create client: ", err)
	}

	cfg := &observability.Config{}

	err = json.Unmarshal([]byte(os.Getenv("K_OBSERVABILITY_CONFIG")), cfg)
	if err != nil {
		log.Printf("failed to parse observability config from env, falling back to default config\n")
	}
	cfg = observability.MergeWithDefaults(cfg)

	ctx = observability.WithConfig(ctx, cfg)

	otelResource, err := resource.Default("hearbeat")
	if err != nil {
		log.Printf("failed to correctly initialize otel resource, resouce may be missing some attributes: %s\n", err.Error())
	}

	meterProvider, err := metrics.NewMeterProvider(
		ctx,
		cfg.Metrics,
		metric.WithResource(otelResource),
	)
	if err != nil {
		log.Printf("failed to setup meter provider, falling back to noop: %s\n", err.Error())
		meterProvider = eventingotel.DefaultMeterProvider(ctx, otelResource)
	}

	otel.SetMeterProvider(meterProvider)

	tracerProvider, err := tracing.NewTracerProvider(
		ctx,
		cfg.Tracing,
		trace.WithResource(otelResource),
	)
	if err != nil {
		log.Printf("failed to setup tracing provider, falling back to noop: %s\n", err.Error())
		tracerProvider = eventingotel.DefaultTraceProvider(ctx, otelResource)
	}

	defer func() {
		ctx, cancel := context.WithTimeout(ctx, time.Second*10)
		defer cancel()

		if err := meterProvider.Shutdown(ctx); err != nil {
			log.Printf("failed to shut down metrics: %s\n", err.Error())
		}

		if err := tracerProvider.Shutdown(ctx); err != nil {
			log.Printf("failed to shut down tracing: %s\n", err.Error())
		}
	}()

	otel.SetTextMapPropagator(tracing.DefaultTextMapPropagator())
	otel.SetTracerProvider(tracerProvider)
	if err := c.StartReceiver(ctx, display); err != nil {
		log.Fatal("Error during receiver's runtime: ", err)
	}
}

// HTTP path of the health endpoint used for probing the service.
const healthzPath = "/healthz"

// healthzMiddleware is a cehttp.Middleware which exposes a health endpoint.
func healthzMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.RequestURI == healthzPath {
			w.WriteHeader(http.StatusNoContent)
		} else {
			next.ServeHTTP(w, req)
		}
	})
}

// requestLoggingMiddleware is a cehttp.Middleware which logs incoming requests.
func requestLoggingMiddleware(enabled bool) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if enabled {
				logRequest(req)
			}
			next.ServeHTTP(w, req)
		})
	}
}

type LoggableRequest struct {
	Method           string      `json:"method,omitempty"`
	URL              *url.URL    `json:"URL,omitempty"`
	Proto            string      `json:"proto,omitempty"`
	ProtoMajor       int         `json:"protoMajor,omitempty"`
	ProtoMinor       int         `json:"protoMinor,omitempty"`
	Header           http.Header `json:"headers,omitempty"`
	Body             string      `json:"body,omitempty"`
	ContentLength    int64       `json:"contentLength,omitempty"`
	TransferEncoding []string    `json:"transferEncoding,omitempty"`
	Host             string      `json:"host,omitempty"`
	Trailer          http.Header `json:"trailer,omitempty"`
	RemoteAddr       string      `json:"remoteAddr"`
	RequestURI       string      `json:"requestURI"`
}

func logRequest(req *http.Request) {
	b, err := json.MarshalIndent(toReq(req), "", "  ")
	if err != nil {
		log.Println("failed to marshal request", err)
	}

	log.Println(string(b))
}

func toReq(req *http.Request) LoggableRequest {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.Println("failed to read request body")
	}
	_ = req.Body.Close()
	// Replace the body with a new reader after reading from the original
	req.Body = io.NopCloser(bytes.NewBuffer(body))
	return LoggableRequest{
		Method:           req.Method,
		URL:              req.URL,
		Proto:            req.Proto,
		ProtoMajor:       req.ProtoMajor,
		ProtoMinor:       req.ProtoMinor,
		Header:           req.Header,
		Body:             string(body),
		ContentLength:    req.ContentLength,
		TransferEncoding: req.TransferEncoding,
		Host:             req.Host,
		Trailer:          req.Trailer,
		RemoteAddr:       req.RemoteAddr,
		RequestURI:       req.RequestURI,
	}
}
