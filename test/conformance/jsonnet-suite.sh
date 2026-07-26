#!/usr/bin/env bash
set -euo pipefail

: "${JSONNET_SUITE_DIR:?set JSONNET_SUITE_DIR to the google/jsonnet test_suite directory}"
: "${SONNETBOX_BIN:?set SONNETBOX_BIN to the Sonnetbox CLI}"
: "${GO_JSONNET_BIN:?set GO_JSONNET_BIN to the matching go-jsonnet CLI}"

suite_dir="$(cd "${JSONNET_SUITE_DIR}" && pwd)"
sonnetbox_bin="$(cd "$(dirname "${SONNETBOX_BIN}")" && pwd)/$(basename "${SONNETBOX_BIN}")"
go_jsonnet_bin="$(cd "$(dirname "${GO_JSONNET_BIN}")" && pwd)/$(basename "${GO_JSONNET_BIN}")"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

# expected_failure_exit reports the exit status Sonnetbox must use for a fixture
# the oracle rejects. Requiring a specific status stops an exhausted budget or a
# host fault from passing as if it were the Jsonnet error upstream reported.
#
# Every fixture defaults to 2, a static or runtime Jsonnet error. A fixture
# listed here diverges for a documented reason.
expected_failure_exit() {
  case "$1" in
  # An empty import path is rejected at the host ABI boundary before any
  # importer sees it, so it surfaces as a host failure rather than a Jsonnet
  # error.
  error.import_empty.jsonnet) printf '1' ;;
  # Security policy outranks compatibility: the backslash in this verbatim path
  # is denied by import policy, so the missing file is never reached.
  error.verbatim_import.jsonnet) printf '4' ;;
  *) printf '2' ;;
  esac
}

find "${suite_dir}" -maxdepth 1 -type f -name '*.jsonnet' -print |
  LC_ALL=C sort >"${work_dir}/fixtures"

fixture_count="$(wc -l <"${work_dir}/fixtures" | tr -d '[:space:]')"
if [[ "${fixture_count}" != 173 ]]; then
  echo "expected 173 Jsonnet fixtures, found ${fixture_count}" >&2
  exit 1
fi

successes=0
failures=0
mismatches=0

while IFS= read -r fixture; do
  name="$(basename "${fixture}")"
  bindings=(
    --ext-str var1=test
    --ext-code 'var2={x:1,y:2}'
  )
  if [[ "${name}" == tla.* ]]; then
    bindings=(
      --tla-str var1=test
      --tla-code 'var2={x:1,y:2}'
    )
  fi

  oracle_output="${work_dir}/oracle.out"
  subject_output="${work_dir}/subject.out"
  if (
    cd "${suite_dir}"
    "${go_jsonnet_bin}" "${bindings[@]}" "${name}"
  ) >"${oracle_output}" 2>&1; then
    oracle_status=0
  else
    oracle_status=$?
  fi

  if (
    cd "${suite_dir}"
    "${sonnetbox_bin}" \
      --cache-dir "${work_dir}/cache" \
      --root "${suite_dir}" \
      --timeout 30s \
      --max-memory 1GiB \
      --max-fuel 10000000000 \
      --max-stack 4096 \
      --max-wasm-stack 256MiB \
      "${bindings[@]}" \
      "${name}"
  ) >"${subject_output}" 2>&1; then
    subject_status=0
  else
    subject_status=$?
  fi

  if [[ "${oracle_status}" == 0 ]]; then
    successes=$((successes + 1))
    if [[ "${subject_status}" != 0 ]] || ! cmp -s "${oracle_output}" "${subject_output}"; then
      echo "FAIL ${name}: oracle succeeded, Sonnetbox status ${subject_status}" >&2
      diff -u "${oracle_output}" "${subject_output}" >&2 || true
      mismatches=$((mismatches + 1))
    fi
  else
    failures=$((failures + 1))
    want_status="$(expected_failure_exit "${name}")"
    if [[ "${subject_status}" != "${want_status}" ]]; then
      echo "FAIL ${name}: oracle failed with status ${oracle_status}," \
        "Sonnetbox exited ${subject_status}, want ${want_status}" >&2
      cat "${subject_output}" >&2
      mismatches=$((mismatches + 1))
    fi
  fi
done <"${work_dir}/fixtures"

echo "Jsonnet conformance: ${successes} matching successes," \
  "${failures} failures rejected with the expected status, ${mismatches} mismatches"
if [[ "${successes}" != 70 ]] || [[ "${failures}" != 103 ]] || [[ "${mismatches}" != 0 ]]; then
  exit 1
fi
