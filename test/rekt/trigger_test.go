//go:build e2e
// +build e2e

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

package rekt

import (
	"testing"

	"knative.dev/pkg/system"
	_ "knative.dev/pkg/system/testing"
	"knative.dev/reconciler-test/pkg/environment"
	"knative.dev/reconciler-test/pkg/eventshub"
	"knative.dev/reconciler-test/pkg/k8s"
	"knative.dev/reconciler-test/pkg/knative"

	"knative.dev/eventing/test/rekt/features/trigger"
)

func TestTriggerDefaulting(t *testing.T) {
	t.Parallel()

	ctx, env := global.Environment(environment.Managed(t))

	env.TestSet(ctx, t, trigger.Defaulting())

	env.Finish()
}

func TestTriggerWithDLS(t *testing.T) {
	ctx, env := global.Environment(
		knative.WithKnativeNamespace(system.Namespace()),
		knative.WithLoggingConfig,
		knative.WithTracingConfig,
		k8s.WithEventListener,
		environment.Managed(t),
	)

	env.Test(ctx, t, trigger.SourceToTriggerSinkWithDLS())
	env.Test(ctx, t, trigger.SourceToTriggerSinkWithDLSDontUseBrokers())
}

func TestMultiTriggerTopology(t *testing.T) {
	ctx, env := global.Environment(
		knative.WithKnativeNamespace(system.Namespace()),
		knative.WithLoggingConfig,
		knative.WithTracingConfig,
		k8s.WithEventListener,
		environment.Managed(t),
	)

	// Test that a bad Trigger doesn't affect sending messages to a valid one
	env.Test(ctx, t, trigger.BadTriggerDoesNotAffectOkTrigger())
}

func TestTriggerDependencyAnnotation(t *testing.T) {
	ctx, env := global.Environment(
		knative.WithKnativeNamespace(system.Namespace()),
		knative.WithLoggingConfig,
		knative.WithTracingConfig,
		k8s.WithEventListener,
		environment.Managed(t),
	)

	// Test that a bad Trigger doesn't affect sending messages to a valid one
	env.Test(ctx, t, trigger.TriggerDependencyAnnotation())
}

func TestTriggerDeliveryFormat(t *testing.T) {
	ctx, env := global.Environment(
		knative.WithKnativeNamespace(system.Namespace()),
		knative.WithLoggingConfig,
		knative.WithTracingConfig,
		k8s.WithEventListener,
		environment.Managed(t),
	)

	env.TestSet(ctx, t, trigger.TriggerSupportsDeliveryFormat())
}

func TestTriggerTLSSubscriber(t *testing.T) {
	t.Parallel()

	ctx, env := global.Environment(
		knative.WithKnativeNamespace(system.Namespace()),
		knative.WithLoggingConfig,
		knative.WithTracingConfig,
		k8s.WithEventListener,
		environment.Managed(t),
		eventshub.WithTLS(t),
	)

	env.ParallelTest(ctx, t, trigger.TriggerWithTLSSubscriber())
	env.ParallelTest(ctx, t, trigger.TriggerWithTLSSubscriberTrustBundle())
	env.ParallelTest(ctx, t, trigger.TriggerWithTLSSubscriberWithAdditionalCATrustBundles())
}
