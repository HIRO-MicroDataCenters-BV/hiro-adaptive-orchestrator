#!/bin/bash
# demo/demo_rebalance_with_mock_agent.sh
#
# Guided, narrated demo of the rebalance engine's decision lifecycle:
#   Watching -> Triggered -> Evaluating -> Decided -> Enacting -> Watching
#
# See internal/rebalance/README.md#demo-walkthrough for a small flow diagram
# of what each beat below actually does mechanically (Mermaid doesn't render
# in a .sh comment, so it lives there, not here).
#
# Needs the mock agent, which the beats steer through env vars. Deploy with
#   USE_MOCK_AGENT=true USE_AI_DECISION_SERVICE=false deploy/scripts/deploy_full_stack_with_helm.sh
#
# Drives six beats against a live cluster (the full-stack deploy must
# already be running — see deploy/scripts/deploy_full_stack.sh), all against the
# existing orchestrationprofile-2 / nginx-deployment-2 / eaoprofile-optional:
#   1. NoOp       — a normal cycle that decides nothing needs to change.
#   2. Retry      — a pod stuck Pending behind the energy gate gets retried
#                   once the energy window opens (RetryPendingSchedule).
#   3. Move       — an accepted Move recommendation evicts a pod and steers
#                   its replacement onto a specific node via the DecisionStore.
#   4. Scale      — an accepted AdjustReplicas recommendation patches the
#                   Deployment's replica count directly (no eviction, no
#                   DecisionStore — see the "Replica bounds" section of
#                   internal/rebalance/README.md).
#   5. Resources  — an accepted AdjustResources recommendation resizes
#                   nginx-2's container CPU/Memory, in place on the running
#                   pod where the cluster supports it, or via the pod
#                   template (next rollout) otherwise — see the
#                   "AdjustResources" section of internal/rebalance/README.md.
#   6. Escalate   — two ways the rebalance loop pauses itself and waits for a
#                   human: the AI directly recommending Escalate, and the
#                   engine auto-escalating after repeated Failed cycles — see
#                   the "Escalation" section of internal/rebalance/README.md.
#
# Beats 3 and 5 keep the energy window OPEN: under a hold the operator refuses
# Move and AdjustResources, because the gate would hold the pods they replace.
# PeriodicReview triggers them instead.
#
# Beat 4 still needs the window CLOSED to fire the trigger (EnergyThreshold only
# matches on an EAO hold: a slot still ahead) but OPEN for the new replica's pod
# to pass CheckEnergyGate and schedule.
#
# Same race as beat 2: a one-shot patch can lose to a NoOp re-arming cooldown or
# to the external EAO controller's own reconcile reverting it before our watch
# reacts. So this script re-asserts clear-cooldown and the window every few
# seconds instead of patching once.
#
# Beat 5 does NOT poll for the Enacting state: the fallback path patches the
# template and returns to Watching in the same reconcile, so Enacting never
# lingers long enough for a 2s poll to observe it. Polling for it just kept
# re-arming the trigger — a burst of AdjustResources decisions, each starting a
# new rollout that superseded the last. wait_for_resources_applied() below polls
# the Deployment's actual resource values instead, so it stops the instant the
# first patch is visible.
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
#   demo/demo_rebalance_with_mock_agent.sh              # numbered menu: pick one beat, all, or cleanup
#   demo/demo_rebalance_with_mock_agent.sh all          # run all six beats, in order
#   demo/demo_rebalance_with_mock_agent.sh noop         # run a single beat
#   demo/demo_rebalance_with_mock_agent.sh retry
#   demo/demo_rebalance_with_mock_agent.sh move
#   demo/demo_rebalance_with_mock_agent.sh scale
#   demo/demo_rebalance_with_mock_agent.sh resources
#   demo/demo_rebalance_with_mock_agent.sh escalate
#   demo/demo_rebalance_with_mock_agent.sh cleanup      # revert the mock agent + restore
#                                       # replica count/resources/escalation,
#                                       # without running anything else
#
# Every beat pauses for Enter before continuing, so you control pacing while
# talking. Ctrl-C at any point still triggers cleanup (trap below).
#
# ─── Config (override via environment) ────────────────────────────────────
#   NAMESPACE       operator namespace       (default: hiro-adaptive-orchestrator-system)
#   APP_NAMESPACE   demo app namespace       (default: default)

set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/demo_common.sh"

# ─── Cleanup (always runs on exit, even Ctrl-C) ────────────────────────────

MOCK_AGENT_MOVE_BUMPED=false
MOCK_AGENT_SCALE_BUMPED=false
MOCK_AGENT_RESOURCE_BUMPED=false
MOCK_AGENT_ESCALATE_BUMPED=false
RETRY_PENDING_POD=""
MOCK_AGENT_SCALED_DOWN=false

cleanup() {
  step "Cleanup"

  # First, always: if the profile ended up escalated (this beat interrupted
  # mid-way, or — in principle — a real repeated failure elsewhere during the
  # demo), unpause it before anything else runs. A live check, not a
  # beat-scoped flag, since escalation isn't only reachable from beat 6.
  ensure_not_escalated

  if [ "$MOCK_AGENT_MOVE_BUMPED" = true ]; then
    revert_mock_agent
  fi

  if [ "$MOCK_AGENT_SCALE_BUMPED" = true ]; then
    revert_mock_agent_scale
  fi

  if [ "$MOCK_AGENT_RESOURCE_BUMPED" = true ]; then
    revert_mock_agent_resources
  fi

  if [ "$MOCK_AGENT_ESCALATE_BUMPED" = true ]; then
    revert_mock_agent_escalate
  fi

  if [ "$MOCK_AGENT_SCALED_DOWN" = true ]; then
    restore_mock_agent_replicas
  fi

  if [ "$ESCALATION_THRESHOLD_TOUCHED" = true ]; then
    revert_escalation_threshold
  fi

  if [ -n "$APP_ORIGINAL_REPLICAS" ]; then
    revert_replicas
  fi

  if [ "$RESOURCES_TOUCHED" = true ]; then
    revert_resources
  fi

  # Last, always: revert_replicas/revert_resources each already open the
  # window and wait out $APP's rollout, so nothing is still trying to
  # schedule by the time this hands EAO status back to its true pre-demo
  # value — which can itself be closed/insufficient.
  if [ "$EAO_TOUCHED" = true ]; then
    revert_eao_window
  fi

  echo "  Done."
}
trap cleanup EXIT INT TERM

# ─── Mock agent tuning (beats 3/4 need a guaranteed, above-threshold action) ─

# Sets (KEY=VALUE) or removes (KEY-) mock knobs; the env change itself rolls the pod.
# Env only, never the ConfigMap, so a Helm-installed mock stays Helm-owned.
set_mock_env() {
  run kubectl set env deployment/mock-decision-agent -n "$NAMESPACE" "$@"
  run kubectl rollout status deployment/mock-decision-agent -n "$NAMESPACE" --timeout=120s
}

bump_mock_agent() {
  step "Forcing the mock agent to always recommend an above-threshold Move"
  set_mock_env MOVE_PROBABILITY=1.0 IMPROVEMENT_MIN=30.0
  MOCK_AGENT_MOVE_BUMPED=true
}

revert_mock_agent() {
  echo "  Reverting mock agent to its default MOVE_PROBABILITY / IMPROVEMENT_MIN"
  set_mock_env MOVE_PROBABILITY- IMPROVEMENT_MIN-
  MOCK_AGENT_MOVE_BUMPED=false
}

bump_mock_agent_scale() {
  step "Forcing the mock agent to always recommend an above-threshold AdjustReplicas"
  set_mock_env SCALE_PROBABILITY=1.0 IMPROVEMENT_MIN=30.0
  MOCK_AGENT_SCALE_BUMPED=true
}

revert_mock_agent_scale() {
  echo "  Reverting mock agent to its default SCALE_PROBABILITY / IMPROVEMENT_MIN"
  set_mock_env SCALE_PROBABILITY- IMPROVEMENT_MIN-
  MOCK_AGENT_SCALE_BUMPED=false
}

bump_mock_agent_resources() {
  step "Forcing the mock agent to always recommend an above-threshold AdjustResources"
  set_mock_env RESOURCE_PROBABILITY=1.0 IMPROVEMENT_MIN=30.0
  MOCK_AGENT_RESOURCE_BUMPED=true
}

revert_mock_agent_resources() {
  echo "  Reverting mock agent to its default RESOURCE_PROBABILITY / IMPROVEMENT_MIN"
  set_mock_env RESOURCE_PROBABILITY- IMPROVEMENT_MIN-
  MOCK_AGENT_RESOURCE_BUMPED=false
}

# ─── Beat 1 — NoOp ──────────────────────────────────────────────────────────

# NoOp arms cooldownSeconds same as any other terminal outcome (see
# dispatch.go's dispatchNoOp), so each round clears it by hand first instead
# of waiting out the real cooldown.
noop() {
  step "Beat 1 — NoOp"
  narrate "NoOp still arms cooldown, so each round below clears it by hand."
  pause

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
  capture_original_replicas
  narrate "Scaling $APP up by one to force a pod Pending behind the energy gate."
  run kubectl scale deployment "$APP" -n "$APP_NAMESPACE" \
    --replicas=$((APP_ORIGINAL_REPLICAS + 1))

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
  narrate "Scales $APP up by one to force a pod Pending behind the closed
energy gate, then re-asserts the open window until the operator retries it."
  pause

  clear_cooldown
  retry_scale_and_wait_pending
  pause

  narrate "A one-shot patch can lose to cooldown re-arming, so this
re-asserts every few seconds — the echoed patches double as progress."
  step "Re-asserting the open energy window until $RETRY_PENDING_POD is retried"
  pause
  retry_reassert_until_retried || true

  run kubectl get pods -n "$APP_NAMESPACE" -l app=nginx-2 -o wide
  pause
}

# ─── Beat 3 — Move ──────────────────────────────────────────────────────────

# Re-asserts clear-cooldown + $1 (open_energy_window or close_energy_window)
# every 2s (up to 150s), polling
# directly for Enacting rather than "left Watching": a full NoOp cycle
# (Watching->Triggered->Evaluating->Watching) completes in under a second,
# so sampling for "not Watching" would alias right past it. Enacting is the
# one state that actually holds still while its own enactor does real,
# time-consuming work — moveEnactor waiting on eviction+reschedule,
# scaleEnactor waiting on ReadyReplicas — so this is shared by beats 3 and 4
# only. resourceEnactor's fallback (template-patch) path patches and returns
# in the same reconcile, so it never holds Enacting long enough for this to
# catch — beat 5 uses wait_for_resources_applied() instead (see below).
wait_for_enacting() {
  local assert_window=$1
  local waited=0
  while [ "$waited" -lt 150 ]; do
    clear_cooldown
    "$assert_window"
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

# Beat 5's counterpart to wait_for_enacting above — see that function's doc
# comment for why polling for Enacting doesn't work for resourceEnactor's
# fallback path. Polls the Deployment's actual requests.cpu instead of
# profile state: race-free, and stops re-triggering the instant the first
# patch is visible, instead of hammering the trigger every 2s for the full
# 150s like an Enacting-based poll would.
wait_for_resources_applied() {
  local waited=0
  local cur_cpu
  while [ "$waited" -lt 150 ]; do
    clear_cooldown
    open_energy_window
    cur_cpu=$(kubectl get deployment "$APP" -n "$APP_NAMESPACE" \
      -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}' 2>/dev/null)
    if [ -n "$cur_cpu" ] && [ "$cur_cpu" != "$APP_ORIGINAL_CPU_REQUEST" ]; then
      echo "  Resize applied after ${waited}s (requests.cpu now $cur_cpu) — check Pane A/B for the Decided reason."
      return 0
    fi
    sleep 2
    waited=$((waited + 2))
  done
  echo "  Did not observe a resource change after ${waited}s — check Pane B. A NoOp in
  between is expected; if it keeps NoOp-ing, check $APP has a Running pod."
  return 1
}

# Re-asserts the open window every 3s, up to $2 seconds (default 90), until
# $1 says we're done: "enacting" (beats 3/4, default) waits for the profile
# to leave Enacting; "rollout" (beat 5) waits for $APP's rollout to finish
# instead, since resourceEnactor's fallback path leaves Enacting before the
# replacement pod even starts scheduling.
keep_window_open() {
  local mode=${1:-enacting}
  local seconds=${2:-90}
  local waited=0
  while [ "$waited" -lt "$seconds" ]; do
    case "$mode" in
      enacting) [ "$(profile_state)" = "Enacting" ] || return 0 ;;
      rollout)
        kubectl rollout status deployment "$APP" -n "$APP_NAMESPACE" --timeout=1s >/dev/null 2>&1 && return 0
        ;;
    esac
    open_energy_window
    sleep 3
    waited=$((waited + 3))
  done
  return 1
}

move() {
  step "Beat 3 — Move"
  narrate "Needs the window OPEN: under a hold the operator refuses a Move, since
the gate would hold the replacement. PeriodicReview triggers the AI."
  pause

  step "Forcing a guaranteed above-threshold Move"
  bump_mock_agent
  pause

  step "Opening the energy window and re-asserting until Enacting is observed"
  wait_for_enacting open_energy_window || return 1

  narrate "Eviction is in flight — re-asserting the open window for the rest of the enacting window."
  pause
  keep_window_open enacting 60

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

# ─── Beat 4 — AdjustReplicas (Scale) ────────────────────────────────────────

# Unlike Move, a real target replica count depends on how many pods $APP
# currently has — the mock agent scales up by one from 1 (its steady state
# after cleanup), so this beat is deterministic in practice even though the
# mock agent picks a direction at random once above 1. capture_original_replicas
# runs here too (idempotent) so cleanup() restores the true pre-demo count
# even if this beat runs standalone, without retry() having captured it first.
scale() {
  step "Beat 4 — AdjustReplicas (Scale)"
  narrate "Needs the window CLOSED to trigger, OPEN for the new replica's pod to
actually schedule. Unlike a Move, scaling up removes no running pod, so it isn't refused."
  capture_original_replicas
  pause

  step "Forcing a guaranteed above-threshold AdjustReplicas"
  bump_mock_agent_scale
  pause

  step "Closing the energy window and re-asserting until Enacting is observed"
  wait_for_enacting close_energy_window || return 1

  step "Spec.Replicas is patched — opening the energy window immediately"
  open_energy_window
  narrate "Re-asserting the open window for the rest of the enacting window."
  pause
  keep_window_open enacting 60

  # Same reasoning as move(): revert now, not at script exit, so nothing
  # forces another Scale recommendation while no one's watching the window.
  revert_mock_agent_scale

  step "Watching $APP pods (Ctrl-C once the new replica is Running)"
  run kubectl get pods -n "$APP_NAMESPACE" -l app=nginx-2 -o wide --watch || true

  step "Final state"
  run kubectl describe orchestrationprofile "$PROFILE"
  narrate "Replica count reverts to $APP_ORIGINAL_REPLICAS on cleanup, same as beat 2."
  pause
}

# ─── Beat 5 — AdjustResources (Resize) ─────────────────────────────────────

# The window stays OPEN: under a hold the operator refuses AdjustResources,
# because the template patch starts a rollout whose new pods the gate would
# hold. capture_original_resources runs here (idempotent) so cleanup() restores
# the true pre-demo values even if this beat runs standalone.
resources() {
  step "Beat 5 — AdjustResources (Resize)"
  capture_original_resources
  narrate "Resizes $APP_CONTAINER_NAME's CPU/Memory. If this cluster supports
in-place pod resize (K8s 1.27+), the running pod updates immediately. The
window stays open, as under a hold the operator refuses this action."
  pause

  step "Forcing a guaranteed above-threshold AdjustResources"
  bump_mock_agent_resources
  pause

  step "Opening the energy window and re-asserting until the resize is applied"
  wait_for_resources_applied || return 1

  narrate "Re-asserting the open window until $APP finishes rolling out (or
up to 90s) — returns immediately if an in-place resize succeeded, since
there's no new rollout to wait for."
  pause
  keep_window_open rollout 90

  # Same reasoning as move()/scale(): revert now, not at script exit, so
  # nothing forces another AdjustResources recommendation while no one's
  # watching the window.
  revert_mock_agent_resources

  narrate "Watch Pane B for which path was actually taken — 'resized N
running pod(s) in place' (same pod, new resources) vs. 'template patched ...
effective on next rollout' (a new pod appears below instead)."
  step "Watching $APP pods (Ctrl-C once you see the resize complete)"
  run kubectl get pods -n "$APP_NAMESPACE" -l app=nginx-2 -o wide --watch || true

  step "Final state"
  run kubectl describe orchestrationprofile "$PROFILE"
  run kubectl get deployment "$APP" -n "$APP_NAMESPACE" \
    -o jsonpath='{.spec.template.spec.containers[0].resources}'
  echo
  narrate "Resources revert to their pre-demo values on cleanup, same as beat 4's replica count."
  pause
}

# ─── Beat 6 — Escalate ──────────────────────────────────────────────────────

# Re-asserts clear-cooldown + close-window every 2s (up to 90s) until the
# profile's escalated flag flips true. EnergyThreshold only matches while the
# window is closed — without re-closing it, the external EAO controller can flip
# it back open between cycles and the streak stalls short of the threshold.
wait_for_escalated() {
  local waited=0
  while [ "$waited" -lt 90 ]; do
    clear_cooldown
    close_energy_window
    if [ "$(profile_escalated)" = "true" ]; then
      echo "  Escalated after ${waited}s — check Pane A/B for the reason."
      return 0
    fi
    sleep 2
    waited=$((waited + 2))
  done
  echo "  Did not observe escalation after ${waited}s — check Pane B."
  return 1
}

bump_mock_agent_escalate() {
  step "Forcing the mock agent to always recommend Escalate"
  set_mock_env ESCALATE_PROBABILITY=1.0
  MOCK_AGENT_ESCALATE_BUMPED=true
}

revert_mock_agent_escalate() {
  echo "  Reverting mock agent to its default ESCALATE_PROBABILITY"
  set_mock_env ESCALATE_PROBABILITY-
  MOCK_AGENT_ESCALATE_BUMPED=false
}

# Simulates "the AI agent is unreachable" for the auto-escalation half of
# this beat — scaling to 0 makes every consult fail fast (bounded by
# DecisionTimeout, default 5s) rather than waiting out a much longer enactor
# timeout, so the Failed streak needed to auto-escalate builds quickly.
scale_mock_agent_down() {
  step "Scaling mock-decision-agent to 0 (simulating the AI agent being unreachable)"
  narrate "This also briefly affects initial placement scoring, which shares
the same mock agent — fine for this short a window."
  run kubectl scale deployment mock-decision-agent -n "$NAMESPACE" --replicas=0
  run kubectl rollout status deployment mock-decision-agent -n "$NAMESPACE" --timeout=60s || true
  MOCK_AGENT_SCALED_DOWN=true
}

restore_mock_agent_replicas() {
  echo "  Restoring mock-decision-agent to 1 replica"
  run kubectl scale deployment mock-decision-agent -n "$NAMESPACE" --replicas=1
  run kubectl rollout status deployment mock-decision-agent -n "$NAMESPACE" --timeout=60s || true
  MOCK_AGENT_SCALED_DOWN=false
}

# Two independent ways the loop pauses itself, demoed back to back: Part A
# is the AI directly recommending Escalate (one cycle, no threshold
# involved); Part B is the engine auto-escalating after repeated Failed
# cycles (escalationThreshold temporarily lowered, and the AI simulated
# unreachable, purely so the streak builds fast enough for a live demo).
# Both end the same way — the persistent `escalated` status flag — and both
# are cleared the same way — a plain status patch, no operator restart.
escalate() {
  step "Beat 6 — Escalate"
  narrate "Two ways this loop pauses itself and waits for a human: the AI
directly recommending Escalate, and the engine auto-escalating after
repeated Failed cycles. This beat demos both, and how to clear each."
  pause

  step "Part A — the AI directly recommends Escalate"
  bump_mock_agent_escalate
  pause

  narrate "One cycle is enough — Escalate skips the improvement-threshold
guardrail and the repeated-failure count entirely, same as NoOp/Reject/Defer."
  wait_for_escalated || return 1
  revert_mock_agent_escalate

  narrate "Watch Pane A — the loop is now paused. The next periodic tick
won't do anything: Reconcile checks the escalated flag before it ever
evaluates a trigger condition."
  run kubectl describe orchestrationprofile "$PROFILE"
  pause

  step "Clearing the escalation — the documented unpause"
  clear_escalation
  narrate "That status patch is the whole recovery mechanism. Watching Pane
A/B below proves detection actually resumed, not just that the flag flipped."
  clear_cooldown
  pause

  step "Part B — auto-escalation after repeated Failed cycles"
  capture_original_escalation_threshold
  set_escalation_threshold 2
  narrate "Lowering escalationThreshold to 2 (default 3) purely so this
doesn't take as long to demo live — the mechanism itself doesn't care what
the number is."
  pause

  scale_mock_agent_down
  narrate "Watch Pane B — expect a couple of quick 'Watching (Failed)'
cycles (bounded by the ~5s AI-consultation timeout, not a slow enactor
timeout), then the one that crosses the threshold gets recorded Escalated
instead of Failed."
  wait_for_escalated || return 1

  step "Restoring the mock agent, then clearing the pause the same way as Part A"
  restore_mock_agent_replicas
  clear_escalation
  revert_escalation_threshold
  pause

  clear_cooldown
  narrate "Watch Pane A/B — up to 30s for the next tick, then instant."
  pause

  step "Final state"
  run kubectl describe orchestrationprofile "$PROFILE"
  pause
}

# ─── Main ───────────────────────────────────────────────────────────────────

usage() {
  echo "Usage: $0 [noop|retry|move|scale|resources|escalate|all|cleanup]"
  echo "  (no argument) shows a numbered menu to pick from"
}

# Prints the numbered menu to the terminal and echoes the chosen beat name.
# Re-prompts on invalid input; reads /dev/tty for the same reason pause() does.
menu() {
  local choice
  {
    echo ""
    echo "Rebalance demo — pick what to run:"
    echo "  1) noop       — a normal cycle that decides nothing needs to change"
    echo "  2) retry      — a Pending pod retried once the energy window opens"
    echo "  3) move       — accepted Move: evict + steer replacement to a node"
    echo "  4) scale      — accepted AdjustReplicas: patch replica count"
    echo "  5) resources  — accepted AdjustResources: resize CPU/Memory"
    echo "  6) escalate   — AI-directed and auto-escalation after repeated failures"
    echo "  7) all        — run 1-6 one by one, in order"
    echo "  8) cleanup    — only revert the mock agent / replicas / resources / escalation"
    echo "  q) quit"
  } > /dev/tty
  while true; do
    read -rp "Choice [1-8, q]: " choice < /dev/tty || { echo quit; return; }
    case "$choice" in
      1) echo noop; return ;;
      2) echo retry; return ;;
      3) echo move; return ;;
      4) echo scale; return ;;
      5) echo resources; return ;;
      6) echo escalate; return ;;
      7) echo all; return ;;
      8) echo cleanup; return ;;
      q|Q) echo quit; return ;;
      *) echo "Invalid choice: '$choice'" > /dev/tty ;;
    esac
  done
}

main() {
  local beat=${1:-}
  if [ -z "$beat" ]; then
    if [ ! -r /dev/tty ]; then
      echo "No TTY for the interactive menu; pass a beat name instead." >&2
      usage >&2; trap - EXIT; exit 1
    fi
    beat=$(menu)
  fi

  case "$beat" in quit|-h|--help) ;; *)
    if ! kubectl get deployment mock-decision-agent -n "$NAMESPACE" >/dev/null 2>&1; then
      echo "mock-decision-agent not found in $NAMESPACE. Deploy it first:" >&2
      echo "  USE_MOCK_AGENT=true USE_AI_DECISION_SERVICE=false deploy/scripts/deploy_full_stack_with_helm.sh" >&2
      trap - EXIT; exit 1
    fi
  esac

  case "$beat" in
    quit) trap - EXIT; exit 0 ;;
    noop) noop ;;
    retry) retry ;;
    move) move ;;
    scale) scale ;;
    resources) resources ;;
    escalate) escalate ;;
    cleanup) : ;; # cleanup runs unconditionally via the EXIT trap below
    all)
      noop
      retry
      move
      scale
      resources
      escalate
      ;;
    -h|--help) usage; trap - EXIT; exit 0 ;;
    *) usage >&2; trap - EXIT; exit 1 ;;
  esac
}

main "$@"
