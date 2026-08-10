#!/bin/bash
# hack/demo_rebalance.sh
#
# Guided, narrated demo of the rebalance engine's decision lifecycle:
#   Watching -> Triggered -> Evaluating -> Decided -> Enacting -> Watching
#
# Drives three beats against a live cluster (the full-stack deploy must
# already be running — see hack/deploy_full_stack.sh), all against the
# existing orchestrationprofile-2 / nginx-deployment-2 / eaoprofile-optional:
#   1. NoOp       — a normal cycle that decides nothing needs to change.
#   2. Retry      — a pod stuck Pending behind the energy gate gets retried
#                   once the energy window opens (RetryPendingSchedule).
#   3. Move       — an accepted Move recommendation evicts a pod and steers
#                   its replacement onto a specific node via the DecisionStore.
#
# Beat 3 needs the energy window CLOSED to fire the trigger (EnergyThreshold
# only matches on insufficient/Delayed/Waiting) but OPEN for the replacement
# pod to actually pass CheckEnergyGate and schedule — see the Move-vs-energy-
# gate interaction documented in internal/rebalance/README.md. This script
# handles that by polling for State: Enacting (eviction has happened, the
# replacement is about to be attempted) and opening the window at exactly
# that moment — moveEnactor's 60s timeout gives plenty of margin.
#
# This script does NOT stream logs itself — open these in separate terminal
# panes before you start, so you (the presenter) can narrate over them while
# this script drives the cluster:
#
#   Pane A — state machine, with its own lastTransitionAt timestamp and a
#   clear " | " delimiter between fields:
#     kubectl get orchestrationprofile orchestrationprofile-2 -w \
#       -o jsonpath='{.status.rebalancingStatus.lastTransitionAt}{" | "}{.status.rebalancingStatus.state}{" | "}{.status.rebalancingStatus.reason}{"\n"}'
#
#   Pane B — rebalance engine logs:
#     kubectl logs -f deployment/hiro-adaptive-orchestrator-controller-manager \
#       -n hiro-adaptive-orchestrator-system | grep -i --line-buffered rebalance
#
#   Pane C — decision-store hits (only fires during beat 3):
#     kubectl logs -f deployment/hiro-adaptive-orchestrator-controller-manager \
#       -n hiro-adaptive-orchestrator-system | grep -i --line-buffered "decision-store"
#
#   Pane D — the actual AI request/response bodies. The operator's own logs
#   (Pane B) only carry a structured summary (action/podName/targetNode/
#   improvement/reason) — the full request/response JSON is logged by the
#   mock agent itself, tagged kind=initial-placement or kind=rebalance:
#     kubectl logs -f deployment/mock-decision-agent -n hiro-adaptive-orchestrator-system
#
# Usage:
#   hack/demo_rebalance.sh              # run all three beats, in order
#   hack/demo_rebalance.sh beat1        # run a single beat
#   hack/demo_rebalance.sh beat2
#   hack/demo_rebalance.sh beat3
#   hack/demo_rebalance.sh cleanup      # revert the mock agent + restore
#                                       # replica count, without running
#                                       # anything else
#
# Every beat pauses for Enter before continuing, so you control pacing while
# talking. Ctrl-C at any point still triggers cleanup (trap below).
#
# ─── Config (override via environment) ────────────────────────────────────
#   NAMESPACE       operator namespace       (default: hiro-adaptive-orchestrator-system)
#   APP_NAMESPACE   demo app namespace       (default: default)

set -euo pipefail

NAMESPACE=${NAMESPACE:-hiro-adaptive-orchestrator-system}
APP_NAMESPACE=${APP_NAMESPACE:-default}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MOCK_AGENT_YAML="$SCRIPT_DIR/mock_decision_agent.yaml"

PROFILE=orchestrationprofile-2
APP=nginx-deployment-2
EAO=eaoprofile-optional

# ─── Presentation helpers ──────────────────────────────────────────────────

step() { printf '\n\033[36m===>\033[0m %s\n' "$*"; }
narrate() { printf '\n\033[33m%s\033[0m\n' "$*"; }
pause() { read -rp $'\n\033[2mPress Enter to continue...\033[0m ' _ || true; }

# Echoes $@ (dimmed, "$ "-prefixed) before executing it, so the presenter and
# anyone reading captured output see exactly what ran against the cluster.
# The echo goes to stderr so `x=$(run kubectl get ...)` still works — only
# the wrapped command's own stdout is captured by callers doing command
# substitution.
run() {
  printf '\033[2m$ %s\033[0m\n' "$*" >&2
  "$@"
}

profile_state() {
  kubectl get orchestrationprofile "$PROFILE" -o jsonpath='{.status.rebalancingStatus.state}' 2>/dev/null
}

# Polls profile_state until it equals $1, or $2 seconds elapse (2s interval).
# Prints a running "." per poll so the presenter sees it's alive, not hung.
wait_for_state() {
  local want=$1 timeout=$2 waited=0
  while [ "$(profile_state)" != "$want" ]; do
    [ "$waited" -ge "$timeout" ] && { echo " timed out waiting for state=$want"; return 1; }
    printf '.'
    sleep 2
    waited=$((waited + 2))
  done
  echo " reached state=$want after ${waited}s"
}

# ─── Cleanup (always runs on exit, even Ctrl-C) ────────────────────────────

MOCK_AGENT_BUMPED=false
NGINX2_ORIGINAL_REPLICAS=""

cleanup() {
  step "Cleanup"

  if [ "$MOCK_AGENT_BUMPED" = true ]; then
    revert_mock_agent
  fi

  if [ -n "$NGINX2_ORIGINAL_REPLICAS" ]; then
    echo "  Restoring $APP to $NGINX2_ORIGINAL_REPLICAS replica(s)"
    run kubectl scale deployment "$APP" -n "$APP_NAMESPACE" --replicas="$NGINX2_ORIGINAL_REPLICAS"
  fi

  echo "  Done. Note: $EAO is managed by the external Energy Aware Orchestrator"
  echo "  (kopf-based) — any manual status patches this script made will be"
  echo "  overwritten by its own reconciliation on its normal schedule."
}
trap cleanup EXIT INT TERM

# ─── Mock agent tuning (beat 3 needs a guaranteed, above-threshold Move) ───

bump_mock_agent() {
  step "Forcing the mock agent to always recommend an above-threshold Move"
  sed -i.bak \
    -e 's/^    MOVE_PROBABILITY   = 0.0$/    MOVE_PROBABILITY   = 1.0/' \
    -e 's/^    IMPROVEMENT_MIN    = 10.0$/    IMPROVEMENT_MIN    = 30.0/' \
    "$MOCK_AGENT_YAML"
  rm -f "$MOCK_AGENT_YAML.bak"
  run kubectl apply -f "$MOCK_AGENT_YAML"
  run kubectl rollout status deployment/mock-decision-agent -n "$NAMESPACE" --timeout=120s
  MOCK_AGENT_BUMPED=true
}

revert_mock_agent() {
  echo "  Reverting mock agent to its default MOVE_PROBABILITY / IMPROVEMENT_MIN"
  sed -i.bak \
    -e 's/^    MOVE_PROBABILITY   = 1.0$/    MOVE_PROBABILITY   = 0.0/' \
    -e 's/^    IMPROVEMENT_MIN    = 30.0$/    IMPROVEMENT_MIN    = 10.0/' \
    "$MOCK_AGENT_YAML"
  rm -f "$MOCK_AGENT_YAML.bak"
  run kubectl apply -f "$MOCK_AGENT_YAML"
  run kubectl rollout status deployment/mock-decision-agent -n "$NAMESPACE" --timeout=120s
  MOCK_AGENT_BUMPED=false
}

clear_cooldown() {
  run kubectl patch orchestrationprofile "$PROFILE" --type=merge --subresource=status \
    -p '{"status":{"rebalancingStatus":{"cooldownUntil":null}}}' >/dev/null
}

close_energy_window() {
  run kubectl patch energyawareorchestration "$EAO" -n "$APP_NAMESPACE" --type=merge --subresource=status \
    -p '{"status":{"decision":{"action":"Waiting","reason":"demo: energy window closed"},"energyMetrics":{"sufficient":false}}}' >/dev/null
}

open_energy_window() {
  run kubectl patch energyawareorchestration "$EAO" -n "$APP_NAMESPACE" --type=merge --subresource=status \
    -p '{"status":{"decision":{"action":"DeployImmediately","reason":"demo: energy window open"},"energyMetrics":{"sufficient":true}}}' >/dev/null
}

# ─── Beat 1 — NoOp ──────────────────────────────────────────────────────────

beat1_noop() {
  step "Beat 1 — NoOp"
  narrate "Every terminal outcome — including NoOp — arms the profile's full
cooldownSeconds (see dispatch.go's dispatchNoOp), not just the 30s detection
tick. So this doesn't repeat on its own every 30s; each cycle below clears
cooldown by hand first, same as a real cooldown expiry would, then waits for
one full Watching -> Triggered -> Evaluating -> Watching cycle."

  local rounds=3
  for i in $(seq 1 "$rounds"); do
    step "NoOp cycle $i of $rounds"
    clear_cooldown
    narrate "Watch Pane A/B/D for this cycle to complete (<=30s for the next
detection tick, then near-instant once triggered)."
    pause
  done
}

# ─── Beat 2 — RetryPendingSchedule ──────────────────────────────────────────

beat2_retry_pending_schedule() {
  step "Beat 2 — RetryPendingSchedule"
  clear_cooldown

  NGINX2_ORIGINAL_REPLICAS=$(run kubectl get deployment "$APP" -n "$APP_NAMESPACE" \
    -o jsonpath='{.spec.replicas}')

  narrate "Scaling $APP up by one to force a pod Pending behind the energy gate."
  run kubectl scale deployment "$APP" -n "$APP_NAMESPACE" \
    --replicas=$((NGINX2_ORIGINAL_REPLICAS + 1))

  step "Waiting for the new pod to go Pending"
  local pending_pod="" waited=0
  while [ -z "$pending_pod" ] && [ "$waited" -lt 60 ]; do
    pending_pod=$(kubectl get pods -n "$APP_NAMESPACE" -l app=nginx-2 \
      --field-selector=status.phase=Pending -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    [ -z "$pending_pod" ] && { printf '.'; sleep 2; waited=$((waited + 2)); }
  done
  echo
  run kubectl get pods -n "$APP_NAMESPACE" -l app=nginx-2 -o wide
  pause

  narrate "The event-driven watch on the EAO fires Reconcile right when we
patch it — that part isn't the issue. But Reconcile checks cooldown before
it ever looks at the trigger (reconciler.go), and a NoOp cycle can slip in
and re-arm a fresh cooldownSeconds between our clear and our patch landing —
same fixed cooldown a NoOp gets as anything else. So instead of a one-shot
patch, this clears cooldown and re-asserts the open window every few seconds
until $pending_pod is actually retried. Each pair of commands below IS the
retry — the echoed patches double as the progress indicator."

  step "Re-asserting the open energy window until $pending_pod is retried"
  local retried=false
  waited=0
  while [ "$waited" -lt 90 ]; do
    clear_cooldown
    open_energy_window
    sleep 3
    waited=$((waited + 3))
    if ! kubectl get pod "$pending_pod" -n "$APP_NAMESPACE" >/dev/null 2>&1; then
      retried=true
      break
    fi
  done

  if [ "$retried" = true ]; then
    echo "  $pending_pod was deleted/replaced — RetryPendingSchedule fired after ${waited}s."
  else
    echo "  Timed out after ${waited}s without a retry — check Pane B for what happened."
  fi

  run kubectl get pods -n "$APP_NAMESPACE" -l app=nginx-2 -o wide
  pause
}

# ─── Beat 3 — Move ──────────────────────────────────────────────────────────

beat3_move() {
  step "Beat 3 — Move"
  clear_cooldown

  narrate "The trigger needs the energy window CLOSED to fire (EnergyThreshold
only matches on insufficient/Delayed/Waiting), but the replacement pod needs
it OPEN to actually schedule. This script closes it now, triggers the Move,
waits for eviction (State: Enacting), then flips the window open before
moveEnactor's 60s timeout — the same trick you'd do by hand, just timed
automatically."

  step "Closing the energy window and forcing a guaranteed Move"
  close_energy_window
  bump_mock_agent

  step "Waiting for the cycle to reach Enacting (eviction in progress)"
  printf '  '
  if ! wait_for_state Enacting 60; then
    echo "  Did not observe Enacting in time — check Pane B for what happened."
    return 1
  fi

  step "Eviction is in flight — opening the energy window now"
  open_energy_window

  narrate "Watch Pane C for \"decision-store hit, bypassing AI call\" — that's
the replacement pod's scheduling request being biased toward the Move's
target node instead of a fresh AI call."
  step "Watching $APP pods (Ctrl-C to stop watching once you see the move complete)"
  run kubectl get pods -n "$APP_NAMESPACE" -l app=nginx-2 -o wide --watch || true

  step "Final state"
  run kubectl describe orchestrationprofile "$PROFILE"
  pause
}

# ─── Main ───────────────────────────────────────────────────────────────────

usage() {
  echo "Usage: $0 [beat1|beat2|beat3|cleanup]"
  echo "  (no argument) runs beat1, beat2, beat3 in order"
}

main() {
  case "${1:-all}" in
    beat1) beat1_noop ;;
    beat2) beat2_retry_pending_schedule ;;
    beat3) beat3_move ;;
    cleanup) : ;; # cleanup runs unconditionally via the EXIT trap below
    all)
      beat1_noop
      beat2_retry_pending_schedule
      beat3_move
      ;;
    -h|--help) usage; trap - EXIT; exit 0 ;;
    *) usage >&2; trap - EXIT; exit 1 ;;
  esac
}

main "$@"
