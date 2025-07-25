/*
Copyright 2020 The Knative Authors

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

package inmemorychannel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"sync"
	"testing"
	"time"

	"github.com/cloudevents/sdk-go/v2/binding"
	"github.com/cloudevents/sdk-go/v2/binding/transformer"
	protocolhttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"github.com/cloudevents/sdk-go/v2/test"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
	"k8s.io/client-go/rest"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/observability/tracing"

	"knative.dev/eventing/pkg/auth"
	"knative.dev/eventing/pkg/channel/fanout"
	"knative.dev/eventing/pkg/channel/multichannelfanout"
	"knative.dev/eventing/pkg/eventingtls"
	"knative.dev/eventing/pkg/kncloudevents"

	fakekubeclient "knative.dev/pkg/client/injection/kube/client/fake"
	logtesting "knative.dev/pkg/logging/testing"
	_ "knative.dev/pkg/system/testing"
)

func TestNewEventDispatcher(t *testing.T) {
	logger := logtesting.TestLogger(t).Desugar()
	sh := multichannelfanout.NewEventHandler(context.TODO(), logger)

	args := &InMemoryEventDispatcherArgs{
		Port:         8080,
		ReadTimeout:  1 * time.Minute,
		WriteTimeout: 1 * time.Minute,
		Handler:      sh,
		Logger:       logger,
	}

	d := NewEventDispatcher(args)

	if d == nil {
		t.Fatalf("Failed to create with NewDispatcher")
	}
}

// This test emulates a real dispatcher usage
func TestDispatcher_close(t *testing.T) {
	logger := logtesting.TestLogger(t).Desugar()
	sh := multichannelfanout.NewEventHandler(context.TODO(), logger)

	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}

	dispatcherArgs := &InMemoryEventDispatcherArgs{
		Port:         port,
		ReadTimeout:  1 * time.Minute,
		WriteTimeout: 1 * time.Minute,
		Handler:      sh,
		Logger:       logger,
	}

	dispatcher := NewEventDispatcher(dispatcherArgs)
	kncloudevents.WithDrainQuietPeriod(time.Nanosecond)(dispatcher.httpBindingsReceiver)

	serverCtx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- dispatcher.Start(serverCtx)
	}()

	cancel()
	err = <-errCh
	if err != nil {
		t.Fatal(err)
	}
}

// This test emulates a real dispatcher usage
func TestDispatcher_dispatch(t *testing.T) {
	ctx := context.Background()
	ctx, _ = fakekubeclient.With(ctx)
	ctx = injection.WithConfig(ctx, &rest.Config{})

	logger, err := zap.NewDevelopment(zap.AddStacktrace(zap.WarnLevel))
	oidcTokenProvider := auth.NewOIDCTokenProvider(ctx)
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	otel.SetTextMapPropagator(tracing.DefaultTextMapPropagator())

	dispatcher := kncloudevents.NewDispatcher(
		eventingtls.NewDefaultClientConfig(),
		oidcTokenProvider,
		kncloudevents.WithTraceProvider(tp),
		kncloudevents.WithMeterProvider(mp),
	)
	if err != nil {
		t.Fatal(err)
	}

	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}

	// We need a channelaproxy and channelbproxy for handling correctly the Host header
	channelAProxy := httptest.NewServer(createReverseProxy(t, "channela.svc", port))
	defer channelAProxy.Close()
	channelBProxy := httptest.NewServer(createReverseProxy(t, "channelb.svc", port))
	defer channelBProxy.Close()

	// Start a bunch of test servers to simulate the various services
	transformationsCh := make(chan error, 1)
	transformationsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		message := protocolhttp.NewMessageFromHttpRequest(r)
		defer message.Finish(nil)

		if message.ReadEncoding() == binding.EncodingUnknown {
			err := errors.New("Message has encoding unknown")
			logger.Fatal("Error in transformation handler", zap.Error(err))
			transformationsCh <- err
		}

		err := protocolhttp.WriteResponseWriter(context.Background(), message, 200, w, transformer.AddExtension("transformed", "true"))
		if err != nil {
			logger.Fatal("Error in transformation handler", zap.Error(err))
			transformationsCh <- err
			w.WriteHeader(http.StatusInternalServerError)
		}
		transformationsCh <- nil
	}))
	defer transformationsServer.Close()

	receiverWg := sync.WaitGroup{}
	receiverWg.Add(1)
	receiverCh := make(chan error, 1)
	receiverServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receiverWg.Done()
		transformed := r.Header.Get("ce-transformed")
		if transformed != "true" {
			err := fmt.Errorf("expecting ce-transformed: true, found %s", transformed)
			logger.Fatal("Error in receiver", zap.Error(err))
			receiverCh <- err
			w.WriteHeader(http.StatusInternalServerError)
		}
		receiverCh <- nil
	}))
	defer receiverServer.Close()

	transformationsFailureWg := sync.WaitGroup{}
	transformationsFailureWg.Add(1)
	transformationsFailureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer transformationsFailureWg.Done()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer transformationsFailureServer.Close()

	deadLetterWg := sync.WaitGroup{}
	deadLetterWg.Add(1)
	deadLetterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer deadLetterWg.Done()
		transformed := r.Header.Get("ce-transformed")
		if transformed != "" {
			w.WriteHeader(http.StatusInternalServerError)
			t.Fatal("Not expecting ce-transformed, found", transformed)
		}
	}))
	defer deadLetterServer.Close()

	logger.Debug("Test servers",
		zap.String("transformations server", transformationsServer.URL),
		zap.String("transformations failure server", transformationsFailureServer.URL),
		zap.String("receiver server", receiverServer.URL),
		zap.String("dead letter server", deadLetterServer.URL),
	)

	// The message flow is:
	// send -> channela -> sub aaaa -> transformationsServer -> channelb -> sub bbbb -> receiver
	// send -> channela -> sub cccc -> transformationsFailureServer
	config := multichannelfanout.Config{
		ChannelConfigs: []multichannelfanout.ChannelConfig{
			{
				Namespace: "default",
				Name:      "channela",
				HostName:  "channela.svc",
				FanoutConfig: fanout.Config{
					AsyncHandler: false,
					Subscriptions: []fanout.Subscription{{
						Subscriber: *mustParseUrlToAddressable(t, transformationsServer.URL),
						Reply:      mustParseUrlToAddressable(t, channelBProxy.URL),
					}, {
						Subscriber: *mustParseUrlToAddressable(t, transformationsFailureServer.URL),
						Reply:      mustParseUrlToAddressable(t, channelBProxy.URL),
						DeadLetter: mustParseUrlToAddressable(t, deadLetterServer.URL),
					}},
				},
			},
			{
				Namespace: "default",
				Name:      "channelb",
				HostName:  "channelb.svc",
				FanoutConfig: fanout.Config{
					AsyncHandler: false,
					Subscriptions: []fanout.Subscription{{
						Subscriber: *mustParseUrlToAddressable(t, receiverServer.URL),
					}},
				},
			},
		},
	}

	sh, err := multichannelfanout.NewEventHandlerWithConfig(context.TODO(), logger, config, dispatcher, mp, tp)
	if err != nil {
		t.Fatal(err)
	}

	logger.Info("Starting dispatcher", zap.Int("port", port))

	dispatcherArgs := &InMemoryEventDispatcherArgs{
		Port:         port,
		ReadTimeout:  1 * time.Minute,
		WriteTimeout: 1 * time.Minute,
		Handler:      sh,
		Logger:       logger,
	}

	inMemoryDispatcher := NewEventDispatcher(dispatcherArgs)

	serverCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the dispatcher
	go func() {
		if err := inMemoryDispatcher.Start(serverCtx); err != nil {
			t.Error(err)
		}
	}()
	inMemoryDispatcher.WaitReady()

	// Ok now everything should be ready to send the event
	d := kncloudevents.NewDispatcher(
		eventingtls.NewDefaultClientConfig(),
		oidcTokenProvider,
		kncloudevents.WithMeterProvider(mp),
		kncloudevents.WithTraceProvider(tp),
	)
	dispatchInfo, err := d.SendEvent(context.TODO(), test.FullEvent(), *mustParseUrlToAddressable(t, channelAProxy.URL))
	if err != nil {
		t.Fatal(err)
	}

	if dispatchInfo.ResponseCode != http.StatusAccepted {
		t.Fatal("Expected 202, Got: ", dispatchInfo.ResponseCode)
	}

	transformationsFailureWg.Wait()
	deadLetterWg.Wait()
	err = <-transformationsCh
	if err != nil {
		t.Fatal(err)
	}
	receiverWg.Wait()
	err = <-receiverCh
	if err != nil {
		t.Fatal(err)
	}
}

func createReverseProxy(t *testing.T, host string, port int) *httputil.ReverseProxy {
	director := func(req *http.Request) {
		target := mustParseUrl(t, fmt.Sprint("http://localhost:", port))
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = target.Path
		req.Host = host
	}
	return &httputil.ReverseProxy{Director: director}
}

func mustParseUrl(t testing.TB, str string) *apis.URL {
	url, err := apis.ParseURL(str)
	if err != nil {
		t.Fatal(err)
	}
	return url
}

func mustParseUrlToAddressable(t testing.TB, url string) *duckv1.Addressable {
	return &duckv1.Addressable{
		URL: mustParseUrl(t, url),
	}
}

func freePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
