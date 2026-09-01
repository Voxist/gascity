# Canonical CPU-count detection, shared by the local test job sizing
# (scripts/test-local-job-count) and the push-gate slot default
# (scripts/push-gate-lock-lib.sh) so the two never drift.
#
# gc_detect_cpus honors the GC_TEST_LOCAL_CPUS pin (a host deliberately sized
# down for tests must present that size everywhere), then falls through
# nproc / getconf / sysctl, defaulting to 8 when nothing answers. An invalid
# pin returns 1 and prints nothing; callers own their error contract.
gc_detect_cpus() {
  if [[ -n "${GC_TEST_LOCAL_CPUS:-}" ]]; then
    [[ "$GC_TEST_LOCAL_CPUS" =~ ^[0-9]+$ && "$GC_TEST_LOCAL_CPUS" -gt 0 ]] || return 1
    printf '%s\n' "$GC_TEST_LOCAL_CPUS"
    return 0
  fi
  nproc 2>/dev/null ||
    getconf _NPROCESSORS_ONLN 2>/dev/null ||
    sysctl -n hw.ncpu 2>/dev/null ||
    printf '8\n'
}
