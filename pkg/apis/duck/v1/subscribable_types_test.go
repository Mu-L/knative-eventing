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

package v1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"knative.dev/pkg/apis"
)

func TestSubscribableGetFullType(t *testing.T) {
	s := &Subscribable{}
	switch s.GetFullType().(type) {
	case *Subscribable:
		// expected
	default:
		t.Errorf("expected GetFullType to return *Subscribable, got %T", s.GetFullType())
	}
}

func TestSubscribableGetListType(t *testing.T) {
	c := &Subscribable{}
	switch c.GetListType().(type) {
	case *SubscribableList:
		// expected
	default:
		t.Errorf("expected GetListType to return *SubscribableList, got %T", c.GetListType())
	}
}

func TestSubscribablePopulate(t *testing.T) {
	got := &Subscribable{}

	want := &Subscribable{
		Spec: SubscribableSpec{
			Subscribers: []SubscriberSpec{{
				UID:           "2f9b5e8e-deb6-11e8-9f32-f2801f1b9fd1",
				Generation:    1,
				SubscriberURI: apis.HTTP("call1"),
				ReplyURI:      apis.HTTP("sink2"),
			}, {
				UID:           "34c5aec8-deb6-11e8-9f32-f2801f1b9fd1",
				Generation:    2,
				SubscriberURI: apis.HTTP("call2"),
				ReplyURI:      apis.HTTP("sink2"),
			}},
		},
		Status: SubscribableStatus{
			// Populate ALL fields
			Subscribers: []SubscriberStatus{{
				UID:                "2f9b5e8e-deb6-11e8-9f32-f2801f1b9fd1",
				ObservedGeneration: 1,
				Ready:              corev1.ConditionTrue,
				Message:            "Some message",
			}, {
				UID:                "34c5aec8-deb6-11e8-9f32-f2801f1b9fd1",
				ObservedGeneration: 2,
				Ready:              corev1.ConditionFalse,
				Message:            "Some message",
			}},
		},
	}

	got.Populate()

	if diff := cmp.Diff(want, got); diff != "" {
		t.Error("Unexpected difference (-want, +got):", diff)
	}

}
