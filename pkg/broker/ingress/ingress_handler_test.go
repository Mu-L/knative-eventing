/*
Copyright 2019 The Knative Authors

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

package ingress

import (
	"bytes"
	"context"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	filteredconfigmapinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/configmap/filtered/fake"
	filteredFactory "knative.dev/pkg/client/injection/kube/informers/factory/filtered"
	"knative.dev/pkg/observability/metrics/metricstest"
	"knative.dev/pkg/observability/tracing"
	"knative.dev/pkg/system"

	"knative.dev/eventing/pkg/eventingtls"

	"github.com/cloudevents/sdk-go/v2/client"
	"github.com/cloudevents/sdk-go/v2/event"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	configmapinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/configmap/fake"

	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/configmap"

	reconcilertesting "knative.dev/pkg/reconciler/testing"

	"knative.dev/eventing/pkg/apis/eventing"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	"knative.dev/eventing/pkg/apis/feature"
	"knative.dev/eventing/pkg/auth"
	"knative.dev/eventing/pkg/broker"

	brokerinformerfake "knative.dev/eventing/pkg/client/injection/informers/eventing/v1/broker/fake"
	eventpolicyinformerfake "knative.dev/eventing/pkg/client/injection/informers/eventing/v1alpha1/eventpolicy/fake"

	_ "knative.dev/pkg/client/injection/kube/client/fake"
	_ "knative.dev/pkg/client/injection/kube/informers/factory/filtered/fake"

	// Fake injection client
	_ "knative.dev/eventing/pkg/client/injection/informers/eventing/v1alpha1/eventpolicy/fake"
)

const (
	senderResponseStatusCode = nethttp.StatusAccepted
)

func TestHandler_ServeHTTP(t *testing.T) {
	t.Parallel()

	logger := zap.NewNop()

	tt := []struct {
		name                   string
		method                 string
		uri                    string
		body                   io.Reader
		headers                nethttp.Header
		expectedHeaders        nethttp.Header
		statusCode             int
		handler                nethttp.Handler
		defaulter              client.EventDefaulter
		brokers                []*eventingv1.Broker
		expectDispatchDuration bool
	}{
		{
			name:       "invalid method PATCH",
			method:     nethttp.MethodPatch,
			uri:        "/ns/name",
			body:       getValidEvent(),
			statusCode: nethttp.StatusMethodNotAllowed,
			handler:    handler(),
			defaulter:  broker.TTLDefaulter(logger, 100),
		},
		{
			name:       "invalid method PUT",
			method:     nethttp.MethodPut,
			uri:        "/ns/name",
			body:       getValidEvent(),
			statusCode: nethttp.StatusMethodNotAllowed,
			handler:    handler(),
			defaulter:  broker.TTLDefaulter(logger, 100),
		},
		{
			name:       "invalid method DELETE",
			method:     nethttp.MethodDelete,
			uri:        "/ns/name",
			body:       getValidEvent(),
			statusCode: nethttp.StatusMethodNotAllowed,
			handler:    handler(),
			defaulter:  broker.TTLDefaulter(logger, 100),
		},
		{
			name:       "invalid method GET",
			method:     nethttp.MethodGet,
			uri:        "/ns/name",
			body:       getValidEvent(),
			statusCode: nethttp.StatusMethodNotAllowed,
			handler:    handler(),
			defaulter:  broker.TTLDefaulter(logger, 100),
		},
		{
			name:   "valid method OPTIONS",
			method: nethttp.MethodOptions,
			uri:    "/ns/name",
			body:   strings.NewReader(""),
			expectedHeaders: nethttp.Header{
				"Allow":                  []string{"PUT, OPTIONS"},
				"WebHook-Allowed-Origin": []string{"*"},
				"WebHook-Allowed-Rate":   []string{"*"},
				"Content-Length":         []string{"0"},
			},
			statusCode: nethttp.StatusOK,
			handler:    handler(),
			defaulter:  broker.TTLDefaulter(logger, 100),
		},
		{
			name:   "valid (happy path POST)",
			method: nethttp.MethodPost,
			uri:    "/ns/name",
			body:   getValidEvent(),
			expectedHeaders: nethttp.Header{
				"Allow": []string{"PUT, OPTIONS"},
			},
			statusCode: senderResponseStatusCode,
			handler:    handler(),
			defaulter:  broker.TTLDefaulter(logger, 100),
			brokers: []*eventingv1.Broker{
				makeBroker("name", "ns"),
			},
			expectDispatchDuration: true,
		},
		{
			name:   "valid - ignore trailing slash (happy path POST)",
			method: nethttp.MethodPost,
			uri:    "/ns/name/",
			body:   getValidEvent(),
			expectedHeaders: nethttp.Header{
				"Allow": []string{"PUT, OPTIONS"},
			},
			statusCode: senderResponseStatusCode,
			handler:    handler(),
			defaulter:  broker.TTLDefaulter(logger, 100),
			brokers: []*eventingv1.Broker{
				makeBroker("name", "ns"),
			},
			expectDispatchDuration: true,
		},
		{
			name:       "invalid event",
			method:     nethttp.MethodPost,
			uri:        "/ns/name",
			body:       getInvalidEvent(),
			statusCode: nethttp.StatusBadRequest,
			handler:    handler(),
			brokers: []*eventingv1.Broker{
				makeBroker("name", "ns"),
			},
		},
		{
			name:       "no TTL drop event",
			method:     nethttp.MethodPost,
			uri:        "/ns/name",
			body:       getValidEvent(),
			statusCode: nethttp.StatusBadRequest,
			handler:    handler(),
			brokers: []*eventingv1.Broker{
				makeBroker("name", "ns"),
			},
		},
		{
			name:       "malformed request URI",
			method:     nethttp.MethodPost,
			uri:        "/knative/ns/name",
			body:       getValidEvent(),
			statusCode: nethttp.StatusBadRequest,
			handler:    handler(),
			brokers: []*eventingv1.Broker{
				makeBroker("name", "ns"),
			},
		},
		{
			name:       "malformed event",
			method:     nethttp.MethodPost,
			uri:        "/ns/name",
			body:       strings.NewReader("not an event"),
			statusCode: nethttp.StatusBadRequest,
			handler:    handler(),
			brokers: []*eventingv1.Broker{
				makeBroker("name", "ns"),
			},
		},
		{
			name:       "no broker annotations",
			method:     nethttp.MethodPost,
			uri:        "/ns/name",
			body:       getValidEvent(),
			statusCode: nethttp.StatusInternalServerError,
			handler:    handler(),
			defaulter:  broker.TTLDefaulter(logger, 100),
			brokers: []*eventingv1.Broker{
				withUninitializedAnnotations(makeBroker("name", "ns")),
			},
		},
		{
			name:       "root request URI",
			method:     nethttp.MethodPost,
			uri:        "/",
			body:       getValidEvent(),
			statusCode: nethttp.StatusNotFound,
			handler:    handler(),
			brokers: []*eventingv1.Broker{
				makeBroker("name", "ns"),
			},
		},
		{
			name:       "pass headers to handler",
			method:     nethttp.MethodPost,
			uri:        "/ns/name",
			body:       getValidEvent(),
			statusCode: senderResponseStatusCode,
			headers: nethttp.Header{
				"foo":              []string{"bar"},
				"Traceparent":      []string{"0"},
				"Knative-Foo":      []string{"123"},
				"X-Request-Id":     []string{"123"},
				cehttp.ContentType: []string{event.ApplicationCloudEventsJSON},
			},
			handler: &svc{},
			expectedHeaders: nethttp.Header{
				"Knative-Foo":  []string{"123"},
				"X-Request-Id": []string{"123"},
			},
			defaulter: broker.TTLDefaulter(logger, 100),
			brokers: []*eventingv1.Broker{
				makeBroker("name", "ns"),
			},
			expectDispatchDuration: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := reconcilertesting.SetupFakeContext(t, SetUpInformerSelector)
			trustBundleConfigMapLister := filteredconfigmapinformer.Get(ctx, eventingtls.TrustBundleLabelSelector).Lister().ConfigMaps(system.Namespace())

			s := httptest.NewServer(tc.handler)
			defer s.Close()

			reader := metric.NewManualReader()
			mp := metric.NewMeterProvider(metric.WithReader(reader))

			exporter := tracetest.NewInMemoryExporter()
			tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
			otel.SetTextMapPropagator(tracing.DefaultTextMapPropagator())

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.uri, tc.body)
			if tc.headers != nil {
				request.Header = tc.headers
			} else {
				tc.expectedHeaders = nethttp.Header{
					cehttp.ContentType: []string{event.ApplicationCloudEventsJSON},
				}
				request.Header.Add(cehttp.ContentType, event.ApplicationCloudEventsJSON)
			}

			for _, b := range tc.brokers {
				// Write the channel address in the broker status annotation unless explicitly set to nil
				if b.Status.Annotations != nil {
					if _, set := b.Status.Annotations[eventing.BrokerChannelAddressStatusAnnotationKey]; !set {
						b.Status.Annotations = map[string]string{
							eventing.BrokerChannelAddressStatusAnnotationKey: s.URL,
						}
					}
				}
				brokerinformerfake.Get(ctx).Informer().GetStore().Add(b)
			}

			tokenProvider := auth.NewOIDCTokenProvider(ctx)
			authVerifier := auth.NewVerifier(ctx, eventpolicyinformerfake.Get(ctx).Lister(), trustBundleConfigMapLister, configmap.NewStaticWatcher(
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "config-features",
						Namespace: "knative-eventing",
					},
					Data: map[string]string{
						feature.OIDCAuthentication: string(feature.Enabled),
					},
				},
			))

			h, err := NewHandler(logger,
				tc.defaulter,
				brokerinformerfake.Get(ctx),
				authVerifier,
				tokenProvider,
				configmapinformer.Get(ctx).Lister().ConfigMaps("ns"),
				func(ctx context.Context) context.Context {
					return ctx
				},
				mp,
				tp,
			)
			if err != nil {
				t.Fatal("Unable to create receiver:", err)
			}

			h.ServeHTTP(recorder, request)

			result := recorder.Result()
			if result.StatusCode != tc.statusCode {
				t.Errorf("expected status code %d got %d", tc.statusCode, result.StatusCode)
			}

			if svc, ok := tc.handler.(*svc); ok {
				for k, expValue := range tc.expectedHeaders {
					if v, ok := svc.receivedHeaders[k]; !ok {
						t.Errorf("expected header %s - %v", k, svc.receivedHeaders)
					} else if diff := cmp.Diff(expValue, v); diff != "" {
						t.Error("(-want +got)", diff)
					}
				}
			}

			expectedMetricNames := []string{}
			if tc.expectDispatchDuration {
				expectedMetricNames = append(expectedMetricNames, "kn.eventing.dispatch.duration")
			}

			metricstest.AssertMetrics(t, reader, metricstest.MetricsPresent(ScopeName, expectedMetricNames...))
		})
	}
}

type svc struct {
	receivedHeaders nethttp.Header
}

func (s *svc) ServeHTTP(w nethttp.ResponseWriter, req *nethttp.Request) {
	s.receivedHeaders = req.Header
	w.WriteHeader(senderResponseStatusCode)
}

func handler() nethttp.Handler {
	return nethttp.HandlerFunc(func(writer nethttp.ResponseWriter, request *nethttp.Request) {
		writer.WriteHeader(senderResponseStatusCode)
	})
}

func getValidEvent() io.Reader {
	e := event.New()
	e.SetType("type")
	e.SetSource("source")
	e.SetID("1234")
	b, _ := e.MarshalJSON()
	return bytes.NewBuffer(b)
}

func getInvalidEvent() io.Reader {
	e := event.New()
	e.SetType("type")
	e.SetID("1234")
	b, _ := e.MarshalJSON()
	return bytes.NewBuffer(b)
}

func makeBroker(name, namespace string) *eventingv1.Broker {
	return &eventingv1.Broker{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "eventing.knative.dev/v1",
			Kind:       "Broker",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: eventingv1.BrokerSpec{},
		Status: eventingv1.BrokerStatus{
			Status: duckv1.Status{
				Annotations: map[string]string{},
			},
		},
	}
}

func withUninitializedAnnotations(b *eventingv1.Broker) *eventingv1.Broker {
	b.Status.Annotations = nil
	return b
}

func SetUpInformerSelector(ctx context.Context) context.Context {
	ctx = filteredFactory.WithSelectors(ctx, eventingtls.TrustBundleLabelSelector)
	return ctx
}
