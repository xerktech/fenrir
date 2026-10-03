#!/usr/bin/env bash

# Copyright 2023 The Kubernetes Authors.
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

set -o errexit
set -o nounset
set -o pipefail


SCRIPT_ROOT=$( cd "$(dirname "${BASH_SOURCE[0]}")/.." ; pwd -P )
# go list -m prints an empty Dir unless the module is already in the module cache, and only
# hack/tools.go (behind the tools build tag) imports it, so a fresh clone or CI runner lacks it.
CODEGEN_PKG=${CODEGEN_PKG:-$(
  go mod download k8s.io/code-generator
  go list -m -f "{{.Dir}}" k8s.io/code-generator
)}

function codegen::join() {
  local IFS="$1"
  shift
  echo "$*"
}

PKG_NAME="games-on-whales.github.io/direwolf"
OUTPUT_PKG="pkg/generated"
BOILERPLATE="${SCRIPT_ROOT}"/hack/boilerplate.go.txt

source "${CODEGEN_PKG}"/kube_codegen.sh

# go get sigs.k8s.io/controller-tools/cmd/controller-gen
go install \
    sigs.k8s.io/controller-tools/cmd/controller-gen \
    k8s.io/code-generator/cmd/deepcopy-gen \
    k8s.io/code-generator/cmd/defaulter-gen \
    k8s.io/code-generator/cmd/register-gen \
    k8s.io/code-generator/cmd/applyconfiguration-gen \
    k8s.io/code-generator/cmd/client-gen \
    k8s.io/code-generator/cmd/lister-gen \
    k8s.io/code-generator/cmd/informer-gen


echo "Generating helpers..." >&2
kube::codegen::gen_helpers --boilerplate "$BOILERPLATE" "$SCRIPT_ROOT"

echo "Generating scheme registration..." >&2
kube::codegen::gen_register --boilerplate "$BOILERPLATE" "${SCRIPT_ROOT}"

echo "Generating clientset..." >&2
kube::codegen::gen_client \
  --with-watch \
  --with-applyconfig \
  --output-dir "${SCRIPT_ROOT}/${OUTPUT_PKG}"\
  --output-pkg "${PKG_NAME}/${OUTPUT_PKG}" \
  --boilerplate "$BOILERPLATE" \
  "${SCRIPT_ROOT}/pkg"

pushd "${SCRIPT_ROOT}" >/dev/null

# Generate CRD manifests for all types using controller-gen
echo "Generating crd manifests..." >&2
# Neither controller-gen nor openapi2jsonschema.py deletes outputs, so clear them first: a removed
# kind's CRD would otherwise keep shipping in the chart.
rm -f crds/*.yaml
rm -f schemas/*/*.json
go run sigs.k8s.io/controller-tools/cmd/controller-gen \
  crd:generateEmbeddedObjectMeta=true \
  paths="${PKG_NAME}/..." \
  output:dir="${SCRIPT_ROOT}/crds"
echo "CRD manifests generated" >&2

echo "Patching crd manifests..." >&2
# Generated CRDs cannot have the empty object defaults, overwriting afterwards
# go run github.com/mikefarah/yq/v4 eval ".spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.matchConstraints.properties.namespaceSelector.default = {}" "./crds/admissionregistration.x-k8s.io_validatingadmissionpolicies.yaml" -i
# go run github.com/mikefarah/yq/v4 eval ".spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.matchConstraints.properties.objectSelector.default = {}" "./crds/admissionregistration.x-k8s.io_validatingadmissionpolicies.yaml" -i
# go run github.com/mikefarah/yq/v4 eval ".spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.matchResources.properties.namespaceSelector.default = {}" "./crds/admissionregistration.x-k8s.io_validatingadmissionpolicybindings.yaml" -i
# go run github.com/mikefarah/yq/v4 eval ".spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.matchResources.properties.objectSelector.default = {}" "./crds/admissionregistration.x-k8s.io_validatingadmissionpolicybindings.yaml" -i

# Every `labels` map in the CRDs is an embedded ObjectMeta's (session pod, PVC). Give its values
# the API server's label-value rule, so a bad value fails on apply rather than on every Session
# (XERK-1375). controller-gen has no marker for ObjectMeta's fields, and a CEL rule over the map's
# unbounded values exceeds the CRD cost budget. Keys are checked by CEL markers in pkg/api.
python3 - crds/*.yaml <<'PY'
import re, sys
for path in sys.argv[1:]:
    with open(path) as f:
        src = f.read()
    out, n = re.subn(
        r"^( *)labels:\n\1  additionalProperties:\n\1    type: string\n",
        lambda m: (f"{m[1]}labels:\n{m[1]}  additionalProperties:\n"
                   f"{m[1]}    maxLength: 63\n"
                   f"{m[1]}    pattern: ^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$\n"
                   f"{m[1]}    type: string\n"),
        src, flags=re.M)
    # Fail rather than silently skip a labels map whose layout controller-gen changed.
    if n != len(re.findall(r"^ *labels:$", src, flags=re.M)):
        sys.exit(f"{path}: patched {n} labels maps; check the pattern against controller-gen's output")
    with open(path, "w") as f:
        f.write(out)
PY

echo "Done" >&2
popd >/dev/null

python3 hack/openapi2jsonschema.py crds/*
