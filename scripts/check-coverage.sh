#!/usr/bin/env bash
#
# Coverage floors for the engine (M1.1-T4).
#
# Two thresholds, because one number cannot express the requirement:
#
#   * A GLOBAL floor keeps overall coverage from eroding commit by commit.
#   * A higher MONEY-PATH floor applies to the packages that decide balances.
#     Averaged into a single global figure, a collapse in domain/ or game/ can be
#     masked by well-tested code elsewhere — so those packages are also checked
#     individually, and a failure names the package.
#
# The global floor sits deliberately BELOW the money-path floor. cmd/engine is
# process wiring (~10%) and telemetry is exporter plumbing (~46%); demanding 80%
# of them would mean writing tests that assert nothing in order to colour a
# number. The aggregate absorbs them, and the packages that matter are pinned
# individually.
#
# Percentages are computed from STATEMENT COUNTS in the coverage profile, the same
# way `go tool cover` does. Averaging the per-function percentages that
# `go tool cover -func` prints would weight a one-line helper the same as a
# 200-statement transaction path and quietly report the wrong number.
set -euo pipefail

PROFILE="${1:-coverage.out}"
GLOBAL_FLOOR="${GLOBAL_FLOOR:-70.0}"
MONEY_FLOOR="${MONEY_FLOOR:-80.0}"

# The packages that compute, allocate, or move value. A money-path package that
# vanishes from the profile (every test deleted) FAILS rather than passing by
# absence — see the check below.
MONEY_PACKAGES=(
  "internal/domain"
  "internal/game"
  "internal/api"
  "internal/worker"
)

if [[ ! -f "$PROFILE" ]]; then
  echo "✖ coverage profile not found: $PROFILE" >&2
  echo "  Run: go test -count=1 -coverprofile=$PROFILE ./..." >&2
  exit 1
fi

MODULE="$(awk '/^module /{print $2; exit}' go.mod)"

# Emit "<package> <covered> <total>" per package, plus a TOTAL line.
readarray -t ROWS < <(
  awk -v module="$MODULE" '
    NR == 1 { next }                       # skip the "mode:" header
    {
      split($1, loc, ":")                  # loc[1] = full file path
      path = loc[1]
      sub("^" module "/", "", path)        # module-relative
      n = split(path, seg, "/")
      pkg = seg[1]
      for (i = 2; i < n; i++) pkg = pkg "/" seg[i]   # dirname => package

      stmts = $2 + 0
      count = $3 + 0

      total[pkg] += stmts
      grand_total += stmts
      if (count > 0) { covered[pkg] += stmts; grand_covered += stmts }
    }
    END {
      for (p in total) printf "%s %d %d\n", p, covered[p], total[p]
      printf "TOTAL %d %d\n", grand_covered, grand_total
    }
  ' "$PROFILE" | sort
)

declare -A PKG_PCT
GLOBAL_PCT="0.0"
for row in "${ROWS[@]}"; do
  read -r pkg covered total <<<"$row"
  pct=$(awk -v c="$covered" -v t="$total" 'BEGIN { printf "%.1f", (t == 0 ? 0 : c * 100 / t) }')
  if [[ "$pkg" == "TOTAL" ]]; then GLOBAL_PCT="$pct"; else PKG_PCT["$pkg"]="$pct"; fi
done

# `bc` is not guaranteed on a runner; awk is.
below() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a < b) }'; }

failures=()

echo "Coverage by package (money-path floor ${MONEY_FLOOR}%, global floor ${GLOBAL_FLOOR}%)"
echo
for pkg in $(printf '%s\n' "${!PKG_PCT[@]}" | sort); do
  pct="${PKG_PCT[$pkg]}"
  marker="     "
  is_money=false
  for m in "${MONEY_PACKAGES[@]}"; do [[ "$pkg" == "$m" ]] && is_money=true && break; done

  if $is_money; then
    if below "$pct" "$MONEY_FLOOR"; then
      marker="FAIL "
      failures+=("$pkg: ${pct}% is below the ${MONEY_FLOOR}% money-path floor")
    else
      marker="ok   "
    fi
    printf "  %s %-28s %6s%%   (money path)\n" "$marker" "$pkg" "$pct"
  else
    printf "  %s %-28s %6s%%\n" "$marker" "$pkg" "$pct"
  fi
done

# A money-path package absent from the profile has no tests at all. Silence here
# would be the worst outcome: the floor would "pass" precisely when coverage is zero.
for m in "${MONEY_PACKAGES[@]}"; do
  if [[ -z "${PKG_PCT[$m]:-}" ]]; then
    failures+=("$m: MISSING from the coverage profile — the package has no tests, or was renamed without updating MONEY_PACKAGES in this script")
  fi
done

echo
printf "  TOTAL %-28s %6s%%\n" "" "$GLOBAL_PCT"
if below "$GLOBAL_PCT" "$GLOBAL_FLOOR"; then
  failures+=("GLOBAL: ${GLOBAL_PCT}% is below the ${GLOBAL_FLOOR}% floor")
fi

echo
if (( ${#failures[@]} > 0 )); then
  echo "✖ COVERAGE GATE FAILED"
  echo
  for f in "${failures[@]}"; do echo "    • $f"; done
  echo
  echo "  Raise coverage on the named package(s). Do NOT lower a floor to get green:"
  echo "  the floors encode which code is allowed to be under-tested, and the money"
  echo "  path is not on that list."
  exit 1
fi

echo "✓ coverage gate passed"
