# Project Structure

Referenced from the main [README](../README.md#project-structure), which only covers the
top-level layout — this file has the full annotated source tree.

```
cmd/
  main.go                              # Operator entry point: manager + PlacementServer
api/v1alpha1/
  orchestrationprofile_types.go        # CRD schema
  zz_generated.deepcopy.go             # Auto-generated — DO NOT EDIT
pkg/
  placement/
    types.go                           # Shared wire types (PlacementContext, DecisionResponse,
                                       # NodeScore) — importable by both operator and scheduler
internal/
  controller/
    orchestrationprofile_controller.go # Main reconciler
    op_index.go                        # O(1) field index (ProfileByAppRefIndex)
    op_watchers.go                     # Pod/Workload → Profile event mapping
    op_validation.go                   # Spec validation
    op_status.go                       # Status computation + event recording
    op_pod_discovery.go                # Pod resolution (OwnerReference walk)
    op_constants.go                    # Status enum + event reason constants
  decision/
    server.go                          # PlacementServer HTTP service (:8090)
                                       #   POST /api/v1/placement/decision  (plugin path)
                                       #   POST /extender/filter            (extender filter)
                                       #   POST /extender/prioritize        (extender scoring)
    builder.go                         # Assembles DecisionRequest + CheckEnergyGate (incl. EAO profile fetch)
    client.go                          # HTTP client to external AI agent
    types.go                           # Type aliases → pkg/placement (zero churn)
    extender_types.go                  # Kubernetes scheduler extender protocol types
                                       #   ExtenderArgs, ExtenderFilterResult, HostPriorityList,
                                       #   HostPriority, EnergyGateResult
  rebalance/                           # See internal/rebalance/README.md for full details
    reconciler.go                      # Detection stage — hybrid periodic + event-driven triggers
    triggers.go                        # TriggerEvaluator — Energy/CPU/Memory/NodeFailure/Scheduled
    pressure.go                        # NodePressureEvaluator — CPU/Memory pressure via metrics-server
    writer.go                          # StateWriter — sole writer of status.rebalancingStatus
    state.go                           # Decision lifecycle state machine transition table
    engine.go                          # manager.Runnable registration
  webhook/v1/
    pod_webhook.go                     # PodCustomDefaulter — sets spec.schedulerName on governed pods
                                       # Soft-fail: profile lookup errors allow pod unchanged
  utils/
    helpers.go                         # ResolveAppFromPod, KeysOf, NodeNames
scheduler-plugin/                      # Separate Go module (own go.mod)
  plugin.go                            # HIROScore: FilterPlugin + PreScorePlugin + ScorePlugin
  client.go                            # PlacementClient (HTTP POST to operator)
  client_test.go                       # httptest-based unit tests
  cmd/
    main.go                            # hiro-scheduler binary entry point
  Dockerfile                           # Build context: repo root
  go.mod                               # k8s.io/kubernetes v1.35.0 + staging replace block
  pin_k8s_version.sh                   # Pin go.mod to a specific K8s minor version
config/
  crd/bases/                           # Generated CRDs — DO NOT EDIT
  rbac/                                # Generated RBAC — DO NOT EDIT
  manager/
    manager.yaml                       # Operator Deployment (env vars, ports, resources)
    placement_service_patch.yaml       # ClusterIP Service for PlacementServer (:8090)
  scheduler/
    kustomization.yaml                 # Kustomize base for scheduler manifests
    serviceaccount.yaml                # Scheduler ServiceAccount
    clusterrole.yaml                   # Scheduler RBAC (nodes, pods, bindings, leases…)
    clusterrolebinding.yaml
    configmap.yaml                     # KubeSchedulerConfiguration + HIROScore pluginConfig
    deployment.yaml                    # hiro-scheduler Deployment (2 replicas, HA)
  extender/
    scheduler-config.yaml              # Extender KubeSchedulerConfiguration template
                                       # (urlPrefix uses DNS; deploy_extender.sh substitutes ClusterIP)
  samples/                             # Example OrchestrationProfile + nginx Deployment
  components/
    metrics/                           # Kustomize Component — metrics Service + manager_metrics_patch.yaml
                                       # Shared by config/default and config/default-with-webhook
  default/                             # Kustomize base — operator only, no webhook
  default-with-webhook/                # Kustomize overlay — operator + MutatingWebhook + cert-manager TLS
                                       # Used by make deploy-scheduler; replacements: block derives all
                                       # Certificate dnsNames and MWC CA annotation from resource names
                                       # (no hardcoded values)
  webhook/
    service.yaml                       # Webhook Service (port 443 → 9443)
    manifests.yaml                     # MutatingWebhookConfiguration (failurePolicy: Ignore, CREATE only)
  certmanager/
    certificate-webhook.yaml           # cert-manager Certificate for webhook TLS
    certificate-metrics.yaml           # cert-manager Certificate for metrics TLS
    issuer.yaml                        # Self-signed Issuer
    kustomizeconfig.yaml               # nameReference: Issuer → Certificate.spec.issuerRef
hack/
  deploy_full_stack.sh                 # Full-stack entry point (phases 1–9) — all parameters defined here
  deploy_operator.sh                   # Operator-only deploy (base or webhook overlay per DEPLOY_SCHEDULER_PLUGIN)
  deploy_scheduler.sh                  # Scheduler-only deploy (build → push → kustomize → pull secret → rollout)
  deploy_extender.sh                   # Extender deploy: resolve ClusterIP → ConfigMap → privileged Job → kubelet restart
  undeploy_extender.sh                 # Roll back: restore original kube-scheduler manifest
  patch_scheduler_static_pod.py        # Patch Job script: adds --config, hostPath volume, and
                                       # hiro.io/last-updated annotation (triggers kubelet restart)
  mock_decision_agent.yaml             # In-cluster mock AI agent (Deployment + Service + ConfigMap)
dist/
  chart/                               # Generated Helm chart — DO NOT EDIT
  install.yaml                         # Generated single-file install bundle
test/e2e/                              # End-to-end tests (Kind)
```

## How this layout was scaffolded

Generated with [Kubebuilder](https://book.kubebuilder.io/):

```bash
kubebuilder init \
  --domain orchestration.hiro.io \
  --repo github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator \
  --owner "HIRO Adaptive Orchestrator"

kubebuilder create api \
  --group orchestration --version v1alpha1 --kind OrchestrationProfile \
  --resource=true --controller=true

kubebuilder edit --plugins=helm/v2-alpha
```

> Do **not** re-run these on an existing checkout — they modify project scaffolding. For
> regenerating against a newer Kubebuilder release, see [upgrading.md](upgrading.md#kubebuilder-version).
