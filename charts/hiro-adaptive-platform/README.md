# hiro-adaptive-platform

Umbrella Helm chart for the full HIRO Adaptive Orchestrator stack: the
operator (as a local subchart dependency on `dist/chart`), the pod-mutating
webhook, a custom scheduler (plugin or extender — pick one), an opt-in mock
decision agent for demos, and optional bundled cluster prerequisites
(cert-manager, metrics-server, kube-prometheus-stack).

It is an additive, parallel path to `hack/deploy_full_stack.sh` — see
[When to use this vs. the shell script](#when-to-use-this-vs-deploy_full_stacksh)
below.

## Requirements

- Helm >= 3.14 (Helm v4 works, but see [Post-renderer plugin
  (Helm v4)](#post-renderer-plugin-helm-v4) below — it changes how
  `--post-renderer` is invoked).
- `kubectl` and a reachable cluster for anything beyond `helm
  lint`/`helm template`.
- `kustomize` — either on `PATH`, or vendored at `bin/kustomize` (what `make
  kustomize` produces elsewhere in this repo). Only needed when
  `webhook.enable=true` or `scheduler.mode=plugin` (see
  [Post-renderer plugin](#post-renderer-plugin-helm-v4)).

## Quick start

```sh
# Render only, no cluster needed
make helm-platform-lint
make helm-platform-template

# Regression-check helm lint/template across representative value combos
make helm-platform-template-check

# Install/upgrade against your current kube-context
make helm-platform-deploy

# Status / uninstall
make helm-platform-status
make helm-platform-uninstall
```

Override any value with `HELM_EXTRA_ARGS`, e.g.:

```sh
HELM_EXTRA_ARGS="--set scheduler.mode=plugin --set scheduler.plugin.image.tag=v0.1.0 --set scheduler.plugin.placementServer.url=http://<svc>.<ns>.svc.cluster.local:8090" \
  make helm-platform-deploy
```

Or use `helm` directly against `charts/hiro-adaptive-platform` with a
`-f custom-values.yaml` — everything below is a plain Helm chart, the
Makefile targets are just convenience wrappers (see
[Post-renderer plugin](#post-renderer-plugin-helm-v4) for the one thing they
do that a bare `helm install` doesn't).

## What gets deployed

| Component | Value that enables it | Notes |
|---|---|---|
| Operator (CRD, RBAC, manager Deployment) | `hiro-operator.enabled` (default `true`) | Local `file://../../dist/chart` dependency, aliased `hiro-operator`. Never hand-edited — it's kubebuilder-generated and gets wiped on every `kubebuilder edit --plugins=helm/v2-alpha` regen. |
| Pod-mutating webhook | `webhook.enable` | Requires `scheduler.mode: plugin` — enforced by a `fail()` check at render time (see [Webhook requires the scheduler plugin](#webhook-requires-the-scheduler-plugin)). |
| Self-signed Issuer + Certificate for the webhook | `webhook.certManager.enable` | Needs a cert-manager already running somewhere in the cluster (yours, or `prerequisites.certManager.enable`). |
| Scheduler plugin (standalone scheduler Deployment) | `scheduler.mode: plugin` | Pods opt in via `spec.schedulerName` — the webhook sets this automatically, or you can set it by hand without the webhook. |
| Scheduler extender (patches the cluster's kube-scheduler) | `scheduler.mode: extender` | Affects every pod via the default scheduler. Privileged hook Jobs — see [Scheduler extender privileges](#scheduler-extender-privileges). |
| Mock decision agent | `mockAgent.enable` | Demo/eval only. Real deployments point `DECISION_AGENT_URL` at their own agent instead. |
| Sample `OrchestrationProfile` CRs + nginx Deployments | `samples.enabled` | Demo/eval only. |
| Bundled cert-manager / metrics-server / kube-prometheus-stack | `prerequisites.{certManager,metricsServer,prometheus}.enable` | Off by default — see [Bundled prerequisites](#bundled-prerequisites). |

## Values reference

Full comments live in `values.yaml` itself; this is the shape, grouped the
same way.

### `hiro-operator` — the operator subchart

```yaml
hiro-operator:
  enabled: true
  manager:
    env: [ ... ]   # complete list — see note below
```

`manager.env` is a plain list, and Helm replaces lists wholesale rather than
merging them by key — there's no way to override one entry from the
umbrella chart's values without restating the whole list. So `values.yaml`
carries the operator's **complete** env list already; to change one entry,
edit it directly there rather than trying to `--set` a single item.

Two entries in that list — `ENABLE_WEBHOOKS` and `HIRO_SCHEDULER_NAME` — are
inert defaults you never need to touch by hand: the Helm post-renderer
overrides both automatically whenever `webhook.enable=true` /
`scheduler.mode=plugin` respectively (see
[Post-renderer plugin](#post-renderer-plugin-helm-v4)).

### `mockAgent`

```yaml
mockAgent:
  enable: false
  serviceName: mock-decision-agent
  port: 8080
  image: python:3.11-slim
  resources: { ... }
```

`serviceName` and `port` are both safely renameable — everything (Service,
container port, the mock server's own listener) is templated from them.
**Except**: if you rename `serviceName`/`port`, update
`hiro-operator.manager.env`'s `DECISION_AGENT_URL` entry to match — that URL
is a plain string in the env list above, not derived from these values
(Helm's `values.yaml` can't template itself, so there's no way to compute it
automatically).

### `webhook`

```yaml
webhook:
  enable: false
  excludeNamespaces: [kube-system, kube-public, kube-node-lease]
  certManager:
    enable: false
```

Requires `scheduler.mode: plugin` (see
[Webhook requires the scheduler plugin](#webhook-requires-the-scheduler-plugin)).
`certManager.enable` needs a cert-manager already reachable in the cluster
— your own, or `prerequisites.certManager.enable: true`.

### `prerequisites` / bundled dependency passthrough

```yaml
prerequisites:
  certManager: { enable: false }
  metricsServer: { enable: false }
  prometheus: { enable: false }

certManager: { crds: { enabled: true } }        # passthrough to the cert-manager subchart
metricsServer: { args: [--kubelet-insecure-tls] } # passthrough to the metrics-server subchart
prometheus: { prometheus: { prometheusSpec: { ... } } } # passthrough to kube-prometheus-stack
```

See [Bundled prerequisites](#bundled-prerequisites) for why the enable flags
and the passthrough config are two separate top-level keys instead of one.

### `scheduler`

```yaml
scheduler:
  mode: none   # none | plugin | extender
  plugin:
    schedulerName: hiro-scheduler
    image: { repository: ..., tag: "", pullPolicy: IfNotPresent }
    replicas: 1
    imagePullSecrets: []
    resources: { ... }
    placementServer: { url: "", path: ..., filterPath: ..., timeoutSeconds: 8 }
  extender:
    images: { kubectl: ..., patcher: ... }
    placementServer: { port: 8090, filterVerb: ..., prioritizeVerb: ..., weight: 5, ... }
```

`scheduler.plugin.image.tag` and `scheduler.plugin.placementServer.url` have
no default and are `required` — they can't be safely guessed (no published
release cadence for the scheduler image; the operator's placement Service
name depends on Helm's own name-truncation logic). Setting either wrong
fails the render loudly instead of deploying something broken silently.
Look the Service name up after the operator is deployed:

```sh
kubectl get svc -n <namespace> -l app.kubernetes.io/component=placement-server
```

`scheduler.extender` needs no `placementServer.url` — its hook Jobs resolve
the Service by label selector at install/upgrade time, when they have real
cluster access (see [Scheduler extender](#scheduler-extender-privileges)).

### `samples`

```yaml
samples:
  enabled: false
  namespace: default
  nginxImage: nginx:latest
  rebalancing: { enabled: true, dryRun: false }
  nginxApp / nginxApp1 / nginxApp2: { deploymentName: ..., serviceName: ... }
  profileSample / profile1 / profile2: { name: ..., targetDeploymentName: ... }
```

Demo/eval only, off by default. `rebalancing.enabled`/`dryRun` here are
per-`OrchestrationProfile` CR spec fields — unrelated to
`hiro-operator.manager.env`'s rebalance tuning despite the similar name.
Each `profileN.targetDeploymentName` is a plain string reference kept in
sync by hand with the matching `nginxAppN.deploymentName` — there's no
cross-reference Helm can enforce between two independent template files.

## Worked examples

**Operator only (defaults):**

```sh
make helm-platform-deploy
```

**Operator + scheduler plugin + auto-mutating webhook:**

```sh
HELM_EXTRA_ARGS="\
  --set scheduler.mode=plugin \
  --set scheduler.plugin.image.tag=v0.1.0 \
  --set scheduler.plugin.placementServer.url=http://<svc>.<ns>.svc.cluster.local:8090 \
  --set webhook.enable=true \
  --set webhook.certManager.enable=true \
  --set prerequisites.certManager.enable=true" \
  make helm-platform-deploy
```

**Operator + scheduler extender (no custom scheduler pod needed):**

```sh
HELM_EXTRA_ARGS="--set scheduler.mode=extender" make helm-platform-deploy
```

**Demo/eval on Kind — mock agent, samples, bundled monitoring:**

```sh
HELM_EXTRA_ARGS="\
  --set mockAgent.enable=true \
  --set samples.enabled=true \
  --set prerequisites.metricsServer.enable=true \
  --set prerequisites.prometheus.enable=true" \
  make helm-platform-deploy
```

## Webhook requires the scheduler plugin

`webhook.enable=true` fails the render if `scheduler.mode` isn't `plugin`.
This isn't an arbitrary restriction: the webhook's only job is setting
`spec.schedulerName` on new pods. With `scheduler.mode: none`, nothing ever
claims that name and mutated pods stick `Pending` forever. With
`scheduler.mode: extender`, the *default* scheduler already handles every
pod via the extender patch, so setting `schedulerName` would misroute them
away from it instead. `hack/deploy_full_stack.sh` has the same coupling —
its webhook phase has no independent toggle at all, it's driven 1:1 by
whether the scheduler plugin is being deployed.

## Post-renderer plugin (Helm v4)

Two things can't be expressed as plain Helm values, because the operator's
`manager.env` is a flat list Helm can't merge into by key, and its
Deployment (from the `dist/chart` subchart) can't be hand-edited:

1. `webhook.enable=true` needs the cert volume/mount/port/arg added to the
   operator container, and `ENABLE_WEBHOOKS` forced to `"true"`.
2. `scheduler.mode=plugin` needs `HIRO_SCHEDULER_NAME` forced to match
   `scheduler.plugin.schedulerName`.

`postrender/render.sh` handles both: it self-detects which feature(s) are
active by scanning Helm's own rendered output (no separate values needed),
and patches the operator's already-rendered Deployment via `kustomize`
(appending duplicate env entries — Kubernetes uses the last one, so this
safely overrides `manager.env`'s static defaults without touching them).
When neither feature is active it passes the input straight through.

**Helm v4 changed what `--post-renderer` accepts**: it now takes the name
of an installed plugin (type `postrenderer/v1`), not a raw script path like
Helm v3 did ([helm/helm#31340](https://github.com/helm/helm/issues/31340)).
`make helm-platform-template`/`helm-platform-deploy`/`helm-platform-template-check`
all install this automatically via the `helm-platform-postrenderer-install`
target. If you run `helm` directly instead:

```sh
helm plugin install charts/hiro-adaptive-platform/postrender
helm template hiro-adaptive-platform charts/hiro-adaptive-platform \
  --post-renderer hiro-adaptive-platform-postrenderer ...
```

Skipping this step doesn't error — it just silently renders webhook/plugin
mode *without* the patches above, leaving a broken deployment (webhook
enabled but the operator never actually starts its webhook server, etc.).

## Scheduler extender privileges

Extender mode (`scheduler.mode: extender`) mutates
`/etc/kubernetes/manifests/kube-scheduler.yaml` on the control-plane node —
state outside anything Helm's release tracking can see. This needs
meaningfully more privilege than the rest of this chart:

- A **prep** Job (ordinary RBAC): `get`/`list` on Services in this
  release's namespace, `get`/`create`/`update`/`delete` on ConfigMaps and
  `get`/`list`/`watch` on Pods in `kube-system`.
- **Patch** and **restore** Jobs: `privileged: true`, scheduled onto the
  control-plane node (`nodeSelector`/tolerations), with `hostPath` mounts of
  `/etc/kubernetes/manifests` and `/etc/kubernetes`. These make no
  Kubernetes API calls at all — the privilege is for direct node filesystem
  access, not RBAC.
- A **verify** Job polls for the patched kube-scheduler pod to come back
  healthy with the new `--config` flag; a **restore** Job (running as
  `pre-delete` when uninstalling from extender mode, or `pre-upgrade` when
  switching *away* from it) puts the original static manifest back,
  tolerantly no-op-ing if no backup is present.

This is unusually broad access for a Helm chart. Review
`templates/scheduler-extender/` before granting it in a shared cluster.

## Bundled prerequisites

`prerequisites.{certManager,metricsServer,prometheus}.enable` bundle
jetstack/cert-manager, metrics-server, and kube-prometheus-stack as
dependencies of *this* release. They're off by default and meant for an
isolated/demo/Kind cluster only — **`helm uninstall` on this release removes
them too**. On a shared cluster, install these yourself once and leave the
flags off.

The enable flag and the dependency's own passthrough config live under two
different top-level keys (`prerequisites.certManager.enable` vs.
`certManager.crds.enabled`) rather than one — cert-manager's chart enforces
a strict values schema that rejects unknown top-level keys, so an `enable`
key living alongside its real config breaks validation. The other two
dependencies don't currently enforce this, but the same split is applied to
all three for consistency.

Two behaviors from `hack/deploy_full_stack.sh` aren't replicated here:

- The script auto-detects and skips installing a component if something
  similar already exists cluster-wide. Helm has no equivalent — enabling
  `prerequisites.*` in a cluster that already runs an unrelated
  Prometheus/Grafana risks a conflicting duplicate (e.g. a node-exporter
  hostPort clash).
- The script self-heals a previous failed install before retrying. A failed
  bundled-dependency release inside this one umbrella install is not
  self-healing — you may need `helm uninstall` and a clean retry.

## CRDs and uninstalling

The `OrchestrationProfile` CRD is templated (not Helm's native `crds/`
directory) so schema changes flow through on `helm upgrade` automatically,
and carries `"helm.sh/resource-policy": keep` so `helm uninstall` leaves it
in place. To remove it once nothing needs it anymore:

```sh
kubectl delete crd orchestrationprofiles.orchestration.hiro.io
```

## When to use this vs. `deploy_full_stack.sh`

`hack/deploy_full_stack.sh` is the faster path to a working demo cluster:
every knob is scriptable in one imperative run, it auto-skips installing a
component that already exists, and it self-heals a previously failed
install. This chart trades those conveniences for repeatable, upgradeable
installs through standard Helm tooling (`helm upgrade`, `helm rollback`,
values files instead of exported shell variables). Neither replaces the
other — pick the shell script for a quick throwaway cluster, this chart for
anything you intend to keep upgrading.
