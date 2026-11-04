#!/bin/bash
# Builds the patched x86_64 xgameruntime.dll for one domsvoboda/nexus-runtime
# release: upstream's XGameRuntime at the exact commit that release pins (its
# candidate.json sourceGraph), with images/library's patch series applied, built
# the way nexus-runtime builds it (CMake, MinGW, ENABLE_TESTS=OFF) against that
# release's Wine headers. The Library swaps it in for the games listed beside
# the patches (xodus-nexus-game-patches); everything else keeps upstream's.
#
# Usage: build.sh RUNTIME_TAG OUTDIR
# Writes OUTDIR/xgameruntime.dll, OUTDIR/xgameruntime.dll.sha256 and
# OUTDIR/build.json. Exits 3 when the patches no longer apply (upstream changed
# that code: check whether it fixed the games, then drop or rebase the patch).
set -euo pipefail

tag="${1:?runtime tag}"
out="${2:?output directory}"
[[ "${tag}" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bad runtime tag: ${tag}" >&2; exit 2; }
root="$(cd "$(dirname "$0")/../.." && pwd)"
share="${root}/images/library/root/usr/share/xodus-nexus/xgameruntime"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${out}"

patch_id="$("${root}/hack/nexus-xgameruntime/patch-id.sh" "${share}/patches")"

curl -fsSL --retry 3 -o "${work}/candidate.json" \
  "https://github.com/domsvoboda/nexus-runtime/releases/download/${tag}/candidate.json"
read -r xgr wine < <(python3 -I -c '
import json, re, sys
g = json.load(open(sys.argv[1]))["sourceGraph"]
x, w = g["xgameruntime"], g["wine"]
assert re.fullmatch("[0-9a-f]{40}", x) and re.fullmatch("[0-9a-f]{40}", w)
print(x, w)' "${work}/candidate.json")
echo "runtime ${tag}: xgameruntime ${xgr}, wine ${wine}, patches ${patch_id}"

# A full clone (the repo is small): --3way below needs the patches' preimage
# blobs locally when upstream has moved the surrounding lines; neither a shallow
# nor a blobless clone has them.
git clone -q --no-checkout https://github.com/domsvoboda/nexus-xgameruntime.git "${work}/src"
git -C "${work}/src" checkout -q --detach "${xgr}"
if ! git -C "${work}/src" -c user.name=ci -c user.email=ci@invalid am -q --3way "${share}"/patches/*.patch; then
  echo "the patch series no longer applies to nexus-xgameruntime ${xgr}" >&2
  exit 3
fi

git init -q "${work}/wine"
git -C "${work}/wine" sparse-checkout set --no-cone /include/ /libs/xml2/
git -C "${work}/wine" fetch -q --depth 1 --filter=blob:none https://github.com/domsvoboda/nexus-wine.git "${wine}"
git -C "${work}/wine" checkout -q --detach FETCH_HEAD

flags="-O2 -D_WIN32_WINNT=0x0A00"
cmake -S "${work}/src" -B "${work}/build" -DCMAKE_TOOLCHAIN_FILE=win64-target.cmake \
  -DCMAKE_C_FLAGS="${flags}" -DCMAKE_CXX_FLAGS="${flags}" -DENABLE_TESTS=OFF \
  -DNEXUS_WINE_SOURCE="${work}/wine" >"${work}/configure.log" 2>&1 || { cat "${work}/configure.log" >&2; exit 1; }
cmake --build "${work}/build" --parallel "$(nproc)" --target xgameruntime >"${work}/build.log" 2>&1 ||
  { tail -50 "${work}/build.log" >&2; exit 1; }

dll="${work}/build/bin/xgameruntime.dll"
# A native module: a Wine builtin marker would make Wine load its own copy instead.
if head -c 96 "${dll}" | grep -aq 'Wine builtin DLL'; then echo "built a Wine builtin module" >&2; exit 1; fi
cp "${dll}" "${out}/xgameruntime.dll"
(cd "${out}" && sha256sum xgameruntime.dll >xgameruntime.dll.sha256)
python3 -I -c '
import json, sys
tag, xgr, wine, pid, out = sys.argv[1:]
json.dump({"schema": 1, "runtimeTag": tag, "xgameruntime": xgr, "wine": wine, "patchId": pid},
          open(out, "w"), indent=2)' "${tag}" "${xgr}" "${wine}" "${patch_id}" "${out}/build.json"
echo "built $(cut -d' ' -f1 "${out}/xgameruntime.dll.sha256") for ${tag}"
