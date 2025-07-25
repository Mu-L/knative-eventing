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

package filter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	filteredFactory "knative.dev/pkg/client/injection/kube/informers/factory/filtered"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/observability/metrics/metricstest"
	"knative.dev/pkg/system"

	"knative.dev/eventing/pkg/eventingtls"

	messagingv1 "knative.dev/eventing/pkg/apis/messaging/v1"
	"knative.dev/eventing/pkg/reconciler/broker/resources"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/binding"
	"github.com/cloudevents/sdk-go/v2/event"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/apis"
	configmapinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/configmap/fake"
	filteredconfigmapinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/configmap/filtered/fake"
	"knative.dev/pkg/logging"
	reconcilertesting "knative.dev/pkg/reconciler/testing"

	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	v1 "knative.dev/eventing/pkg/apis/eventing/v1"
	"knative.dev/eventing/pkg/apis/feature"
	"knative.dev/eventing/pkg/auth"
	"knative.dev/eventing/pkg/broker"
	"knative.dev/eventing/pkg/eventfilter/subscriptionsapi"

	brokerinformerfake "knative.dev/eventing/pkg/client/injection/informers/eventing/v1/broker/fake"
	triggerinformerfake "knative.dev/eventing/pkg/client/injection/informers/eventing/v1/trigger/fake"
	eventpolicyinformerfake "knative.dev/eventing/pkg/client/injection/informers/eventing/v1alpha1/eventpolicy/fake"
	subscriptioninformerfake "knative.dev/eventing/pkg/client/injection/informers/messaging/v1/subscription/fake"

	_ "knative.dev/pkg/client/injection/kube/client/fake"
	_ "knative.dev/pkg/client/injection/kube/informers/factory/filtered/fake"

	// Fake injection client
	_ "knative.dev/eventing/pkg/client/injection/informers/eventing/v1alpha1/eventpolicy/fake"
)

const (
	testNS         = "test-namespace"
	triggerName    = "test-trigger"
	triggerUID     = "test-trigger-uid"
	eventType      = `com.example.someevent`
	eventSource    = `/mycontext`
	extensionName  = `myextension`
	extensionValue = `my-extension-value`

	// Because it's a URL we're comparing to, without protocol it looks like this.
	toBeReplaced = "//toBeReplaced"

	invalidEvent = `{"id":"1234","knativebrokerttl":1,"source":"/mycontext","specversion":"0.1","type":"com.example.someevent"}`
)

var (
	validPath = fmt.Sprintf("/triggers/%s/%s/%s", testNS, triggerName, triggerUID)
)

type TriggerOption func(trigger *eventingv1.Trigger)

func TestReceiver(t *testing.T) {
	testCases := map[string]struct {
		// input
		triggers               []*eventingv1.Trigger
		request                *http.Request
		event                  *cloudevents.Event
		requestFails           bool
		failureStatus          int
		additionalReplyHeaders http.Header

		// expectations
		expectedResponseEvent       *cloudevents.Event
		expectedResponse            *http.Response
		expectedDispatch            bool
		expectedStatus              int
		expectedHeaders             http.Header
		expectedEventDispatchTime   bool
		expectedEventProcessingTime bool
		expectedResponseHeaders     http.Header
	}{
		"Not POST": {
			request:        httptest.NewRequest(http.MethodGet, validPath, nil),
			expectedStatus: http.StatusMethodNotAllowed,
		},
		"Path too short": {
			request:        httptest.NewRequest(http.MethodPost, "/test-namespace/test-trigger", nil),
			expectedStatus: http.StatusBadRequest,
		},
		"Path too long": {
			request:        httptest.NewRequest(http.MethodPost, "/triggers/test-namespace/test-trigger/uuid/extra/extra", nil),
			expectedStatus: http.StatusBadRequest,
		},
		"Path without prefix": {
			request:        httptest.NewRequest(http.MethodPost, "/something/test-namespace/test-trigger", nil),
			expectedStatus: http.StatusBadRequest,
		},
		"Trigger.Get fails": {
			// No trigger exists, so the Get will fail.
			expectedStatus: http.StatusNotFound,
		},
		"Trigger doesn't have SubscriberURI": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withoutSubscriberURI()),
			},
			expectedStatus: http.StatusNotFound,
		},
		"Trigger without a Filter": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(),
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
		},
		"No TTL": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{
					Attributes: map[string]string{"type": "some-other-type"},
				})),
			},
			event: makeEventWithoutTTL(),
		},
		"Wrong type": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{
					Attributes: map[string]string{"type": "some-other-type"},
				})),
			},
		},
		"Wrong source": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{
					Attributes: map[string]string{"source": "some-other-source"},
				})),
			},
		},
		"Wrong extension": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{
					Attributes: map[string]string{extensionName: "some-other-extension"},
				})),
			},
		},
		"Dispatch failed": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			requestFails:                true,
			expectedStatus:              http.StatusBadRequest,
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
		},
		"GetTrigger fails": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(
					withAttributesFilter(&eventingv1.TriggerFilter{}),
					withUID("wrongone"),
				),
			},
			expectedDispatch:          false,
			expectedEventDispatchTime: false,
		},
		"Dispatch succeeded - Any": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
		},
		"Dispatch succeeded - Source with type": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{
					Attributes: map[string]string{"type": eventType, "source": eventSource},
				})),
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
		},
		"Dispatch succeeded - Source, type and extensions": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{
					Attributes: map[string]string{"type": eventType, "source": eventSource, extensionName: extensionValue},
				})),
			},
			event:                       makeEventWithExtension(extensionName, extensionValue),
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
		},
		"Dispatch succeeded - Any - Arrival extension": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			event:                       makeEventWithExtension(broker.EventArrivalTime, "2019-08-26T23:38:17.834384404Z"),
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
		},
		"Wrong Extension with correct source and type": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{
					Attributes: map[string]string{
						"type":        eventType,
						"source":      eventSource,
						extensionName: "some-other-extension-value"},
				})),
			},
			event: makeEventWithExtension(extensionName, extensionValue),
		},
		"Returned Cloud Event": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedResponseEvent:       makeDifferentEvent(),
		},
		"Error From Trigger": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			event:                       makeEvent(),
			requestFails:                true,
			failureStatus:               http.StatusTooManyRequests,
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedStatus:              http.StatusTooManyRequests,
		},
		"Returned Cloud Event with custom headers": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			request: func() *http.Request {
				e := makeEvent()
				b, _ := e.MarshalJSON()
				request := httptest.NewRequest(http.MethodPost, validPath, bytes.NewBuffer(b))

				// foo won't pass filtering.
				request.Header.Set("foo", "bar")
				// Traceparent will not pass filtering.
				request.Header.Set("Traceparent", "0")
				// Knative-Foo will pass as a prefix match.
				request.Header.Set("Knative-Foo", "baz")
				// X-B3-Foo will pass as a prefix match.
				request.Header.Set("X-B3-Foo", "bing")
				// X-Request-Id will pass as an exact header match.
				request.Header.Set("X-Request-Id", "123")
				// Content-Type will not pass filtering.
				request.Header.Set(cehttp.ContentType, event.ApplicationCloudEventsJSON)

				return request
			}(),
			expectedHeaders: http.Header{
				// X-Request-Id will pass as an exact header match.
				"X-Request-Id": []string{"123"},
				// Knative-Foo will pass as a prefix match.
				"Knative-Foo": []string{"baz"},
				// X-B3-Foo will pass as a prefix match.
				"X-B3-Foo": []string{"bing"},
				// Prefer: reply will be added for every request as defined in the spec.
				"Prefer": []string{"reply"},
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedResponseEvent:       makeDifferentEvent(),
		},
		"Maintain `Prefer: reply` header when it is provided in the original request": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			request: func() *http.Request {
				e := makeEvent()
				b, _ := e.MarshalJSON()
				request := httptest.NewRequest(http.MethodPost, validPath, bytes.NewBuffer(b))
				// Following the spec (https://github.com/knative/specs/blob/main/specs/eventing/data-plane.md#derived-reply-events)
				//   this header should be present even if it is provided in the original request
				request.Header.Set("Prefer", "reply")
				// Content-Type to pass filtering.
				request.Header.Set(cehttp.ContentType, event.ApplicationCloudEventsJSON)

				return request
			}(),
			expectedHeaders: http.Header{
				// Prefer: reply must be present, even if it is provided in the original request
				"Prefer": []string{"reply"},
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedResponseEvent:       makeDifferentEvent(),
		},
		"Add `Prefer: reply` header when it isn't provided in the original request": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			request: func() *http.Request {
				e := makeEvent()
				b, _ := e.MarshalJSON()
				request := httptest.NewRequest(http.MethodPost, validPath, bytes.NewBuffer(b))
				request.Header.Set(cehttp.ContentType, event.ApplicationCloudEventsJSON)

				return request
			}(),
			expectedHeaders: http.Header{
				// Prefer: reply must be present, even if it is provided in the original request
				"Prefer": []string{"reply"},
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedResponseEvent:       makeDifferentEvent(),
		},
		"Returned non empty non event expectedResponse": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedStatus:              http.StatusBadGateway,
			expectedResponse:            makeNonEmptyResponse(),
		},
		"Returned malformed Cloud Event": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedStatus:              http.StatusOK,
			expectedResponse:            makeMalformedEventResponse(),
		},
		"Returned malformed structured Cloud Event": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedStatus:              http.StatusBadGateway,
			expectedResponse:            makeMalformedStructuredEventResponse(),
		},
		"Returned empty body 200": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedStatus:              http.StatusOK,
			expectedResponse:            makeEmptyResponse(200),
		},
		"Returned empty body 202": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedStatus:              http.StatusAccepted,
			expectedResponse:            makeEmptyResponse(202),
		},
		"Proxy allowed empty non event response headers": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedStatus:              http.StatusTooManyRequests,
			expectedResponse:            makeEmptyResponse(http.StatusTooManyRequests),
			additionalReplyHeaders:      http.Header{"Retry-After": []string{"10"}},
			expectedResponseHeaders:     http.Header{"Retry-After": []string{"10"}},
		},
		"Do not proxy disallowed response headers": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withAttributesFilter(&eventingv1.TriggerFilter{})),
			},
			expectedDispatch:            true,
			expectedEventDispatchTime:   true,
			expectedEventProcessingTime: true,
			expectedResponseEvent:       makeDifferentEvent(),
			additionalReplyHeaders:      http.Header{"Retry-After": []string{"10"}, "Test-Header": []string{"TestValue"}},
			expectedResponseHeaders:     http.Header{"Retry-After": []string{"10"}},
		},
	}
	for n, tc := range testCases {
		t.Run(n, func(t *testing.T) {
			ctx, _ := reconcilertesting.SetupFakeContext(t, SetUpInformerSelector)
			ctx = feature.ToContext(ctx, feature.Flags{
				feature.OIDCAuthentication: feature.Enabled,
			})
			trustBundleConfigMapLister := filteredconfigmapinformer.Get(ctx, eventingtls.TrustBundleLabelSelector).Lister().ConfigMaps(system.Namespace())

			fh := fakeHandler{
				failRequest:            tc.requestFails,
				failStatus:             tc.failureStatus,
				expectedResponseEvent:  tc.expectedResponseEvent,
				expectedRequestHeaders: tc.expectedHeaders,
				t:                      t,
				expectedResponse:       tc.expectedResponse,
				additionalReplyHeaders: tc.additionalReplyHeaders,
			}
			s := httptest.NewServer(&fh)
			defer s.Close()

			reader := metric.NewManualReader()
			mp := metric.NewMeterProvider(metric.WithReader(reader))

			exporter := tracetest.NewInMemoryExporter()
			tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
			otel.SetTextMapPropagator(propagation.TraceContext{})

			logger := zaptest.NewLogger(t, zaptest.WrapOptions(zap.AddCaller()))
			oidcTokenProvider := auth.NewOIDCTokenProvider(ctx)
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

			for _, trig := range tc.triggers {
				// Replace the SubscriberURI to point at our fake server.
				if trig.Status.SubscriberURI != nil && trig.Status.SubscriberURI.String() == toBeReplaced {

					url, err := apis.ParseURL(s.URL)
					if err != nil {
						t.Fatalf("Failed to parse URL %q : %s", s.URL, err)
					}
					trig.Status.SubscriberURI = url
				}
				triggerinformerfake.Get(ctx).Informer().GetStore().Add(trig)

				// create needed triggers subscription object
				sub := &messagingv1.Subscription{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resources.SubscriptionName(feature.FromContext(ctx), trig),
						Namespace: trig.Namespace,
					},
				}
				subscriptioninformerfake.Get(ctx).Informer().GetStore().Add(sub)

				// create the needed broker object
				b := &v1.Broker{
					ObjectMeta: metav1.ObjectMeta{
						Name:      trig.Spec.Broker,
						Namespace: trig.Namespace,
					},
				}
				brokerinformerfake.Get(ctx).Informer().GetStore().Add(b)
			}

			r, err := NewHandler(
				logger,
				authVerifier,
				oidcTokenProvider,
				triggerinformerfake.Get(ctx),
				brokerinformerfake.Get(ctx),
				subscriptioninformerfake.Get(ctx),
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

			e := tc.event
			if e == nil {
				e = makeEvent()
			}
			if tc.request == nil {
				b, err := e.MarshalJSON()
				if err != nil {
					t.Fatal(err)
				}
				tc.request = httptest.NewRequest(http.MethodPost, validPath, bytes.NewBuffer(b))
				tc.request.Header.Set(cehttp.ContentType, event.ApplicationCloudEventsJSON)
			}
			responseWriter := httptest.NewRecorder()
			r.ServeHTTP(&responseWriterWithInvocationsCheck{
				ResponseWriter: responseWriter,
				headersWritten: atomic.NewBool(false),
				t:              t,
			}, tc.request)

			response := responseWriter.Result()

			if tc.expectedStatus != http.StatusInternalServerError && tc.expectedStatus != http.StatusBadGateway {
				for expectedHeaderKey, expectedHeaderValues := range tc.expectedResponseHeaders {
					if response.Header[expectedHeaderKey] == nil || response.Header[expectedHeaderKey][0] != expectedHeaderValues[0] {
						t.Errorf("Response header proxy failed for header '%v'. Expected %v, Actual %v", expectedHeaderKey, expectedHeaderValues[0], response.Header[expectedHeaderKey])
					}
				}
			}

			if tc.expectedStatus != 0 && tc.expectedStatus != response.StatusCode {
				t.Errorf("Unexpected status. Expected %v. Actual %v.", tc.expectedStatus, response.StatusCode)
			}
			if tc.expectedDispatch != fh.requestReceived {
				t.Errorf("Incorrect dispatch. Expected %v, Actual %v", tc.expectedDispatch, fh.requestReceived)
			}

			expectedMetricNames := []string{}
			if tc.expectedEventDispatchTime {
				expectedMetricNames = append(expectedMetricNames, "kn.eventing.dispatch.duration")
			}
			if tc.expectedEventProcessingTime {
				expectedMetricNames = append(expectedMetricNames, "kn.eventing.process.duration")
			}
			if len(expectedMetricNames) > 0 {
				metricstest.AssertMetrics(t, reader, metricstest.MetricsPresent(ScopeName, expectedMetricNames...))
			}

			if tc.expectedResponseEvent != nil {
				if tc.expectedResponseEvent.SpecVersion() != event.CloudEventsVersionV1 {
					t.Errorf("Incorrect spec version. Expected %v, Actual %v", tc.expectedResponseEvent.SpecVersion(), event.CloudEventsVersionV1)
				}
			}
			// Compare the returned event.
			message := cehttp.NewMessageFromHttpResponse(response)
			event, err := binding.ToEvent(context.Background(), message)
			if tc.expectedResponseEvent == nil {
				if err == nil || event != nil {
					t.Fatal("Unexpected response event:", event)
				}
				return
			}
			if err != nil || event == nil {
				t.Fatalf("Expected response event, actually nil (err: %+v)", err)
			}

			// The TTL will be added again.
			expectedResponseEvent := addTTLToEvent(*tc.expectedResponseEvent)

			// cloudevents/sdk-go doesn't preserve the extension type, so get TTL and set it back again.
			// https://github.com/cloudevents/sdk-go/blob/97abfeb3da0bed09e395bff2c5bcf35b6435cb5f/v2/types/value.go#L57
			ttl, err := broker.GetTTL(event.Context)
			if err != nil {
				t.Error("failed to get TTL", err)
			}
			err = broker.SetTTL(event.Context, ttl)
			if err != nil {
				t.Error("failed to set TTL", err)
			}

			if diff := cmp.Diff(expectedResponseEvent.Context.AsV1(), event.Context.AsV1()); diff != "" {
				t.Error("Incorrect response event context (-want +got):", diff)
			}
			if diff := cmp.Diff(expectedResponseEvent.Data(), event.Data()); diff != "" {
				t.Error("Incorrect response event data (-want +got):", diff)
			}
		})
	}
}

func TestReceiver_WithSubscriptionsAPI(t *testing.T) {
	testCases := map[string]struct {
		triggers                  []*eventingv1.Trigger
		event                     *cloudevents.Event
		expectedDispatch          bool
		expectedEventDispatchTime bool
		expectEventProcessTime    bool
	}{
		"Wrong source": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withSubscriptionAPIFilter(&eventingv1.SubscriptionsAPIFilter{
					Exact: map[string]string{"source": "some-other-source"},
				})),
			},
		},
		"Wrong extension": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withSubscriptionAPIFilter(&eventingv1.SubscriptionsAPIFilter{
					Exact: map[string]string{extensionName: "some-other-extension"},
				})),
			},
		},
		"Dispatch succeeded - Source with type": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(withSubscriptionAPIFilter(&eventingv1.SubscriptionsAPIFilter{
					CESQL: fmt.Sprintf("type = '%s' AND source = '%s'", eventType, eventSource),
				})),
			},
			expectedDispatch:          true,
			expectedEventDispatchTime: true,
			expectEventProcessTime:    true,
		},
		"Dispatch succeeded - SubscriptionsAPI filter overrides Attributes Filter": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(
					withSubscriptionAPIFilter(&eventingv1.SubscriptionsAPIFilter{
						CESQL: fmt.Sprintf("type = '%s' AND source = '%s'", eventType, eventSource),
					}),
					withAttributesFilter(&eventingv1.TriggerFilter{
						Attributes: map[string]string{"type": "some-other-type", "source": "some-other-source"},
					})),
			},
			expectedDispatch:          true,
			expectEventProcessTime:    true,
			expectedEventDispatchTime: true,
		},
		"Dispatch failed - empty SubscriptionsAPI filter does not override Attributes Filter": {
			triggers: []*eventingv1.Trigger{
				makeTrigger(
					withAttributesFilter(&eventingv1.TriggerFilter{
						Attributes: map[string]string{"type": "some-other-type", "source": "some-other-source"},
					})),
			},
		},
	}
	for n, tc := range testCases {
		t.Run(n, func(t *testing.T) {
			ctx, _ := reconcilertesting.SetupFakeContext(t, SetUpInformerSelector)
			trustBundleConfigMapLister := filteredconfigmapinformer.Get(ctx, eventingtls.TrustBundleLabelSelector).Lister().ConfigMaps(system.Namespace())

			fh := fakeHandler{
				t: t,
			}
			s := httptest.NewServer(&fh)
			defer s.Close()

			filtersMap := subscriptionsapi.NewFiltersMap()

			reader := metric.NewManualReader()
			mp := metric.NewMeterProvider(metric.WithReader(reader))

			exporter := tracetest.NewInMemoryExporter()
			tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
			otel.SetTextMapPropagator(propagation.TraceContext{})

			logger := zaptest.NewLogger(t, zaptest.WrapOptions(zap.AddCaller()))
			oidcTokenProvider := auth.NewOIDCTokenProvider(ctx)
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

			// Replace the SubscriberURI to point at our fake server.
			for _, trig := range tc.triggers {
				if trig.Status.SubscriberURI != nil && trig.Status.SubscriberURI.String() == toBeReplaced {

					url, err := apis.ParseURL(s.URL)
					if err != nil {
						t.Fatalf("Failed to parse URL %q : %s", s.URL, err)
					}
					trig.Status.SubscriberURI = url
				}
				triggerinformerfake.Get(ctx).Informer().GetStore().Add(trig)
				filtersMap.Set(trig, subscriptionsapi.CreateSubscriptionsAPIFilters(logging.FromContext(ctx).Desugar(), trig.Spec.Filters))

				// create needed triggers subscription object
				sub := &messagingv1.Subscription{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resources.SubscriptionName(feature.FromContext(ctx), trig),
						Namespace: trig.Namespace,
					},
				}
				subscriptioninformerfake.Get(ctx).Informer().GetStore().Add(sub)

				// create the needed broker object
				b := &v1.Broker{
					ObjectMeta: metav1.ObjectMeta{
						Name:      trig.Spec.Broker,
						Namespace: trig.Namespace,
					},
				}
				brokerinformerfake.Get(ctx).Informer().GetStore().Add(b)
			}
			r, err := NewHandler(
				logger,
				authVerifier,
				oidcTokenProvider,
				triggerinformerfake.Get(ctx),
				brokerinformerfake.Get(ctx),
				subscriptioninformerfake.Get(ctx),
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

			r.filtersMap = filtersMap

			e := tc.event
			if e == nil {
				e = makeEvent()
			}
			b, err := e.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}

			request := httptest.NewRequest(http.MethodPost, validPath, bytes.NewBuffer(b))
			request.Header.Set(cehttp.ContentType, event.ApplicationCloudEventsJSON)
			responseWriter := httptest.NewRecorder()
			r.ServeHTTP(&responseWriterWithInvocationsCheck{
				ResponseWriter: responseWriter,
				headersWritten: atomic.NewBool(false),
				t:              t,
			}, request)

			response := responseWriter.Result()

			if tc.expectedDispatch != fh.requestReceived {
				t.Errorf("Incorrect dispatch. Expected %v, Actual %v", tc.expectedDispatch, fh.requestReceived)
			}

			expectedMetricNames := []string{}
			if tc.expectedEventDispatchTime {
				expectedMetricNames = append(expectedMetricNames, "kn.eventing.dispatch.duration")
			}
			if tc.expectEventProcessTime {
				expectedMetricNames = append(expectedMetricNames, "kn.eventing.process.duration")
			}
			if len(expectedMetricNames) > 0 {
				metricstest.AssertMetrics(t, reader, metricstest.MetricsPresent(ScopeName, expectedMetricNames...))
			}
			// Compare the returned event.
			message := cehttp.NewMessageFromHttpResponse(response)
			event, err := binding.ToEvent(context.Background(), message)
			if err == nil || event != nil {
				t.Fatal("Unexpected response event:", event)
			}
		})
	}
}

func withSubscriptionAPIFilter(filter *eventingv1.SubscriptionsAPIFilter) TriggerOption {
	return func(trigger *eventingv1.Trigger) {
		trigger.Spec.Filters = []eventingv1.SubscriptionsAPIFilter{
			*filter,
		}
	}
}

type responseWriterWithInvocationsCheck struct {
	http.ResponseWriter
	headersWritten *atomic.Bool
	t              *testing.T
}

func (r *responseWriterWithInvocationsCheck) WriteHeader(statusCode int) {
	if !r.headersWritten.CAS(false, true) {
		r.t.Fatal("WriteHeader invoked more than once")
	}
	r.ResponseWriter.WriteHeader(statusCode)
}

type fakeHandler struct {
	t *testing.T

	// input
	failRequest            bool
	failStatus             int
	additionalReplyHeaders http.Header

	// expectations
	expectedRequestHeaders http.Header
	expectedResponseEvent  *cloudevents.Event
	expectedResponse       *http.Response

	// results
	requestReceived bool
}

func (h *fakeHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	if h.expectedResponseEvent != nil && h.expectedResponse != nil {
		h.t.Errorf("Can not specify both expectedResponseEvent and expectedResponse.")
	}
	h.requestReceived = true

	for n, v := range h.expectedRequestHeaders {
		if strings.Contains(strings.ToLower(n), strings.ToLower(broker.TTLAttribute)) {
			h.t.Errorf("Broker TTL should not be seen by the subscriber: %s", n)
		}
		if diff := cmp.Diff(v, req.Header[n]); diff != "" {
			h.t.Errorf("Incorrect request header '%s' (-want +got): %s", n, diff)
		}
	}

	if h.failRequest {
		if h.failStatus != 0 {
			resp.WriteHeader(h.failStatus)
		} else {
			resp.WriteHeader(http.StatusBadRequest)
		}
		return
	}
	if h.expectedResponseEvent == nil && h.expectedResponse == nil {
		resp.WriteHeader(http.StatusAccepted)
		return
	}

	if h.expectedResponseEvent != nil {
		message := binding.ToMessage(h.expectedResponseEvent)
		defer message.Finish(nil)
		for k, v := range h.additionalReplyHeaders {
			resp.Header().Set(k, v[0])
		}
		err := cehttp.WriteResponseWriter(context.Background(), message, http.StatusAccepted, resp)
		if err != nil {
			h.t.Fatalf("Unable to write body: %v", err)
		}
	}
	if h.expectedResponse != nil {
		for k, v := range h.expectedResponse.Header {
			resp.Header().Set(k, v[0])
		}
		for k, v := range h.additionalReplyHeaders {
			resp.Header().Add(k, v[0])
		}
		resp.WriteHeader(h.expectedResponse.StatusCode)
		if h.expectedResponse.Body != nil {
			defer h.expectedResponse.Body.Close()
			body, err := io.ReadAll(h.expectedResponse.Body)
			if err != nil {
				h.t.Fatal("Unable to read body: ", err)
			}
			resp.Write(body)
		}
	}
}

func makeTrigger(options ...TriggerOption) *eventingv1.Trigger {
	t := &eventingv1.Trigger{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "eventing.knative.dev/v1",
			Kind:       "Trigger",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNS,
			Name:      triggerName,
			UID:       triggerUID,
		},
		Spec: eventingv1.TriggerSpec{},
		Status: eventingv1.TriggerStatus{
			SubscriberURI: &apis.URL{Host: "toBeReplaced"},
		},
	}
	for _, opt := range options {
		opt(t)
	}
	return t
}

func withUID(uid string) TriggerOption {
	return func(t *eventingv1.Trigger) {
		t.ObjectMeta.UID = types.UID(uid)
	}
}

func withAttributesFilter(filter *eventingv1.TriggerFilter) TriggerOption {
	return func(t *eventingv1.Trigger) {
		t.Spec.Filter = filter
	}
}

func withoutSubscriberURI() TriggerOption {
	return func(t *eventingv1.Trigger) {
		t.Status.SubscriberURI = nil
	}
}

func makeEventWithoutTTL() *cloudevents.Event {
	e := event.New()
	e.SetType(eventType)
	e.SetSource(eventSource)
	e.SetID("1234")
	return &e
}

func makeEvent() *cloudevents.Event {
	noTTL := makeEventWithoutTTL()
	e := addTTLToEvent(*noTTL)
	return &e
}

func addTTLToEvent(e cloudevents.Event) cloudevents.Event {
	_ = broker.SetTTL(e.Context, 1)
	return e
}

func makeDifferentEvent() *cloudevents.Event {
	e := makeEvent()
	e.SetSource("another-source")
	e.SetID("another-id")
	return e
}

func makeEventWithExtension(extName, extValue string) *cloudevents.Event {
	noTTL := makeEvent()
	noTTL.SetExtension(extName, extValue)
	e := addTTLToEvent(*noTTL)
	return &e
}

func makeNonEmptyResponse() *http.Response {
	r := &http.Response{
		Status:     "200 OK",
		StatusCode: 200,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Body:       io.NopCloser(bytes.NewBufferString(invalidEvent)),
		Header:     make(http.Header),
	}
	r.Header.Set("Content-Type", "garbage")
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(invalidEvent)))
	return r
}

func makeMalformedEventResponse() *http.Response {
	r := &http.Response{
		Status:     "200 OK",
		StatusCode: 200,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	r.Header.Set("Ce-Specversion", "9000.1")
	return r
}

func makeMalformedStructuredEventResponse() *http.Response {
	r := &http.Response{
		Status:     "200 OK",
		StatusCode: 200,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
		Header:     make(http.Header),
	}
	r.Header.Set("Content-Type", cloudevents.ApplicationCloudEventsJSON)

	return r
}

func makeEmptyResponse(status int) *http.Response {
	s := fmt.Sprintf("%d OK", status)
	r := &http.Response{
		Status:     s,
		StatusCode: status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	return r
}

func SetUpInformerSelector(ctx context.Context) context.Context {
	ctx = filteredFactory.WithSelectors(ctx, eventingtls.TrustBundleLabelSelector)
	return ctx
}
