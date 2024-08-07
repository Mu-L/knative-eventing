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

package mtping

import (
	"context"
	"fmt"

	"testing"

	. "knative.dev/pkg/reconciler/testing"

	"knative.dev/eventing/pkg/adapter/v2"
	// Fake injection informers
	_ "knative.dev/eventing/pkg/client/injection/informers/sources/v1/pingsource/fake"

	sourcesv1 "knative.dev/eventing/pkg/apis/sources/v1"
)

var removePingsource map[string]bool

type testAdapter struct {
	adapter.Adapter
}

func (testAdapter) Update(context.Context, *sourcesv1.PingSource) {
}

func (testAdapter) Remove(p *sourcesv1.PingSource) {
	if removePingsource == nil {
		removePingsource = make(map[string]bool)
	}
	removePingsource[fmt.Sprintf("%s/%s", p.Namespace, p.Name)] = true
}

func (testAdapter) RemoveAll(context.Context) {
}

func TestNew(t *testing.T) {
	ctx, _ := SetupFakeContext(t)

	if c := NewController(ctx, testAdapter{}); c == nil {
		t.Fatal("Expected NewController to return a non-nil value")
	}
}
