#!/bin/bash
# Builds the patched x86_64 xgameruntime.dll for one domsvoboda/nexus-runtime
# release: upstream's XGameRuntime at the exact commit that release pins (its
# candidate.json sourceGraph), with images/library's patch series applied, built
# the way nexus-runtime builds it (CMake, MinGW, ENABLE_TESTS=OFF) against that
# release's Wine headers. The Library swaps it in for the games listed beside
# the patches (xodus-nexus-game-patches); everything else keeps upstream's.
#
# Usage: build.sh RUNTIME_TAG OUTDIR [--tests]
# Writes OUTDIR/xgameruntime.dll, OUTDIR/xgameruntime.dll.sha256 and
# OUTDIR/build.json. Exits 3 when the patches no longer apply (upstream changed
# that code: check whether it fixed the games, then drop or rebase the patch).
#
# --tests (CI on pull requests) also configures ENABLE_TESTS=ON, compiles every
# upstream test with the patches applied, and runs under Wine the gtest cases
# the patches add (each added TEST/TEST_F line), as upstream's
# scripts/test-component.mjs does. Without it a patch whose test hunk does not
# compile still built the DLL (XERK-2839). Only the patches' own cases run:
# under Ubuntu's Wine, unpatched upstream already fails other cases (e.g. 23
# NexusAuthentication ones in test_nexus_user, InitializeNetwork 0x80040154).
# Needs wine (and a display, e.g. xvfb-run) on PATH.
set -euo pipefail

tag="${1:?runtime tag}"
out="${2:?output directory}"
tests="${3:-}"
[[ -z "${tests}" || "${tests}" == --tests ]] || { echo "unknown option: ${tests}" >&2; exit 2; }
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
enable_tests=OFF target=(--target xgameruntime)
# Without --target: every upstream test executable, so any patched test file compiles.
[[ -n "${tests}" ]] && enable_tests=ON target=()
cmake -S "${work}/src" -B "${work}/build" -DCMAKE_TOOLCHAIN_FILE=win64-target.cmake \
  -DCMAKE_C_FLAGS="${flags}" -DCMAKE_CXX_FLAGS="${flags}" -DENABLE_TESTS="${enable_tests}" \
  -DNEXUS_WINE_SOURCE="${work}/wine" >"${work}/configure.log" 2>&1 || { cat "${work}/configure.log" >&2; exit 1; }
# -k: report every broken test file, not only the first.
cmake --build "${work}/build" --parallel "$(nproc)" "${target[@]}" -- -k >"${work}/build.log" 2>&1 ||
  { grep -E 'error|Error [0-9]' "${work}/build.log" | head -50 >&2; tail -20 "${work}/build.log" >&2; exit 1; }

dll="${work}/build/bin/xgameruntime.dll"
# A native module: a Wine builtin marker would make Wine load its own copy instead.
if head -c 96 "${dll}" | grep -aq 'Wine builtin DLL'; then echo "built a Wine builtin module" >&2; exit 1; fi

if [[ -n "${tests}" ]]; then
  # gtest cases the patches add, as Suite.Name (TEST_P names carry an
  # instantiation prefix: add an explicit list here if a patch ever uses one).
  mapfile -t cases < <(sed -nE 's/^\+[[:space:]]*TEST(_F)?[[:space:]]*\([[:space:]]*([A-Za-z0-9_]+)[[:space:]]*,[[:space:]]*([A-Za-z0-9_]+)[[:space:]]*\).*/\2.\3/p' \
    "${share}"/patches/*.patch | sort -u)
  if ((${#cases[@]})); then
    export WINEPREFIX="${work}/prefix" WINEDEBUG=-all WINEDLLOVERRIDES="xgameruntime=n;mscoree,mshtml="
    wineboot --init >"${work}/wineboot.log" 2>&1 || { cat "${work}/wineboot.log" >&2; exit 1; }
    wineserver -w
    # As upstream's test-component.mjs: the candidate DLL in the prefix's system32.
    cp "${dll}" "${WINEPREFIX}/drive_c/windows/system32/xgameruntime.dll"
    filter="$(IFS=:; echo "${cases[*]}")"
    : >"${work}/tests.log"
    for exe in "${work}"/build/bin/test_*.exe; do
      echo "== $(basename "${exe}")" >>"${work}/tests.log"
      # Not judged by exit status: filtered cases are, below.
      (cd "$(dirname "${exe}")" && timeout 600 wine "${exe}" --gtest_filter="${filter}") >>"${work}/tests.log" 2>&1 || true
    done
    wineserver -k || true
    failed=0
    for c in "${cases[@]}"; do
      if grep -qE "^\[ +OK \] ${c//./\\.}( |$)" "${work}/tests.log"; then echo "passed ${c}"
      else echo "patch test ${c} did not pass" >&2; failed=1; fi
    done
    ((failed == 0)) || { cat "${work}/tests.log" >&2; exit 1; }
  else
    echo "the patches add no gtest cases: compiled the tests, ran none"
  fi
fi

cp "${dll}" "${out}/xgameruntime.dll"
(cd "${out}" && sha256sum xgameruntime.dll >xgameruntime.dll.sha256)
python3 -I -c '
import json, sys
tag, xgr, wine, pid, out = sys.argv[1:]
json.dump({"schema": 1, "runtimeTag": tag, "xgameruntime": xgr, "wine": wine, "patchId": pid},
          open(out, "w"), indent=2)' "${tag}" "${xgr}" "${wine}" "${patch_id}" "${out}/build.json"
echo "built $(cut -d' ' -f1 "${out}/xgameruntime.dll.sha256") for ${tag}"
