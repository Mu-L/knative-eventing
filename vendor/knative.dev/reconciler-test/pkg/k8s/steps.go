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

package k8s

import (
	"context"
	"fmt"
	"time"

	"knative.dev/pkg/apis"
	"knative.dev/pkg/network"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"

	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/injection/clients/dynamicclient"

	"knative.dev/reconciler-test/pkg/environment"
	"knative.dev/reconciler-test/pkg/feature"
)

// IsReady returns a reusable feature.StepFn to assert if a resource is ready
// within the time given. Timing is optional but if provided is [interval, timeout].
func IsReady(gvr schema.GroupVersionResource, name string, timing ...time.Duration) feature.StepFn {
	return func(ctx context.Context, t feature.T) {
		interval, timeout := PollTimings(ctx, timing)
		env := environment.FromContext(ctx)
		if err := WaitForResourceReady(ctx, t, env.Namespace(), name, gvr, interval, timeout); err != nil {
			t.Error(gvr, "did not become ready,", err)
		}
	}
}

// IsNotReady returns a reusable feature.StepFn to assert if a resource is not ready
// within the time given. Timing is optional but if provided is [interval, timeout].
func IsNotReady(gvr schema.GroupVersionResource, name string, timing ...time.Duration) feature.StepFn {
	return func(ctx context.Context, t feature.T) {
		interval, timeout := PollTimings(ctx, timing)
		env := environment.FromContext(ctx)
		if err := WaitForResourceNotReady(ctx, t, env.Namespace(), name, gvr, interval, timeout); err != nil {
			t.Error(gvr, "did become ready,", err)
		}
	}
}

// IsAddressable tests to see if a resource becomes Addressable within the time
// given. Timing is optional but if provided is [interval, timeout].
func IsAddressable(gvr schema.GroupVersionResource, name string, timing ...time.Duration) feature.StepFn {
	return func(ctx context.Context, t feature.T) {
		interval, timeout := PollTimings(ctx, timing)
		var lastError error
		err := wait.PollImmediate(interval, timeout, func() (bool, error) {
			addr, err := Address(ctx, gvr, name)
			if err != nil {
				lastError = err
				if apierrors.IsNotFound(err) {
					// keep polling
					return false, nil
				}
				return false, err
			}
			if addr == nil {
				lastError = fmt.Errorf("%s %s has no status.address.url %w", gvr, name, err)
				return false, nil
			}

			// Success!
			return true, nil
		})
		if err != nil {
			t.Fatalf("%s %s did not become addressable %v: %w", gvr, name, err, lastError)
		}
	}
}

// Address attempts to resolve an Addressable address into a URL. If the
// resource is found but not Addressable, Address will return (nil, nil).
func Address(ctx context.Context, gvr schema.GroupVersionResource, name string) (*duckv1.Addressable, error) {
	env := environment.FromContext(ctx)
	return NamespacedAddress(ctx, gvr, name, env.Namespace())
}

// NamespacedAddress attempts to resolve an Addressable address in a specific namespace into a URL. If the
// resource is found but not Addressable, Address will return (nil, nil).
func NamespacedAddress(ctx context.Context, gvr schema.GroupVersionResource, name, namespace string) (*duckv1.Addressable, error) {
	// Special case Service.
	if gvr.Group == "" && gvr.Version == "v1" && gvr.Resource == "services" {
		u := "http://" + network.GetServiceHostname(name, namespace)
		url, err := apis.ParseURL(u)
		return &duckv1.Addressable{URL: url}, err
	}

	like := &duckv1.AddressableType{}
	us, err := dynamicclient.Get(ctx).Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	obj := like.DeepCopy()
	if err = runtime.DefaultUnstructuredConverter.FromUnstructured(us.Object, obj); err != nil {
		return nil, fmt.Errorf("error from DefaultUnstructured.Dynamiconverter. %w", err)
	}
	obj.ResourceVersion = gvr.Version
	obj.APIVersion = gvr.GroupVersion().String()

	if obj.Status.Address == nil || obj.Status.Address.URL == nil {
		// Not Addressable (yet?).
		return nil, nil
	}

	// Success!
	return obj.Status.Address, nil
}
