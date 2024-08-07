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

package flags

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Sources holds the Sources we want to run test against.
type Sources []metav1.TypeMeta

func (sources *Sources) String() string {
	return fmt.Sprint(*sources)
}

// Set appends the input string to Sources.
func (sources *Sources) Set(value string) error {
	*sources = csvToObjects(value, isValidSource)
	return nil
}

// Check if the Source kind is valid.
func isValidSource(source string) bool {
	return strings.HasSuffix(source, "Source")
}
