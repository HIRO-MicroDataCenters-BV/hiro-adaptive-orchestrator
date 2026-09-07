#!/bin/bash
# hack/demo_rebalance.sh
#
# Guided, narrated demo of the rebalance engine's decision lifecycle:
#   Watching -> Triggered -> Evaluating -> Decided -> Enacting -> Watching
#
# See internal/rebalance/README.md#demo-walkthrough for a small flow diagram
# of what each beat below actually does mechanically (Mermaid doesn't render
# in a .sh comment, so it lives there, not here).
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
# gate interaction documented in internal/rebalance/README.md. Same race as
# beat 2: a one-shot patch can lose to a NoOp re-arming cooldown or to the
# external EAO controller's own reconcile reverting it before our watch
# reacts. So this script re-asserts clear-cooldown+close-window every few
# seconds until the cycle leaves Watching, then re-asserts the open window
# throughout the whole Enacting phase instead of patching either side once.
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
#   hack/demo_rebalance.sh noop         # run a single beat
#   hack/demo_rebalance.sh retry
#   hack/demo_rebalance.sh move
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

# ─── Cleanup (always runs on exit, even Ctrl-C) ────────────────────────────

MOCK_AGENT_BUMPED=false
NGINX2_ORIGINAL_REPLICAS=""
RETRY_PENDING_POD=""
EAO_TOUCHED=false
EAO_ORIG_ACTION=""
EAO_ORIG_REASON=""
EAO_ORIG_SUFFICIENT=""

cleanup() {
  step "Cleanup"

  if [ "$MOCK_AGENT_BUMPED" = true ]; then
    revert_mock_agent
  fi

  if [ -n "$NGINX2_ORIGINAL_REPLICAS" ]; then
    echo "  Restoring $APP to $NGINX2_ORIGINAL_REPLICAS replica(s)"
    run kubectl scale deployment "$APP" -n "$APP_NAMESPACE" --replicas="$NGINX2_ORIGINAL_REPLICAS"
  fi

  if [ "$EAO_TOUCHED" = true ]; then
    revert_eao_window
  fi

  echo "  Done."
}
trap cleanup EXIT INT TERM

# ─── Mock agent tuning (beat 3 needs a guaranteed, above-threshold Move) ───

# Applies $MOCK_AGENT_YAML with a fresh redeployed-at timestamp piped in
# (never written to disk — same non-destructive trick deploy_full_stack.sh
# uses). Without this, kubectl apply alone sees no pod-template diff when
# only the ConfigMap-mounted script's constants changed, so it reports
# "deployment.apps/mock-decision-agent unchanged", the pod never restarts,
# and the running process keeps serving the OLD MOVE_PROBABILITY /
# IMPROVEMENT_MIN indefinitely — silently, since rollout status still
# reports success (there's nothing to roll out).
apply_mock_agent() {
  sed -e "s/REPLACE_DEPLOY_TIMESTAMP/$(date -u +%Y-%m-%dT%H:%M:%SZ)/g" \
    "$MOCK_AGENT_YAML" | run kubectl apply -f -
  run kubectl rollout status deployment/mock-decision-agent -n "$NAMESPACE" --timeout=120s
}

bump_mock_agent() {
  step "Forcing the mock agent to always recommend an above-threshold Move"
  sed -i.bak \
    -e 's/^    MOVE_PROBABILITY   = 0.0$/    MOVE_PROBABILITY   = 1.0/' \
    -e 's/^    IMPROVEMENT_MIN    = 10.0$/    IMPROVEMENT_MIN    = 30.0/' \
    "$MOCK_AGENT_YAML"
  rm -f "$MOCK_AGENT_YAML.bak"
  apply_mock_agent
  MOCK_AGENT_BUMPED=true
}

revert_mock_agent() {
  echo "  Reverting mock agent to its default MOVE_PROBABILITY / IMPROVEMENT_MIN"
  sed -i.bak \
    -e 's/^    MOVE_PROBABILITY   = 1.0$/    MOVE_PROBABILITY   = 0.0/' \
    -e 's/^    IMPROVEMENT_MIN    = 30.0$/    IMPROVEMENT_MIN    = 10.0/' \
    "$MOCK_AGENT_YAML"
  rm -f "$MOCK_AGENT_YAML.bak"
  apply_mock_agent
  MOCK_AGENT_BUMPED=false
}

clear_cooldown() {
  run kubectl patch orchestrationprofile "$PROFILE" --type=merge --subresource=status \
    -p '{"status":{"rebalancingStatus":{"cooldownUntil":null}}}' >/dev/null
}

# $EAO is owned by the external, kopf-based Energy Aware Orchestrator — our
# window patches below are a demo-only override. Its own reconcile would
# eventually overwrite them, but on its own schedule (minutes), not ours. So
# we capture its real pre-demo window state here, once, the first time we're
# about to touch it — then cleanup() restores exactly that instead of
# waiting the external controller out. Empty capture means the field was
# genuinely unset (not just empty string), so revert_eao_window patches it
# back with JSON null (merge-patch semantics: null removes the key) rather
# than writing a literal empty string.
capture_eao_original() {
  [ "$EAO_TOUCHED" = true ] && return
  EAO_ORIG_ACTION=$(kubectl get energyawareorchestration "$EAO" -n "$APP_NAMESPACE" \
    -o jsonpath='{.status.decision.action}' 2>/dev/null)
  EAO_ORIG_REASON=$(kubectl get energyawareorchestration "$EAO" -n "$APP_NAMESPACE" \
    -o jsonpath='{.status.decision.reason}' 2>/dev/null)
  EAO_ORIG_SUFFICIENT=$(kubectl get energyawareorchestration "$EAO" -n "$APP_NAMESPACE" \
    -o jsonpath='{.status.energyMetrics.sufficient}' 2>/dev/null)
  EAO_TOUCHED=true
}

revert_eao_window() {
  echo "  Restoring $EAO's energy window status to its pre-demo values (not waiting on its own controller)"
  local action_json reason_json sufficient_json
  action_json=$([ -n "$EAO_ORIG_ACTION" ] && printf '"%s"' "$EAO_ORIG_ACTION" || echo null)
  reason_json=$([ -n "$EAO_ORIG_REASON" ] && printf '"%s"' "$EAO_ORIG_REASON" || echo null)
  sufficient_json=$([ -n "$EAO_ORIG_SUFFICIENT" ] && echo "$EAO_ORIG_SUFFICIENT" || echo null)
  run kubectl patch energyawareorchestration "$EAO" -n "$APP_NAMESPACE" --type=merge --subresource=status \
    -p "{\"status\":{\"decision\":{\"action\":${action_json},\"reason\":${reason_json}},\"energyMetrics\":{\"sufficient\":${sufficient_json}}}}" >/dev/null
}

close_energy_window() {
  capture_eao_original
  run kubectl patch energyawareorchestration "$EAO" -n "$APP_NAMESPACE" --type=merge --subresource=status \
    -p '{"status":{"decision":{"action":"Waiting","reason":"demo: energy window closed"},"energyMetrics":{"sufficient":false}}}' >/dev/null
}

open_energy_window() {
  capture_eao_original
  run kubectl patch energyawareorchestration "$EAO" -n "$APP_NAMESPACE" --type=merge --subresource=status \
    -p '{"status":{"decision":{"action":"DeployImmediately","reason":"demo: energy window open"},"energyMetrics":{"sufficient":true}}}' >/dev/null
}

# ─── Beat 1 — NoOp ──────────────────────────────────────────────────────────

# NoOp arms cooldownSeconds same as any other terminal outcome (see
# dispatch.go's dispatchNoOp), so each round clears it by hand first instead
# of waiting out the real cooldown.
noop() {
  step "Beat 1 — NoOp"
  narrate "NoOp still arms cooldown, so each round below clears it by hand."

  local rounds=3
  for i in $(seq 1 "$rounds"); do
    step "NoOp cycle $i of $rounds"
    clear_cooldown
    narrate "Watch Pane A/B/D — up to 30s for the next tick, then instant."
    pause
  done
}

# ─── Beat 2 — RetryPendingSchedule ──────────────────────────────────────────

# Scales $APP up by one and waits (up to 60s) for the new pod to go Pending
# behind the energy gate. Sets RETRY_PENDING_POD.
retry_scale_and_wait_pending() {
  NGINX2_ORIGINAL_REPLICAS=$(run kubectl get deployment "$APP" -n "$APP_NAMESPACE" \
    -o jsonpath='{.spec.replicas}')
  narrate "Scaling $APP up by one to force a pod Pending behind the energy gate."
  run kubectl scale deployment "$APP" -n "$APP_NAMESPACE" \
    --replicas=$((NGINX2_ORIGINAL_REPLICAS + 1))

  step "Waiting for the new pod to go Pending"
  RETRY_PENDING_POD=""
  local waited=0
  while [ -z "$RETRY_PENDING_POD" ] && [ "$waited" -lt 60 ]; do
    RETRY_PENDING_POD=$(kubectl get pods -n "$APP_NAMESPACE" -l app=nginx-2 \
      --field-selector=status.phase=Pending -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    [ -z "$RETRY_PENDING_POD" ] && { printf '.'; sleep 2; waited=$((waited + 2)); }
  done
  echo
  run kubectl get pods -n "$APP_NAMESPACE" -l app=nginx-2 -o wide
}

# Re-asserts clear-cooldown + open-window every 3s (up to 90s) until
# RETRY_PENDING_POD is gone. A one-shot patch can lose a race to cooldown
# re-arming (reconciler.go checks cooldown before the trigger) or to a NoOp
# cycle slipping in between our clear and our patch landing.
retry_reassert_until_retried() {
  local waited=0
  while [ "$waited" -lt 90 ]; do
    clear_cooldown
    open_energy_window
    sleep 3
    waited=$((waited + 3))
    if ! kubectl get pod "$RETRY_PENDING_POD" -n "$APP_NAMESPACE" >/dev/null 2>&1; then
      echo "  $RETRY_PENDING_POD was deleted/replaced — retried after ${waited}s."
      return 0
    fi
  done
  echo "  Timed out after ${waited}s without a retry — check Pane B."
  return 1
}

retry() {
  step "Beat 2 — RetryPendingSchedule"
  clear_cooldown
  retry_scale_and_wait_pending
  pause

  narrate "A one-shot patch can lose to cooldown re-arming, so this
re-asserts every few seconds — the echoed patches double as progress."
  step "Re-asserting the open energy window until $RETRY_PENDING_POD is retried"
  retry_reassert_until_retried || true

  run kubectl get pods -n "$APP_NAMESPACE" -l app=nginx-2 -o wide
  pause
}

# ─── Beat 3 — Move ──────────────────────────────────────────────────────────

# Re-asserts clear-cooldown + close-window every 2s (up to 150s), polling
# directly for Enacting rather than "left Watching": a full NoOp cycle
# (Watching->Triggered->Evaluating->Watching) completes in under a second,
# so sampling for "not Watching" would alias right past it. Enacting is the
# one state that actually holds still (moveEnactor's fixed 60s window).
move_wait_for_enacting() {
  local waited=0
  while [ "$waited" -lt 150 ]; do
    clear_cooldown
    close_energy_window
    if [ "$(profile_state)" = "Enacting" ]; then
      echo "  Reached Enacting after ${waited}s — check Pane A/B for the Decided reason."
      return 0
    fi
    sleep 2
    waited=$((waited + 2))
  done
  echo "  Did not observe Enacting after ${waited}s — check Pane B. A NoOp in
  between is expected; if it keeps NoOp-ing, check $APP has a Running pod."
  return 1
}

# Re-asserts the open window every 3s for the rest of moveEnactor's 60s
# window, in case the external EAO controller reverts a single patch.
move_keep_window_open() {
  local waited=0
  while [ "$waited" -lt 60 ] && [ "$(profile_state)" = "Enacting" ]; do
    open_energy_window
    sleep 3
    waited=$((waited + 3))
  done
}

move() {
  step "Beat 3 — Move"
  narrate "Needs the window CLOSED to trigger, OPEN for the replacement to
schedule — same cooldown/kopf race as beat 2, handled the same way."

  step "Forcing a guaranteed above-threshold Move"
  bump_mock_agent

  step "Closing the energy window and re-asserting until Enacting is observed"
  move_wait_for_enacting || return 1

  step "Eviction is in flight — opening the energy window immediately"
  open_energy_window
  narrate "Re-asserting the open window for the rest of the enacting window."
  pause
  move_keep_window_open

  # Revert MOVE_PROBABILITY now rather than waiting for script exit: once the
  # move has resolved, nothing is holding the window open any more, so any
  # natural trigger that fires during the unbounded watch/pause below would
  # get a forced Move recommendation with no one home to rescue it — exactly
  # the failure mode this sidesteps.
  revert_mock_agent

  narrate "Watch Pane C for a decision-store hit — the replacement pod being
steered to the Move's target node instead of a fresh AI call."
  step "Watching $APP pods (Ctrl-C once you see the move complete)"
  run kubectl get pods -n "$APP_NAMESPACE" -l app=nginx-2 -o wide --watch || true

  step "Final state"
  run kubectl describe orchestrationprofile "$PROFILE"
  pause
}

# ─── Main ───────────────────────────────────────────────────────────────────

usage() {
  echo "Usage: $0 [noop|retry|move|cleanup]"
  echo "  (no argument) runs noop, retry, move in order"
}

main() {
  case "${1:-all}" in
    noop) noop ;;
    retry) retry ;;
    move) move ;;
    cleanup) : ;; # cleanup runs unconditionally via the EXIT trap below
    all)
      noop
      retry
      move
      ;;
    -h|--help) usage; trap - EXIT; exit 0 ;;
    *) usage >&2; trap - EXIT; exit 1 ;;
  esac
}

main "$@"
