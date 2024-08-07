#!/usr/bin/env bash

# Copyright 2023 The Knative Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# This script builds and installs Knative Eventing components from source for local development.

set -e
set -o errexit
set -o nounset
set -o pipefail

go run "$(dirname "$0")/../test/version_check/check_k8s_version.go"
if [[ $? -ne 0 ]]; then
    echo "Kubernetes version check failed. Exiting."
    exit 1
fi

export SCALE_CHAOSDUCK_TO_ZERO=1
export REPLICAS=1

KO_ARCH=$(go env | grep GOARCH | awk -F\' '{print $2}')

export KO_FLAGS=${KO_FLAGS:-"--platform=linux/$KO_ARCH"}

source "$(dirname "$0")/../test/e2e-common.sh"

knative_setup || exit $?

test_setup || exit $?

if [[ ! -e $(dirname "$0")/../tmp ]]; then
    mkdir $(dirname "$0")/../tmp
fi
echo "${UNINSTALL_LIST[@]}" > $(dirname "$0")/../tmp/uninstall_list.txt
