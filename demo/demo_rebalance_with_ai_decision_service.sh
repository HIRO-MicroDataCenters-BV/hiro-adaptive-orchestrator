#!/bin/bash
# demo/demo_rebalance_with_ai_decision_service.sh
#
# Rebalance scenarios against the real AI Decision Service. It can't be steered like the mock: it decides
# from facts, so each scenario creates the real condition, then checks what the service and the operator did.
#
# Today the service answers only Move (off a down node) or NoOp. AdjustReplicas, AdjustResources, Reject,
# Defer and an AI-chosen Escalate still need demo/demo_rebalance_with_mock_agent.sh.
#
# Deploy first:
#   USE_AI_DECISION_SERVICE=true deploy/scripts/deploy_full_stack_with_helm.sh
#
# Usage:
#   demo/demo_rebalance_with_ai_decision_service.sh                     # numbered menu: 6, or 1 3 5, or all
#   demo/demo_rebalance_with_ai_decision_service.sh retry node-failure  # by name, in that order
#   demo/demo_rebalance_with_ai_decision_service.sh all
#   demo/demo_rebalance_with_ai_decision_service.sh --auto all          # no pauses or prompts
#   demo/demo_rebalance_with_ai_decision_service.sh list                # the scenarios
#   demo/demo_rebalance_with_ai_decision_service.sh panes               # log commands to watch alongside
#   demo/demo_rebalance_with_ai_decision_service.sh cleanup             # recover after a killed run
#
# Exits non-zero if any check fails.
#
# Config (environment):
#   NAMESPACE              operator namespace              (default: hiro-adaptive-orchestrator-system)
#   APP_NAMESPACE          demo app namespace              (default: default)
#   AI_DEPLOYMENT          AI Decision Service deployment  (default: ai-decision-service)
#   OPERATOR_DEPLOYMENT    operator deployment             (default: auto-detected)
#   APP_SELECTOR           the app's pod label             (default: app=nginx-2)
#   DEMO_COOLDOWN_SECONDS  cooldown while a scenario waits (default: 20)
#   NODE_FAILURE_REPLICAS  replicas in node-failure        (default: 3)
#   NODE_DOWN_CMD          stops a node; %s is its name    (default: docker exec %s systemctl stop kubelet)
#   NODE_UP_CMD            restarts a node; %s is its name (default: docker exec %s systemctl start kubelet)

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$SCRIPT_DIR/lib/demo_common.sh"

AI_DEPLOYMENT=${AI_DEPLOYMENT:-ai-decision-service}
OPERATOR_DEPLOYMENT=${OPERATOR_DEPLOYMENT:-}
APP_SELECTOR=${APP_SELECTOR:-app=nginx-2}
DEMO_COOLDOWN_SECONDS=${DEMO_COOLDOWN_SECONDS:-20}
NODE_FAILURE_REPLICAS=${NODE_FAILURE_REPLICAS:-3}
NODE_DOWN_CMD=${NODE_DOWN_CMD:-docker exec %s systemctl stop kubelet}
NODE_UP_CMD=${NODE_UP_CMD:-docker exec %s systemctl start kubelet}
DEMO_AUTO=${DEMO_AUTO:-false}

POLL_SECONDS=3
DEMO_PDB=hiro-demo-pdb
DEMO_HPA=hiro-demo-hpa
DEMO_VPA=hiro-demo-vpa
CORDONED_LABEL=hiro-demo-cordoned

# name|what it shows. A new scenario is one line here plus a scenario_<name> function.
SCENARIOS=(
  "healthy|Healthy cluster: the service answers NoOp"
  "placement|Packed, Balanced and Spread scoring for a new pod"
  "energy-hold|Window closed: NoOp lists the hold, and a new pod waits behind the gate"
  "retry|A pod the gate held is retried once the window opens"
  "stuck-pod|A pod stuck on resources is left alone, not deleted in a loop"
  "node-failure|Every pod moves off a down node, ignoring the cooldown"
  "node-failure-hold|Under a hold the Move is refused and the pod stays, until the window opens"
  "node-failure-no-target|No healthy node to move to: NoOp, until a node is uncordoned"
  "node-failure-pdb|A PDB blocks the eviction: Deferred, until the PDB is gone"
  "dry-run|Dry-run reports the Move but evicts nothing"
  "gates|NoOp lists what the gates blocked (externally managed, PDB)"
  "escalate|AI service down: repeated failures pause the loop, then recovery"
  "autoscaler-hint|An undeclared HPA or VPA is flagged in the status"
)

SELECTED=()
RESULTS=()
PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0
CURRENT_SCENARIO=""
BASELINE_AT=""
WANT_WINDOW=""
KEEP_COOLDOWN_CLEAR=false
DOWN_NODE=""
NODE_DOWN_CONFIRMED=false
ORIG_REBALANCING=""
ORIG_PLACEMENT=""
AI_ORIGINAL_REPLICAS=""
FOUND_ENTRY=""
HELD_POD=""
STUCK_POD=""
NEW_POD=""
FAILED_NODE=""
FAILED_POD=""
AI_LINE=""

# ─── Results ───────────────────────────────────────────────────────────────

pass() {
  PASS_COUNT=$((PASS_COUNT + 1))
  RESULTS+=("PASS  $CURRENT_SCENARIO: $*")
  printf '  \033[32mPASS\033[0m %s\n' "$*"
}

fail() {
  FAIL_COUNT=$((FAIL_COUNT + 1))
  RESULTS+=("FAIL  $CURRENT_SCENARIO: $*")
  printf '  \033[31mFAIL\033[0m %s\n' "$*"
  show_recent_history
}

skip() {
  SKIP_COUNT=$((SKIP_COUNT + 1))
  RESULTS+=("SKIP  $CURRENT_SCENARIO: $*")
  printf '  \033[33mSKIP\033[0m %s\n' "$*"
}

expect_equal() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (got '$2', want '$3')"; fi
}

expect_contains() {
  case "$2" in
    *"$3"*) pass "$1" ;;
    *) fail "$1 (no '$3' in '$2')" ;;
  esac
}

expect_lacks() {
  case "$2" in
    *"$3"*) fail "$1 (found '$3' in '$2')" ;;
    *) pass "$1" ;;
  esac
}

print_summary() {
  [ ${#RESULTS[@]} -gt 0 ] || return 0
  step "Summary"
  local result
  for result in "${RESULTS[@]}"; do
    printf '  %s\n' "$result"
  done
  printf '\n  %s passed, %s failed, %s skipped\n' "$PASS_COUNT" "$FAIL_COUNT" "$SKIP_COUNT"
}

die() {
  printf '\033[31m%s\033[0m\n' "$*" >&2
  exit 1
}

# ─── Reading the profile ───────────────────────────────────────────────────

rs_field() {
  kubectl get orchestrationprofile "$PROFILE" -o jsonpath="{.status.rebalancingStatus.$1}" 2>/dev/null || true
}

condition_status() {
  kubectl get orchestrationprofile "$PROFILE" \
    -o jsonpath="{.status.conditions[?(@.type==\"$1\")].status}" 2>/dev/null || true
}

# Fields: 1 at, 2 action, 3 outcome, 4 reason, 5 trigger, 6 decidedBy, 7 pod, 8 source, 9 target, 10 newNode, 11 message.
history_lines() {
  kubectl get orchestrationprofile "$PROFILE" -o jsonpath='{range .status.rebalancingStatus.history[*]}{.at}{"|"}{.action}{"|"}{.outcome}{"|"}{.reason}{"|"}{.trigger}{"|"}{.decidedBy}{"|"}{.podName}{"|"}{.sourceNode}{"|"}{.targetNode}{"|"}{.newPodNode}{"|"}{.message}{"\n"}{end}' 2>/dev/null || true
}

newest_at() {
  history_lines | awk -F'|' 'NR==1 {print $1}'
}

# Compared with the operator's own timestamps, so clock differences don't matter.
set_baseline() {
  BASELINE_AT=$(newest_at)
}

new_entries() {
  history_lines | awk -F'|' -v b="$BASELINE_AT" '$1 > b'
}

# find_entry <action> <outcome> <reason> <trigger> [text]: the newest new entry that matches; "" matches anything.
find_entry() {
  new_entries | awk -F'|' -v a="${1:-}" -v o="${2:-}" -v r="${3:-}" -v t="${4:-}" -v m="${5:-}" '
    !found && (a=="" || $2==a) && (o=="" || $3==o) && (r=="" || $4==r) && (t=="" || $5==t) && (m=="" || index($0, m)) {
      print; found=1
    }'
}

entry_found() {
  FOUND_ENTRY=$(find_entry "$@")
  [ -n "$FOUND_ENTRY" ]
}

entry_field() {
  printf '%s\n' "$1" | cut -d'|' -f"$2"
}

show_entry() {
  printf '%s\n' "$1" | awk -F'|' '{
    printf "       %s %s/%s trigger=%s decidedBy=%s", $2, $3, $4, $5, $6
    if ($7 != "") printf " pod=%s", $7
    if ($9 != "") printf " target=%s", $9
    if ($10 != "") printf " landedOn=%s", $10
    printf "\n       %s\n", $11
  }'
}

show_recent_history() {
  echo "       Latest history:"
  history_lines | awk -F'|' 'NR<=3 {printf "         %s %s %s/%s trigger=%s: %s\n", $1, $2, $3, $4, $5, $11}'
}

ai_decision_line() {
  kubectl logs "deployment/$AI_DEPLOYMENT" -n "$NAMESPACE" --since=15m 2>/dev/null \
    | grep '"msg":"Decision made"' | grep -F -- "$1" | tail -1 || true
}

ai_decision_found() {
  AI_LINE=$(ai_decision_line "$1")
  [ -n "$AI_LINE" ]
}

show_ai_decision() {
  local line
  line=$(ai_decision_line "\"flow\":\"$1\"")
  [ -n "$line" ] && printf '       AI service said: %s\n' "$(printf '%s' "$line" | sed 's/.*"response"://' | cut -c1-200)"
  return 0
}

# ─── Waiting ───────────────────────────────────────────────────────────────

# The real EAO controller overwrites window patches, so the wanted window is re-asserted on every poll.
reassert() {
  case "$WANT_WINDOW" in
    open) DEMO_QUIET=true open_energy_window || true ;;
    closed) DEMO_QUIET=true close_energy_window || true ;;
  esac
  if [ "$KEEP_COOLDOWN_CLEAR" = true ]; then
    DEMO_QUIET=true clear_cooldown || true
  fi
}

# wait_until <seconds> <what> <command...>
wait_until() {
  local timeout=$1 what=$2 waited=0
  shift 2
  printf '  Waiting up to %ss: %s ' "$timeout" "$what"
  while [ "$waited" -lt "$timeout" ]; do
    reassert
    if "$@"; then
      printf 'done (%ss)\n' "$waited"
      return 0
    fi
    printf '.'
    sleep "$POLL_SECONDS"
    waited=$((waited + POLL_SECONDS))
  done
  printf 'timed out\n'
  return 1
}

# expect_entry <what> <seconds> <action> <outcome> <reason> <trigger> [text]
expect_entry() {
  local what=$1 timeout=$2
  shift 2
  if wait_until "$timeout" "$what" entry_found "$@"; then
    pass "$what"
    show_entry "$FOUND_ENTRY"
    return 0
  fi
  fail "$what"
  return 1
}

want_window() {
  WANT_WINDOW=$1
  echo "  Energy window: $1"
  reassert
}

profile_is_watching() {
  local state
  state=$(profile_state || true)
  [ -z "$state" ] || [ "$state" = "Watching" ]
}

cooldown_armed() {
  [ -n "$(rs_field cooldownUntil)" ]
}

# ─── The app's pods ────────────────────────────────────────────────────────

# Live pods only, as name|node|phase; a pod being deleted doesn't count.
live_app_pods() {
  kubectl get pods -n "$APP_NAMESPACE" -l "$APP_SELECTOR" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.spec.nodeName}{"|"}{.status.phase}{"|"}{.metadata.deletionTimestamp}{"\n"}{end}' 2>/dev/null \
    | awk -F'|' '$4=="" {print $1"|"$2"|"$3}' || true
}

live_pod_names() {
  live_app_pods | awk -F'|' '{print $1}' | tr '\n' ' '
}

pods_on_node() {
  live_app_pods | awk -F'|' -v n="$1" '$2==n {print $1}'
}

no_live_pods_on_node() {
  [ -z "$(pods_on_node "$1")" ]
}

pod_is_live() {
  live_app_pods | awk -F'|' -v p="$1" '$1==p {found=1} END {exit !found}'
}

pod_gone() {
  ! kubectl get pod "$1" -n "$APP_NAMESPACE" >/dev/null 2>&1
}

live_pod_count_is() {
  [ "$(live_app_pods | wc -l | tr -d ' ')" = "$1" ]
}

pod_schedule_message() {
  kubectl get pod "$1" -n "$APP_NAMESPACE" \
    -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].message}' 2>/dev/null || true
}

unscheduled_pods() {
  live_app_pods | awk -F'|' '$3=="Pending" && $2=="" {print $1}'
}

held_pod_found() {
  local pod
  HELD_POD=""
  for pod in $(unscheduled_pods); do
    case "$(pod_schedule_message "$pod")" in
      *"energy gate: "*) HELD_POD=$pod; return 0 ;;
    esac
  done
  return 1
}

stuck_pod_found() {
  local pod
  STUCK_POD=""
  for pod in $(unscheduled_pods); do
    if [ -n "$(pod_schedule_message "$pod")" ]; then
      STUCK_POD=$pod
      return 0
    fi
  done
  return 1
}

# new_scheduled_pod "<names before>": sets NEW_POD to a pod that appeared since, once it has a node.
new_scheduled_pod() {
  local line name node
  NEW_POD=""
  for line in $(live_app_pods); do
    name=${line%%|*}
    node=$(printf '%s' "$line" | cut -d'|' -f2)
    case " $1 " in
      *" $name "*) ;;
      *) if [ -n "$node" ]; then NEW_POD=$name; return 0; fi ;;
    esac
  done
  return 1
}

app_rollout_done() {
  kubectl rollout status deployment "$APP" -n "$APP_NAMESPACE" --timeout=1s >/dev/null 2>&1
}

scale_app() {
  capture_original_replicas
  run kubectl scale deployment "$APP" -n "$APP_NAMESPACE" --replicas="$1" >/dev/null
}

# ─── Changing the profile ──────────────────────────────────────────────────

save_profile_spec() {
  [ -z "$ORIG_REBALANCING" ] || return 0
  ORIG_REBALANCING=$(kubectl get orchestrationprofile "$PROFILE" -o jsonpath='{.spec.rebalancing}')
  ORIG_PLACEMENT=$(kubectl get orchestrationprofile "$PROFILE" -o jsonpath='{.spec.placement}')
}

patch_rebalancing() {
  save_profile_spec
  run kubectl patch orchestrationprofile "$PROFILE" --type=merge -p "{\"spec\":{\"rebalancing\":$1}}" >/dev/null
}

set_strategy() {
  save_profile_spec
  run kubectl patch orchestrationprofile "$PROFILE" --type=merge \
    -p "{\"spec\":{\"placement\":{\"strategy\":\"$1\"}}}" >/dev/null
}

# For scenarios that don't test the gate: the real EAO controller's writes then can't refuse a Move, hold a pod or fire EnergyThreshold.
disable_energy_awareness() {
  save_profile_spec
  echo "  Energy awareness: off"
  run kubectl patch orchestrationprofile "$PROFILE" --type=merge \
    -p '{"spec":{"placement":{"awareness":{"energy":false}}}}' >/dev/null
}

restore_profile_spec() {
  [ -n "$ORIG_REBALANCING" ] || return 0
  echo "  Restoring $PROFILE's spec"
  run kubectl patch orchestrationprofile "$PROFILE" --type=json -p "[
    {\"op\":\"replace\",\"path\":\"/spec/rebalancing\",\"value\":$ORIG_REBALANCING},
    {\"op\":\"replace\",\"path\":\"/spec/placement\",\"value\":$ORIG_PLACEMENT}]" >/dev/null
  ORIG_REBALANCING=""
  ORIG_PLACEMENT=""
}

# ─── Nodes ─────────────────────────────────────────────────────────────────

node_ready_status() {
  kubectl get node "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true
}

node_not_ready() {
  [ "$(node_ready_status "$1")" != "True" ]
}

node_recovered() {
  [ "$(node_ready_status "$1")" = "True" ] || return 1
  case "$(kubectl get node "$1" -o jsonpath='{.spec.taints[*].key}' 2>/dev/null || true)" in
    *not-ready*|*unreachable*) return 1 ;;
  esac
  return 0
}

worker_nodes() {
  kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o name 2>/dev/null | sed 's#^node/##' || true
}

# Nodes that must stay up: downing the decider or the control plane would fail a scenario for the wrong reason.
protected_nodes() {
  kubectl get pods -n "$NAMESPACE" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null || true
  kubectl get nodes -l node-role.kubernetes.io/control-plane -o name 2>/dev/null | sed 's#^node/##' || true
}

pick_failure_node() {
  local protected node
  protected=$(protected_nodes)
  for node in $(live_app_pods | awk -F'|' '$2!="" {print $2}' | sort -u); do
    if ! printf '%s\n' "$protected" | grep -qx "$node"; then
      echo "$node"
      return 0
    fi
  done
  return 0
}

# Scales to $1 replicas (default: as it is) and picks a safe node hosting a pod; else skips.
prepare_failure_node() {
  capture_original_replicas
  local replicas=${1:-$APP_ORIGINAL_REPLICAS}
  scale_app "$replicas"
  wait_until 120 "$APP to reach $replicas replica(s)" app_rollout_done || true
  FAILED_NODE=$(pick_failure_node)
  if [ -z "$FAILED_NODE" ]; then
    replicas=$((replicas + 1))
    scale_app "$replicas"
    wait_until 120 "$APP to reach $replicas replicas, so one lands on another node" app_rollout_done || true
    FAILED_NODE=$(pick_failure_node)
  fi
  if [ -z "$FAILED_NODE" ]; then
    skip "no worker hosts $APP without also hosting pods from $NAMESPACE. To free one: kubectl cordon <worker>, kubectl rollout restart deployment -n $NAMESPACE, kubectl uncordon <worker>"
    return 1
  fi
  FAILED_POD=$(pods_on_node "$FAILED_NODE" | awk 'NR==1')
  echo "  Node to stop: $FAILED_NODE (hosts $FAILED_POD)"
}

confirm_node_down() {
  if [ "$DEMO_AUTO" = true ] || [ "$NODE_DOWN_CONFIRMED" = true ]; then
    return 0
  fi
  local count answer
  count=$(kubectl get pods -A --field-selector "spec.nodeName=$1" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  narrate "This stops the kubelet on $1, which runs $count pods. They keep running, but Kubernetes
evicts them after 5 minutes NotReady. The node comes back as soon as the scenario ends."
  read -rp "Continue? [y/N] " answer < /dev/tty || answer=n
  case "$answer" in
    y|Y) NODE_DOWN_CONFIRMED=true; return 0 ;;
    *) return 1 ;;
  esac
}

node_down() {
  if ! confirm_node_down "$1"; then
    skip "stopping $1 was declined"
    return 1
  fi
  DOWN_NODE=$1
  # shellcheck disable=SC2059
  if ! run bash -c "$(printf "$NODE_DOWN_CMD" "$1")"; then
    fail "could not stop $1 (set NODE_DOWN_CMD if this isn't Kind)"
    DOWN_NODE=""
    return 1
  fi
  if ! wait_until 120 "$1 to report NotReady" node_not_ready "$1"; then
    fail "$1 never reported NotReady"
    return 1
  fi
}

restore_node() {
  [ -n "$DOWN_NODE" ] || return 0
  echo "  Restarting the kubelet on $DOWN_NODE"
  # shellcheck disable=SC2059
  run bash -c "$(printf "$NODE_UP_CMD" "$DOWN_NODE")" || true
  wait_until 180 "$DOWN_NODE to recover" node_recovered "$DOWN_NODE" || true
  DOWN_NODE=""
}

cordon_other_workers() {
  local node
  for node in $(worker_nodes); do
    if [ "$node" != "$1" ]; then
      run kubectl cordon "$node" >/dev/null
      run kubectl label node "$node" "$CORDONED_LABEL=true" --overwrite >/dev/null
    fi
  done
}

uncordon_demo_nodes() {
  local node
  for node in $(kubectl get nodes -l "$CORDONED_LABEL=true" -o name 2>/dev/null | sed 's#^node/##' || true); do
    run kubectl uncordon "$node" >/dev/null
    run kubectl label node "$node" "$CORDONED_LABEL-" >/dev/null
  done
}

# ─── Demo objects ──────────────────────────────────────────────────────────

create_pdb() {
  run kubectl apply -f - >/dev/null <<EOF
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: $DEMO_PDB
  namespace: $APP_NAMESPACE
spec:
  minAvailable: "100%"
  selector:
    matchLabels:
      ${APP_SELECTOR%%=*}: ${APP_SELECTOR#*=}
EOF
}

delete_pdb() {
  run kubectl delete pdb "$DEMO_PDB" -n "$APP_NAMESPACE" --ignore-not-found >/dev/null
}

create_hpa() {
  run kubectl apply -f - >/dev/null <<EOF
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: $DEMO_HPA
  namespace: $APP_NAMESPACE
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: $APP
  minReplicas: 1
  maxReplicas: 3
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 80
EOF
}

delete_hpa() {
  run kubectl delete hpa "$DEMO_HPA" -n "$APP_NAMESPACE" --ignore-not-found >/dev/null
}

vpa_installed() {
  kubectl api-resources --api-group=autoscaling.k8s.io -o name 2>/dev/null | grep -q verticalpodautoscalers
}

# create_vpa <updateMode>
create_vpa() {
  run kubectl apply -f - >/dev/null <<EOF
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata:
  name: $DEMO_VPA
  namespace: $APP_NAMESPACE
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: $APP
  updatePolicy:
    updateMode: "$1"
EOF
}

delete_demo_objects() {
  delete_pdb || true
  delete_hpa || true
  if vpa_installed; then
    run kubectl delete vpa "$DEMO_VPA" -n "$APP_NAMESPACE" --ignore-not-found >/dev/null || true
  fi
}

scale_ai_service_down() {
  AI_ORIGINAL_REPLICAS=$(kubectl get deployment "$AI_DEPLOYMENT" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}')
  run kubectl scale deployment "$AI_DEPLOYMENT" -n "$NAMESPACE" --replicas=0 >/dev/null
  run kubectl rollout status deployment "$AI_DEPLOYMENT" -n "$NAMESPACE" --timeout=60s >/dev/null || true
}

restore_ai_service() {
  [ -n "$AI_ORIGINAL_REPLICAS" ] || return 0
  echo "  Restoring $AI_DEPLOYMENT to $AI_ORIGINAL_REPLICAS replica(s)"
  run kubectl scale deployment "$AI_DEPLOYMENT" -n "$NAMESPACE" --replicas="$AI_ORIGINAL_REPLICAS" >/dev/null
  run kubectl rollout status deployment "$AI_DEPLOYMENT" -n "$NAMESPACE" --timeout=120s >/dev/null || true
  AI_ORIGINAL_REPLICAS=""
}

# ─── Restore (after every scenario, and on Ctrl-C) ─────────────────────────

restore_all() {
  step "Restoring the cluster"
  WANT_WINDOW=""
  KEEP_COOLDOWN_CLEAR=false
  restore_node
  uncordon_demo_nodes
  delete_demo_objects
  restore_ai_service
  ensure_not_escalated || true
  restore_profile_spec
  if [ "$RESOURCES_TOUCHED" = true ]; then
    revert_resources
    RESOURCES_TOUCHED=false
  fi
  if [ -n "$APP_ORIGINAL_REPLICAS" ]; then
    revert_replicas
    APP_ORIGINAL_REPLICAS=""
  fi
  if [ "$EAO_TOUCHED" = true ]; then
    revert_eao_window
    EAO_TOUCHED=false
  fi
  clear_cooldown || true
}

on_exit() {
  local code=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$CURRENT_SCENARIO" ]; then
    restore_all
  fi
  print_summary
  if [ "$FAIL_COUNT" -gt 0 ]; then
    exit 1
  fi
  exit "$code"
}

# For a run that was killed, so nothing is remembered: restores from the cluster and config/samples.
recover_killed_run() {
  local node
  set +e
  step "Recovering from a killed run"
  for node in $(worker_nodes); do
    if node_not_ready "$node"; then
      echo "  $node is NotReady: restarting its kubelet"
      # shellcheck disable=SC2059
      run bash -c "$(printf "$NODE_UP_CMD" "$node")" || true
      wait_until 180 "$node to recover" node_recovered "$node" || true
    fi
  done
  uncordon_demo_nodes
  delete_demo_objects
  if [ "$(kubectl get deployment "$AI_DEPLOYMENT" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || true)" = "0" ]; then
    run kubectl scale deployment "$AI_DEPLOYMENT" -n "$NAMESPACE" --replicas=1 >/dev/null
  fi
  open_energy_window || true
  run kubectl apply -f "$SCRIPT_DIR/../config/samples/orchestrationprofile_2.yaml" >/dev/null
  run kubectl apply -n "$APP_NAMESPACE" -f "$SCRIPT_DIR/../config/samples/nginx_deployment_2.yaml" >/dev/null
  await_app_rollout
  clear_escalation || true
  clear_cooldown || true
  echo "  Done. The real EAO controller sets $EAO's status again on its own schedule."
}

# ─── Scenarios ─────────────────────────────────────────────────────────────

scenario_healthy() {
  narrate "Nothing is wrong, so the service should answer NoOp from its rules."
  pause
  disable_energy_awareness
  KEEP_COOLDOWN_CLEAR=true
  expect_entry "A healthy cluster gets NoOp from the periodic review" 60 NoOp NoOp NoChange PeriodicReview || return 0
  expect_equal "The service's rules decided" "$(entry_field "$FOUND_ENTRY" 6)" "rules"
  show_ai_decision rebalance
}

scenario_placement() {
  local strategy before uid
  narrate "Each strategy scores a new pod. The service logs its answer under the pod's UID."
  pause
  disable_energy_awareness
  capture_original_replicas
  for strategy in Packed Balanced Spread; do
    step "Strategy: $strategy"
    set_strategy "$strategy"
    before=$(live_pod_names)
    scale_app $((APP_ORIGINAL_REPLICAS + 1))
    if ! wait_until 90 "a new pod to schedule" new_scheduled_pod "$before"; then
      fail "$strategy: no new pod scheduled"
      continue
    fi
    uid=$(kubectl get pod "$NEW_POD" -n "$APP_NAMESPACE" -o jsonpath='{.metadata.uid}')
    if wait_until 20 "the service's decision for $NEW_POD" ai_decision_found "\"requestId\":\"$uid\""; then
      expect_contains "$strategy: placement flow" "$AI_LINE" '"flow":"placement"'
      expect_contains "$strategy: the scorer decided" "$AI_LINE" '"decidedBy":"scorer"'
      expect_contains "$strategy: the reason names the strategy" "$AI_LINE" "\"reason\":\"$strategy:"
      printf '       %s\n' "$(printf '%s' "$AI_LINE" | sed 's/.*"response"://' | cut -c1-200)"
    else
      fail "$strategy: no decision logged for $NEW_POD (kube-scheduler skips scoring when only one node fits)"
    fi
    scale_app "$APP_ORIGINAL_REPLICAS"
    wait_until 60 "$APP to scale back" live_pod_count_is "$APP_ORIGINAL_REPLICAS" || true
  done
}

scenario_energy_hold() {
  local running pod
  narrate "With the window closed, EnergyThreshold asks the service, whose gate blocks Move. A new pod waits behind the gate."
  pause
  running=$(live_pod_names)
  want_window closed
  KEEP_COOLDOWN_CLEAR=true
  capture_original_replicas
  expect_entry "The service answers NoOp and names the hold" 60 NoOp NoOp NoChange EnergyThreshold "EAO holds until"
  KEEP_COOLDOWN_CLEAR=false
  scale_app $((APP_ORIGINAL_REPLICAS + 1))
  if wait_until 60 "a new pod to be held by the gate" held_pod_found; then
    pass "A new pod is held: $(pod_schedule_message "$HELD_POD")"
  else
    fail "No new pod was held by the energy gate"
  fi
  for pod in $running; do
    if pod_is_live "$pod"; then pass "Running pod $pod is untouched"; else fail "Running pod $pod was removed"; fi
  done
  show_ai_decision rebalance
}

scenario_retry() {
  narrate "A pod held behind the closed window is deleted once it opens, so its replacement schedules now."
  pause
  capture_original_replicas
  want_window closed
  scale_app $((APP_ORIGINAL_REPLICAS + 1))
  if ! wait_until 60 "a new pod to be held by the gate" held_pod_found; then
    fail "No new pod was held by the energy gate"
    return 0
  fi
  pass "$HELD_POD is held by the gate"
  set_baseline
  want_window open
  expect_entry "The held pod is retried without asking the service" 90 \
    RetryPendingSchedule Enacted Retried EnergyThreshold || return 0
  expect_equal "No AI decision was involved" "$(entry_field "$FOUND_ENTRY" 6)" ""
  if pod_gone "$HELD_POD"; then pass "$HELD_POD was deleted"; else fail "$HELD_POD still exists"; fi
  if wait_until 90 "the replacement to run" app_rollout_done; then
    pass "The replacement is running"
  else
    fail "The replacement never ran"
  fi
}

scenario_stuck_pod() {
  narrate "A pod that can't fit is not the gate's doing, so it must be left alone even with the window open."
  pause
  want_window open
  capture_original_resources
  run kubectl set resources deployment "$APP" -n "$APP_NAMESPACE" -c="$APP_CONTAINER_NAME" \
    --requests=cpu=1000 --limits=cpu=1000 >/dev/null
  if ! wait_until 60 "a new pod to be turned away" stuck_pod_found; then
    fail "No pod was turned away by the scheduler"
    return 0
  fi
  local message
  message=$(pod_schedule_message "$STUCK_POD")
  printf '       %s: %s\n' "$STUCK_POD" "$message"
  expect_lacks "The pod wasn't held by the energy gate" "$message" "energy gate: "
  if wait_until 90 "$STUCK_POD to be deleted (it must not be)" pod_gone "$STUCK_POD"; then
    fail "$STUCK_POD was deleted"
  else
    pass "$STUCK_POD is still there after 90s"
  fi
  expect_equal "No retry was attempted" "$(find_entry RetryPendingSchedule)" ""
}

enacted_node_failure_moves() {
  new_entries | awk -F'|' '$2=="Move" && $3=="Enacted" && $4=="Moved" && $5=="NodeFailure"'
}

# A pod counts as gone once evicted, but its Move is only recorded after the replacement lands.
moves_recorded() {
  [ "$(enacted_node_failure_moves | grep -c . || true)" -ge "$1" ]
}

scenario_node_failure() {
  local expected moved not_by_rules onto_failed
  narrate "NodeFailure goes first: every pod moves off the down node, one per cycle, ignoring the shared cooldown."
  pause
  patch_rebalancing '{"cooldownSeconds":300}'
  disable_energy_awareness
  prepare_failure_node "$NODE_FAILURE_REPLICAS" || return 0
  expected=$(pods_on_node "$FAILED_NODE" | wc -l | tr -d ' ')
  clear_cooldown
  if ! wait_until 60 "a normal cycle to arm the 300s cooldown" cooldown_armed; then
    fail "The shared cooldown was never armed"
    return 0
  fi
  set_baseline
  node_down "$FAILED_NODE" || return 0
  # Stop here on failure: waiting longer keeps the node down until Kubernetes evicts its pods at 5 minutes.
  if ! wait_until 180 "all $expected pod(s) to leave $FAILED_NODE" no_live_pods_on_node "$FAILED_NODE"; then
    fail "Pods are still on $FAILED_NODE: $(pods_on_node "$FAILED_NODE" | tr '\n' ' ')"
    return 0
  fi
  pass "All $expected pod(s) left $FAILED_NODE, inside the 300s cooldown"
  wait_until 60 "$expected Move(s) to be recorded" moves_recorded "$expected" || true
  moved=$(enacted_node_failure_moves)
  expect_equal "One Move per pod" "$(printf '%s' "$moved" | grep -c . || true)" "$expected"
  if [ -n "$moved" ]; then
    not_by_rules=$(printf '%s\n' "$moved" | awk -F'|' '$6!="rules"')
    expect_equal "The service's rules decided every Move" "$not_by_rules" ""
    onto_failed=$(printf '%s\n' "$moved" | awk -F'|' -v n="$FAILED_NODE" '$10==n')
    expect_equal "No replacement landed on $FAILED_NODE" "$onto_failed" ""
    show_entry "$(printf '%s\n' "$moved" | awk 'NR==1')"
  fi
  if wait_until 90 "$APP to be fully available" app_rollout_done; then
    pass "$APP is fully available"
  else
    fail "$APP isn't fully available"
  fi
}

scenario_node_failure_hold() {
  local failures
  narrate "The service still says Move off a down node, but the operator refuses: the gate would hold the replacement."
  pause
  patch_rebalancing "{\"cooldownSeconds\":$DEMO_COOLDOWN_SECONDS}"
  want_window open
  prepare_failure_node || return 0
  set_baseline
  want_window closed
  node_down "$FAILED_NODE" || return 0
  expect_entry "The Move is refused under the hold" 120 Move Rejected EnergyGateClosed NodeFailure || return 0
  if pod_is_live "$FAILED_POD"; then pass "$FAILED_POD wasn't evicted"; else fail "$FAILED_POD was evicted"; fi
  failures=$(rs_field consecutiveFailures)
  expect_equal "A hold isn't counted as a failure" "${failures:-0}" "0"
  set_baseline
  want_window open
  expect_entry "Once the window opens, the pod moves" 120 Move Enacted Moved NodeFailure
}

scenario_node_failure_no_target() {
  narrate "With every other worker cordoned, no node can take the pod, so the service answers NoOp."
  pause
  patch_rebalancing "{\"cooldownSeconds\":$DEMO_COOLDOWN_SECONDS}"
  disable_energy_awareness
  prepare_failure_node || return 0
  cordon_other_workers "$FAILED_NODE"
  set_baseline
  node_down "$FAILED_NODE" || return 0
  expect_entry "The service finds no healthy node" 120 NoOp NoOp NoChange NodeFailure "no healthy node can take pod" || return 0
  if pod_is_live "$FAILED_POD"; then pass "$FAILED_POD wasn't evicted"; else fail "$FAILED_POD was evicted"; fi
  uncordon_demo_nodes
  set_baseline
  expect_entry "Once a node is uncordoned, the pod moves" 120 Move Enacted Moved NodeFailure
}

scenario_node_failure_pdb() {
  narrate "Leaving a down node skips the service's PDB gate, but the eviction API still enforces the PDB."
  pause
  patch_rebalancing "{\"cooldownSeconds\":$DEMO_COOLDOWN_SECONDS,\"escalationThreshold\":10}"
  disable_energy_awareness
  prepare_failure_node || return 0
  create_pdb
  set_baseline
  node_down "$FAILED_NODE" || return 0
  expect_entry "The PDB blocks the eviction" 120 Move Deferred PDBBlocked NodeFailure || return 0
  if pod_is_live "$FAILED_POD"; then pass "$FAILED_POD wasn't evicted"; else fail "$FAILED_POD was evicted"; fi
  delete_pdb
  set_baseline
  expect_entry "Once the PDB is gone, the pod moves" 120 Move Enacted Moved NodeFailure
}

dry_run_reported() {
  case "$(rs_field details)" in
    *"[dry-run] would move"*) return 0 ;;
  esac
  return 1
}

scenario_dry_run() {
  narrate "Dry-run goes through the same decision and reports the Move, but nothing is evicted."
  pause
  patch_rebalancing "{\"dryRun\":true,\"cooldownSeconds\":$DEMO_COOLDOWN_SECONDS}"
  disable_energy_awareness
  prepare_failure_node || return 0
  set_baseline
  node_down "$FAILED_NODE" || return 0
  KEEP_COOLDOWN_CLEAR=true
  if wait_until 120 "the dry-run report" dry_run_reported; then
    pass "Dry-run reports the Move"
    printf '       %s\n' "$(rs_field details)"
  else
    fail "No dry-run report"
  fi
  if pod_is_live "$FAILED_POD"; then pass "$FAILED_POD wasn't evicted"; else fail "$FAILED_POD was evicted"; fi
  expect_equal "A dry-run writes no Move to history" "$(find_entry Move)" ""
}

scenario_gates() {
  local message
  narrate "With no rule to apply, the NoOp says what the gates blocked."
  pause
  disable_energy_awareness
  patch_rebalancing '{"replicasExternallyManaged":true,"resourcesExternallyManaged":true}'
  create_pdb
  set_baseline
  KEEP_COOLDOWN_CLEAR=true
  expect_entry "The NoOp lists the blocked actions" 60 NoOp NoOp NoChange PeriodicReview \
    "resources are externally managed" || return 0
  message=$(entry_field "$FOUND_ENTRY" 11-)
  expect_contains "Replicas are blocked" "$message" "replicas are externally managed"
  expect_contains "Move is blocked by the PDB" "$message" "PDB $DEMO_PDB does not allow disruptions"
}

scenario_escalate() {
  local at
  narrate "The service is down, so cycles fail. Reaching the threshold pauses the loop until a human clears it."
  pause
  disable_energy_awareness
  clear_escalation
  patch_rebalancing "{\"cooldownSeconds\":$DEMO_COOLDOWN_SECONDS,\"escalationThreshold\":2}"
  scale_ai_service_down
  set_baseline
  KEEP_COOLDOWN_CLEAR=true
  expect_entry "A cycle fails with the service down" 60 "" Failed AIUnavailable PeriodicReview
  expect_entry "The second failure escalates" 60 "" Escalated FailureStreak PeriodicReview
  KEEP_COOLDOWN_CLEAR=false
  expect_equal "The profile is escalated" "$(profile_escalated)" "true"
  expect_equal "The status condition says so" "$(condition_status RebalancingEscalated)" "True"
  at=$(newest_at)
  clear_cooldown
  sleep 15
  expect_equal "The loop stays paused" "$(newest_at)" "$at"
  restore_ai_service
  # The Service refuses connections briefly after the pod is Ready; those failures mustn't pause the loop again.
  patch_rebalancing '{"escalationThreshold":10}'
  clear_escalation
  set_baseline
  KEEP_COOLDOWN_CLEAR=true
  expect_entry "After clearing it, the loop runs again" 60 NoOp NoOp NoChange PeriodicReview
}

hint_is() {
  [ "$(condition_status UndeclaredAutoscaler)" = "$1" ]
}

# expect_hint <what> <status>
expect_hint() {
  if wait_until 60 "$1" hint_is "$2"; then
    pass "$1 ($2)"
  else
    fail "$1 (got '$(condition_status UndeclaredAutoscaler)', want '$2')"
  fi
}

scenario_autoscaler_hint() {
  narrate "An autoscaler the profile doesn't declare could fight the operator. This only informs: no decision changes."
  pause
  save_profile_spec
  KEEP_COOLDOWN_CLEAR=true
  create_hpa
  expect_hint "An undeclared HPA is flagged" True
  printf '       %s\n' "$(kubectl get orchestrationprofile "$PROFILE" \
    -o jsonpath='{.status.conditions[?(@.type=="UndeclaredAutoscaler")].message}')"
  patch_rebalancing '{"replicasExternallyManaged":true}'
  expect_hint "Declaring it clears the flag" False
  patch_rebalancing '{"replicasExternallyManaged":false}'
  expect_hint "Undeclaring it flags it again" True
  delete_hpa
  expect_hint "Removing the HPA clears the flag" False
  if ! vpa_installed; then
    skip "VPA part: the VerticalPodAutoscaler CRD isn't installed"
    return 0
  fi
  create_vpa Off
  expect_hint "A VPA that only recommends isn't flagged" False
  create_vpa Recreate
  expect_hint "An active undeclared VPA is flagged" True
  run kubectl delete vpa "$DEMO_VPA" -n "$APP_NAMESPACE" --ignore-not-found >/dev/null
  expect_hint "Removing the VPA clears the flag" False
}

# ─── Selecting and running ─────────────────────────────────────────────────

list_scenarios() {
  local i=1 scenario
  for scenario in "${SCENARIOS[@]}"; do
    printf '  %2d) %-24s %s\n' "$i" "${scenario%%|*}" "${scenario#*|}"
    i=$((i + 1))
  done
}

usage() {
  echo "Usage: $0 [--auto] [all | <name or number>...]"
  echo "       $0 list | panes | cleanup"
  echo
  list_scenarios
}

scenario_description() {
  local scenario
  for scenario in "${SCENARIOS[@]}"; do
    if [ "${scenario%%|*}" = "$1" ]; then
      echo "${scenario#*|}"
      return 0
    fi
  done
}

# Echoes the scenario name for a name or a number; fails if there's no such scenario.
resolve_scenario() {
  local i=1 scenario
  for scenario in "${SCENARIOS[@]}"; do
    if [ "$1" = "$i" ] || [ "$1" = "${scenario%%|*}" ]; then
      echo "${scenario%%|*}"
      return 0
    fi
    i=$((i + 1))
  done
  return 1
}

# Fills SELECTED from names, numbers or "all"; fails on the first it doesn't know.
select_scenarios() {
  local token scenario name
  SELECTED=()
  for token in "$@"; do
    if [ "$token" = all ]; then
      for scenario in "${SCENARIOS[@]}"; do
        SELECTED+=("${scenario%%|*}")
      done
    elif name=$(resolve_scenario "$token"); then
      SELECTED+=("$name")
    else
      echo "Unknown scenario: '$token'" >&2
      return 1
    fi
  done
}

menu() {
  local answer
  {
    echo
    echo "Rebalance scenarios with the AI Decision Service:"
    list_scenarios
    echo "      all      every scenario, in order"
    echo "      q        quit"
    echo
    echo "Tip: '$0 panes' prints log commands to watch alongside."
  } > /dev/tty
  while true; do
    read -rp "Pick (6, or 1 3 5, or all): " answer < /dev/tty || exit 0
    case "$answer" in
      q|Q) exit 0 ;;
      "") continue ;;
    esac
    # shellcheck disable=SC2086
    if select_scenarios $answer; then
      return 0
    fi
  done
}

detect_operator_deployment() {
  if [ -z "$OPERATOR_DEPLOYMENT" ]; then
    OPERATOR_DEPLOYMENT=$(kubectl get deployment -n "$NAMESPACE" -o name 2>/dev/null \
      | grep controller-manager | awk 'NR==1' | sed 's#^.*/##' || true)
  fi
}

show_panes() {
  detect_operator_deployment
  cat <<EOF
Pane A, the state machine:
  kubectl get orchestrationprofile $PROFILE -w -o jsonpath='{.status.rebalancingStatus.lastTransitionAt}{" | "}{.status.rebalancingStatus.state}{" | "}{.status.rebalancingStatus.reason}{"\n"}'

Pane B, the operator's rebalance logs:
  kubectl logs -f deployment/${OPERATOR_DEPLOYMENT:-<operator>} -n $NAMESPACE | grep -i --line-buffered rebalance

Pane C, the service's decisions:
  kubectl logs -f deployment/$AI_DEPLOYMENT -n $NAMESPACE | grep --line-buffered 'Decision made'

Pane D, the app's pods:
  kubectl get pods -n $APP_NAMESPACE -l $APP_SELECTOR -o wide -w
EOF
}

preflight() {
  local ready url triggers trigger age
  step "Preflight"
  kubectl get deployment "$AI_DEPLOYMENT" -n "$NAMESPACE" >/dev/null 2>&1 \
    || die "$AI_DEPLOYMENT not found in $NAMESPACE. Deploy it: USE_AI_DECISION_SERVICE=true deploy/scripts/deploy_full_stack_with_helm.sh"
  ready=$(kubectl get deployment "$AI_DEPLOYMENT" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
  [ "${ready:-0}" -ge 1 ] || die "$AI_DEPLOYMENT has no ready replica."

  detect_operator_deployment
  [ -n "$OPERATOR_DEPLOYMENT" ] || die "No operator deployment found in $NAMESPACE. Set OPERATOR_DEPLOYMENT."

  # Helm sets the agent URL in a ConfigMap; kustomize sets it on the Deployment.
  url=$(kubectl get configmap -n "$NAMESPACE" -o jsonpath='{range .items[*]}{.data.DECISION_AGENT_URL}{end}' 2>/dev/null || true)
  if [ -z "$url" ]; then
    url=$(kubectl get deployment "$OPERATOR_DEPLOYMENT" -n "$NAMESPACE" \
      -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="DECISION_AGENT_URL")].value}' 2>/dev/null || true)
  fi
  case "$url" in
    *"$AI_DEPLOYMENT"*) echo "  Operator calls: $url" ;;
    "") echo "  Couldn't read the operator's DECISION_AGENT_URL; continuing." ;;
    *) die "The operator calls $url, not $AI_DEPLOYMENT." ;;
  esac

  kubectl get orchestrationprofile "$PROFILE" >/dev/null 2>&1 || die "Profile $PROFILE not found. Run: kubectl apply -k config/samples/"
  kubectl get deployment "$APP" -n "$APP_NAMESPACE" >/dev/null 2>&1 || die "$APP not found. Run: kubectl apply -k config/samples/"
  kubectl get energyawareorchestration "$EAO" -n "$APP_NAMESPACE" >/dev/null 2>&1 || die "EAO $EAO not found in $APP_NAMESPACE."

  triggers=$(kubectl get orchestrationprofile "$PROFILE" -o jsonpath='{.spec.rebalancing.triggerConditions}')
  for trigger in EnergyThreshold NodeFailure PeriodicReview; do
    case "$triggers" in
      *"$trigger"*) ;;
      *) die "$PROFILE must declare $trigger; it has $triggers. Run: kubectl apply -k config/samples/" ;;
    esac
  done
  [ "$(kubectl get orchestrationprofile "$PROFILE" -o jsonpath='{.spec.placement.awareness.energy}')" = "true" ] \
    || die "$PROFILE must set placement.awareness.energy: true. A killed run can leave it off: run $0 cleanup."

  age=$(kubectl logs "deployment/$OPERATOR_DEPLOYMENT" -n "$NAMESPACE" -c manager 2>/dev/null \
    | grep -m1 'rebalance engine configured' | grep -o '"minPendingPodAge": "[^"]*"' || true)
  if [ -n "$age" ]; then
    echo "  Operator setting: $age"
  else
    printf '  \033[33mThe operator log has no minPendingPodAge: it may predate the latest fixes, so redeploy it.\033[0m\n'
  fi
}

run_scenario() {
  local name=$1
  CURRENT_SCENARIO=$name
  step "Scenario: $name — $(scenario_description "$name")"
  ensure_not_escalated || true
  wait_until 60 "$PROFILE to be Watching" profile_is_watching || true
  set_baseline
  # Run in a condition, so one scenario's error is reported instead of ending the run.
  "scenario_${name//-/_}" || true
  pause
  restore_all || true
  CURRENT_SCENARIO=""
}

main() {
  local tokens=() token name
  for token in "$@"; do
    case "$token" in
      --auto) DEMO_AUTO=true ;;
      -h|--help|help) usage; exit 0 ;;
      list) list_scenarios; exit 0 ;;
      panes) show_panes; exit 0 ;;
      cleanup) recover_killed_run; exit 0 ;;
      *) tokens+=("$token") ;;
    esac
  done

  if [ ${#tokens[@]} -gt 0 ]; then
    select_scenarios "${tokens[@]}" || { usage >&2; exit 1; }
  elif [ -r /dev/tty ] && [ "$DEMO_AUTO" != true ]; then
    menu
  else
    usage >&2
    exit 1
  fi

  preflight
  trap on_exit EXIT
  trap 'exit 130' INT TERM
  for name in "${SELECTED[@]}"; do
    run_scenario "$name"
  done
}

main "$@"
