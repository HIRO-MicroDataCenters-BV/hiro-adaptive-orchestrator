#!/bin/bash
# demo/lib/demo_common.sh
#
# Helpers shared by the rebalance demo scripts. Source it, don't run it.

NAMESPACE=${NAMESPACE:-hiro-adaptive-orchestrator-system}
APP_NAMESPACE=${APP_NAMESPACE:-default}

PROFILE=orchestrationprofile-2
APP=nginx-deployment-2
EAO=eaoprofile-optional

APP_ORIGINAL_REPLICAS=""
EAO_TOUCHED=false
EAO_ORIG_ACTION=""
EAO_ORIG_REASON=""
EAO_ORIG_SUFFICIENT=""
EAO_ORIG_SLOT=""
RESOURCES_TOUCHED=false
APP_CONTAINER_NAME=""
APP_ORIGINAL_CPU_REQUEST=""
APP_ORIGINAL_MEMORY_REQUEST=""
APP_ORIGINAL_CPU_LIMIT=""
APP_ORIGINAL_MEMORY_LIMIT=""
ESCALATION_THRESHOLD_TOUCHED=false
APP_ORIGINAL_ESCALATION_THRESHOLD=""

# ─── Presentation ──────────────────────────────────────────────────────────

step() { printf '\n\033[36m===>\033[0m %s\n' "$*"; }
narrate() { printf '\n\033[33m%s\033[0m\n' "$*"; }

# Reads /dev/tty, as stdin may be piped; DEMO_AUTO=true skips every pause.
pause() {
  if [ "${DEMO_AUTO:-false}" = true ]; then
    return 0
  fi
  if [ -r /dev/tty ]; then
    read -rp $'\n\033[2mPress Enter to continue...\033[0m ' _ < /dev/tty || true
  else
    printf '\n\033[2m(no TTY available — continuing without pause)\033[0m\n'
  fi
}

# Echoes the command to stderr first, so `x=$(run kubectl get ...)` still works; DEMO_QUIET=true hides the echo.
run() {
  if [ "${DEMO_QUIET:-false}" != true ]; then
    printf '\033[2m$ %s\033[0m\n' "$*" >&2
  fi
  "$@"
}

profile_state() {
  kubectl get orchestrationprofile "$PROFILE" -o jsonpath='{.status.rebalancingStatus.state}' 2>/dev/null
}

clear_cooldown() {
  run kubectl patch orchestrationprofile "$PROFILE" --type=merge --subresource=status \
    -p '{"status":{"rebalancingStatus":{"cooldownUntil":null}}}' >/dev/null
}

# ─── Energy window ─────────────────────────────────────────────────────────

# $EAO belongs to the external Energy Aware Orchestrator, so keep its real status to restore it afterwards.
capture_eao_original() {
  [ "$EAO_TOUCHED" = true ] && return
  EAO_ORIG_ACTION=$(kubectl get energyawareorchestration "$EAO" -n "$APP_NAMESPACE" \
    -o jsonpath='{.status.decision.action}' 2>/dev/null)
  EAO_ORIG_REASON=$(kubectl get energyawareorchestration "$EAO" -n "$APP_NAMESPACE" \
    -o jsonpath='{.status.decision.reason}' 2>/dev/null)
  EAO_ORIG_SUFFICIENT=$(kubectl get energyawareorchestration "$EAO" -n "$APP_NAMESPACE" \
    -o jsonpath='{.status.energyMetrics.sufficient}' 2>/dev/null)
  # jsonpath prints the slot object as JSON, or nothing when it's unset.
  EAO_ORIG_SLOT=$(kubectl get energyawareorchestration "$EAO" -n "$APP_NAMESPACE" \
    -o jsonpath='{.status.decision.scheduledSlot}' 2>/dev/null)
  EAO_TOUCHED=true
}

# An empty capture means the field was unset, so it's patched back with null (which removes the key).
revert_eao_window() {
  echo "  Restoring $EAO's energy window status to its pre-demo values (not waiting on its own controller)"
  local action_json reason_json sufficient_json slot_json
  action_json=$([ -n "$EAO_ORIG_ACTION" ] && printf '"%s"' "$EAO_ORIG_ACTION" || echo null)
  reason_json=$([ -n "$EAO_ORIG_REASON" ] && printf '"%s"' "$EAO_ORIG_REASON" || echo null)
  sufficient_json=$([ -n "$EAO_ORIG_SUFFICIENT" ] && echo "$EAO_ORIG_SUFFICIENT" || echo null)
  slot_json=${EAO_ORIG_SLOT:-null}
  run kubectl patch energyawareorchestration "$EAO" -n "$APP_NAMESPACE" --type=merge --subresource=status \
    -p "{\"status\":{\"decision\":{\"action\":${action_json},\"reason\":${reason_json},\"scheduledSlot\":${slot_json}},\"energyMetrics\":{\"sufficient\":${sufficient_json}}}}" >/dev/null
}

# A slot an hour ahead is a Hold: the gate keeps new pods Pending and EnergyThreshold fires.
# lastUpdated is set because the AI Decision Service ignores a stale Hold.
close_energy_window() {
  capture_eao_original
  local slot_start slot_end now
  slot_start=$(date -u -v+1H +%FT%TZ 2>/dev/null || date -u -d '+1 hour' +%FT%TZ)
  slot_end=$(date -u -v+2H +%FT%TZ 2>/dev/null || date -u -d '+2 hours' +%FT%TZ)
  now=$(date -u +%FT%TZ)
  run kubectl patch energyawareorchestration "$EAO" -n "$APP_NAMESPACE" --type=merge --subresource=status \
    -p "{\"status\":{\"lastUpdated\":\"${now}\",\"decision\":{\"action\":\"Scheduled\",\"reason\":\"demo: energy window closed\",\"scheduledSlot\":{\"slotStart\":\"${slot_start}\",\"slotEnd\":\"${slot_end}\"}},\"energyMetrics\":{\"sufficient\":false}}}" >/dev/null
}

open_energy_window() {
  capture_eao_original
  local now
  now=$(date -u +%FT%TZ)
  run kubectl patch energyawareorchestration "$EAO" -n "$APP_NAMESPACE" --type=merge --subresource=status \
    -p "{\"status\":{\"lastUpdated\":\"${now}\",\"decision\":{\"action\":\"DeployImmediately\",\"reason\":\"demo: energy window open\",\"scheduledSlot\":null},\"energyMetrics\":{\"sufficient\":true}}}" >/dev/null
}

# ─── App replicas and resources ────────────────────────────────────────────

# Lets a scale or rollout finish before the window goes back to EAO's real status, which may be closed.
await_app_rollout() {
  run kubectl rollout status deployment "$APP" -n "$APP_NAMESPACE" --timeout=90s || true
}

# Captured once, so the first scenario to change the count sets what gets restored.
capture_original_replicas() {
  [ -n "$APP_ORIGINAL_REPLICAS" ] && return
  APP_ORIGINAL_REPLICAS=$(run kubectl get deployment "$APP" -n "$APP_NAMESPACE" \
    -o jsonpath='{.spec.replicas}')
}

# Opens the window first, as scaling back up can create a pod that must pass the gate.
revert_replicas() {
  echo "  Restoring $APP to $APP_ORIGINAL_REPLICAS replica(s)"
  open_energy_window
  run kubectl scale deployment "$APP" -n "$APP_NAMESPACE" --replicas="$APP_ORIGINAL_REPLICAS"
  await_app_rollout
}

# Captured once, so the true original is restored however many times resources change.
capture_original_resources() {
  [ "$RESOURCES_TOUCHED" = true ] && return
  APP_CONTAINER_NAME=$(run kubectl get deployment "$APP" -n "$APP_NAMESPACE" \
    -o jsonpath='{.spec.template.spec.containers[0].name}')
  APP_ORIGINAL_CPU_REQUEST=$(run kubectl get deployment "$APP" -n "$APP_NAMESPACE" \
    -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}')
  APP_ORIGINAL_MEMORY_REQUEST=$(run kubectl get deployment "$APP" -n "$APP_NAMESPACE" \
    -o jsonpath='{.spec.template.spec.containers[0].resources.requests.memory}')
  APP_ORIGINAL_CPU_LIMIT=$(run kubectl get deployment "$APP" -n "$APP_NAMESPACE" \
    -o jsonpath='{.spec.template.spec.containers[0].resources.limits.cpu}')
  APP_ORIGINAL_MEMORY_LIMIT=$(run kubectl get deployment "$APP" -n "$APP_NAMESPACE" \
    -o jsonpath='{.spec.template.spec.containers[0].resources.limits.memory}')
  RESOURCES_TOUCHED=true
}

# Patching the template rolls every pod, which also undoes any in-place resize.
revert_resources() {
  echo "  Restoring $APP's container resources to their pre-demo values"
  open_energy_window
  run kubectl set resources deployment "$APP" -n "$APP_NAMESPACE" \
    -c="$APP_CONTAINER_NAME" \
    --requests="cpu=$APP_ORIGINAL_CPU_REQUEST,memory=$APP_ORIGINAL_MEMORY_REQUEST" \
    --limits="cpu=$APP_ORIGINAL_CPU_LIMIT,memory=$APP_ORIGINAL_MEMORY_LIMIT"
  await_app_rollout
}

# ─── Escalation ────────────────────────────────────────────────────────────

profile_escalated() {
  kubectl get orchestrationprofile "$PROFILE" -o jsonpath='{.status.rebalancingStatus.escalated}' 2>/dev/null
}

# The documented unpause; resetting the count avoids re-escalating on the next single failure.
clear_escalation() {
  run kubectl patch orchestrationprofile "$PROFILE" --type=merge --subresource=status \
    -p '{"status":{"rebalancingStatus":{"escalated":false,"consecutiveFailures":0}}}' >/dev/null
}

# Checks the live flag, as any scenario's failures can escalate the profile.
ensure_not_escalated() {
  if [ "$(profile_escalated)" = "true" ]; then
    echo "  $PROFILE is escalated — clearing it so the loop isn't left paused."
    clear_escalation
  fi
}

capture_original_escalation_threshold() {
  [ "$ESCALATION_THRESHOLD_TOUCHED" = true ] && return
  APP_ORIGINAL_ESCALATION_THRESHOLD=$(kubectl get orchestrationprofile "$PROFILE" \
    -o jsonpath='{.spec.rebalancing.escalationThreshold}' 2>/dev/null)
  ESCALATION_THRESHOLD_TOUCHED=true
}

set_escalation_threshold() {
  run kubectl patch orchestrationprofile "$PROFILE" --type=merge \
    -p "{\"spec\":{\"rebalancing\":{\"escalationThreshold\":$1}}}" >/dev/null
}

revert_escalation_threshold() {
  echo "  Restoring escalationThreshold to its pre-demo value"
  local val_json
  val_json=$([ -n "$APP_ORIGINAL_ESCALATION_THRESHOLD" ] && echo "$APP_ORIGINAL_ESCALATION_THRESHOLD" || echo null)
  run kubectl patch orchestrationprofile "$PROFILE" --type=merge \
    -p "{\"spec\":{\"rebalancing\":{\"escalationThreshold\":${val_json}}}}" >/dev/null
}
