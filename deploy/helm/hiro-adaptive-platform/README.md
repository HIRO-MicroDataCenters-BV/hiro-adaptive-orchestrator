# hiro-adaptive-platform

Umbrella Helm chart for the full HIRO Adaptive Orchestrator stack: the
operator (as a local subchart dependency on `dist/chart`), the pod-mutating
webhook, a custom scheduler (plugin or extender — pick one), an opt-in mock
decision agent for demos, and optional bundled cluster prerequisites
(cert-manager, metrics-server, kube-prometheus-stack).

It is an additive, parallel path to `deploy/scripts/deploy_full_stack.sh` — see
[When to use this vs. the shell script](#when-to-use-this-vs-deploy_full_stacksh)
below.

## Requirements

- Helm >= 3.14 (Helm v4 works).
- `kubectl` and a reachable cluster for anything beyond `helm
  lint`/`helm template`.

No post-renderer, plugin or second install pass is needed: one `helm upgrade
--install` brings up everything that is enabled (see [One install](#one-install)).

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
HELM_EXTRA_ARGS="--set scheduler.mode=plugin --set scheduler.plugin.image.tag=v0.1.0 --set webhook.enable=true" \
  make helm-platform-deploy
```

Or use `helm` directly against `deploy/helm/hiro-adaptive-platform` with a
`-f custom-values.yaml` — everything below is a plain Helm chart, the
Makefile targets are just convenience wrappers.

## One install

Everything the stack needs from itself is computed at render time, so
operator, scheduler, webhook and mock agent go up together:

- The scheduler plugin's PlacementServer URL defaults to the operator's own
  Service, whose name the chart derives from the release name
  (`_helpers.tpl`). Set `scheduler.plugin.placementServer.url` only to point
  elsewhere.
- The webhook's TLS certificate is generated in-chart (`webhook.tls.mode:
  helm`), so no cert-manager has to exist first.
- `ENABLE_WEBHOOKS` and `HIRO_SCHEDULER_NAME` are rendered from
  `webhook.enable` and `scheduler.plugin.schedulerName`.

Typical demo install, one command:

```sh
helm upgrade --install hiro deploy/helm/hiro-adaptive-platform -n hiro-system --create-namespace --wait \
  --set mockAgent.enable=true \
  --set scheduler.mode=plugin --set scheduler.plugin.image.tag=<tag> \
  --set webhook.enable=true
kubectl apply -k config/samples/   # optional demo apps + profiles, never Helm-owned
```

## What gets deployed

| Component | Value that enables it | Notes |
|---|---|---|
| Operator (CRD, RBAC, manager Deployment) | `hiro-operator.enabled` (default `true`) | Local `file://../../../dist/chart` dependency, aliased `hiro-operator`. Never hand-edited — it's kubebuilder-generated and gets wiped on every `kubebuilder edit --plugins=helm/v2-alpha` regen. |
| Pod-mutating webhook | `webhook.enable` | Requires `scheduler.mode: plugin` — enforced by a `fail()` check at render time (see [Webhook requires the scheduler plugin](#webhook-requires-the-scheduler-plugin)). |
| Webhook serving certificate | `webhook.tls.mode` (`helm` default, `certManager`, `existingSecret`) | `helm` generates a self-signed CA + cert in-chart and keeps it across upgrades. `certManager` needs cert-manager installed beforehand. See [Webhook TLS](#webhook-tls). |
| Scheduler plugin (standalone scheduler Deployment) | `scheduler.mode: plugin` | Pods opt in via `spec.schedulerName` — the webhook sets this automatically, or you can set it by hand without the webhook. |
| Scheduler extender (patches the cluster's kube-scheduler) | `scheduler.mode: extender` | Affects every pod via the default scheduler. Privileged hook Jobs — see [Scheduler extender privileges](#scheduler-extender-privileges). |
| Mock decision agent | `mockAgent.enable` | Demo/eval only. Real deployments point `DECISION_AGENT_URL` at their own agent instead. |
| Bundled cert-manager / metrics-server / kube-prometheus-stack | `prerequisites.{certManager,metricsServer,prometheus}.enable` | Off by default — see [Bundled prerequisites](#bundled-prerequisites). cert-manager is only useful with `webhook.tls.mode=certManager`, and then must be installed first. |

## Values reference

Full comments live in `values.yaml` itself; this is the shape, grouped the
same way.

### `hiro-operator` — the operator subchart

```yaml
hiro-operator:
  enabled: true
  config:                       # one key per environment variable main.go reads
    DECISION_AGENT_URL: http://mock-decision-agent:8080
    REBALANCE_MIN_CPU: 50m
    # ...
  manager:
    env: []                     # must stay empty — see below
```

`config` is a map, so Helm merges it key by key. Change one setting and every
other key keeps its default:

```sh
--set hiro-operator.config.REBALANCE_MIN_CPU=100m
```

The map is rendered into the `hiro-operator-config` ConfigMap
(`templates/operator/configmap.yaml`) and loaded into the operator pod with
`envFrom`, so `cmd/main.go` still just reads environment variables. A hash of
the map is passed as the ignored `--config-checksum` flag, so editing a key
rolls the pod. Notes:

- Use `--set-string` for values that look like numbers or booleans
  (`--set-string hiro-operator.config.REBALANCE_MAX_REPLICAS=10`), and
  escape commas in `--set` values.
- Do not put entries in `hiro-operator.manager.env` (or `envOverrides`): an
  inline entry would silently beat the ConfigMap, so the chart fails the
  render if either is non-empty.
- `ENABLE_WEBHOOKS` and `HIRO_SCHEDULER_NAME` are derived from `webhook.enable`
  and `scheduler.plugin.schedulerName`; setting them in `config` has no
  effect. Changing either on an existing release does not alter the pod spec,
  so run `kubectl rollout restart` on the operator Deployment afterwards.
- `PLACEMENT_SERVER_PORT` must stay `:8090`: the placement Service comes from
  the generated subchart with that port fixed.
- Removing a key (`--set hiro-operator.config.KEY=null`) falls back to the
  operator's built-in default. `DECISION_AGENT_URL` has none and is required.

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
`hiro-operator.config.DECISION_AGENT_URL` to match — that URL is a plain
string in the values, not derived from these (Helm's `values.yaml` can't
template itself, so there's no way to compute it automatically).

### `webhook`

```yaml
webhook:
  enable: false
  excludeNamespaces: [kube-system, kube-public, kube-node-lease]
  tls:
    mode: helm        # helm | certManager | existingSecret
    validityDays: 3650
    caBundle: ""      # only for existingSecret
```

Requires `scheduler.mode: plugin` (see
[Webhook requires the scheduler plugin](#webhook-requires-the-scheduler-plugin));
both can be set in the same install. See [Webhook TLS](#webhook-tls) for the
three `tls.mode` values.

### `prerequisites` / bundled dependency passthrough

```yaml
prerequisites:
  certManager: { enable: false }
  metricsServer: { enable: false }
  prometheus: { enable: false }

cert-manager: { crds: { enabled: true } }          # passthrough to the cert-manager subchart
metrics-server: { args: [--kubelet-insecure-tls] } # passthrough to the metrics-server subchart
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

`scheduler.plugin.image.tag` has no default and is `required` — there is no
published release cadence for the scheduler image to guess from. Leaving it
unset fails the render loudly instead of deploying something broken.

`placementServer.url` is optional: empty means the operator's own placement
Service, named from the release by `_helpers.tpl`. Set it only for an
external PlacementServer.

`scheduler.extender` needs no URL — its hook Jobs resolve the Service by
label selector at install/upgrade time, when they have real cluster access
(see [Scheduler extender](#scheduler-extender-privileges)).

### Samples (not in this chart)

The demo profiles and nginx apps live in `config/samples/` and are applied
with `kubectl apply -k config/samples/` after install
(`deploy_full_stack_with_helm.sh` does this when `DEPLOY_SAMPLES=true`).
They stay out of Helm because other repos (e.g. EAO) create the same apps,
and a Helm-owned copy would conflict with theirs on upgrade.

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
  --set webhook.enable=true" \
  make helm-platform-deploy
```

**Operator + scheduler extender (no custom scheduler pod needed):**

```sh
HELM_EXTRA_ARGS="--set scheduler.mode=extender" make helm-platform-deploy
```

**Demo/eval on Kind — mock agent, bundled monitoring, then samples:**

```sh
HELM_EXTRA_ARGS="\
  --set mockAgent.enable=true \
  --set prerequisites.metricsServer.enable=true \
  --set prerequisites.prometheus.enable=true" \
  make helm-platform-deploy
kubectl apply -k config/samples/
```

## Migrating from the two-pass chart

Earlier versions listed the operator's settings as `hiro-operator.manager.env`
(a list) and needed a Helm post-renderer plus a second `helm upgrade` for the
scheduler and webhook. To move an existing release:

1. Drop `--post-renderer ...` and `--reuse-values` from your command; uninstall
   the plugin with `helm plugin uninstall hiro-adaptive-platform-postrenderer`.
2. Move every `hiro-operator.manager.env` / `envOverrides` entry you customised
   to `hiro-operator.config.<NAME>` (a map). The chart refuses to render while
   the old keys are non-empty.
3. Replace `webhook.certManager.enable=true` with `webhook.tls.mode=certManager`,
   or drop it to get the in-chart certificate. The webhook Secret
   `webhook-server-cert` keeps its name; if cert-manager owned it, delete it
   before the first upgrade in `helm` mode so the chart can create its own.
4. `scheduler.plugin.placementServer.url` can be removed.

## Webhook requires the scheduler plugin

`webhook.enable=true` fails the render if `scheduler.mode` isn't `plugin`.
This isn't an arbitrary restriction: the webhook's only job is setting
`spec.schedulerName` on new pods. With `scheduler.mode: none`, nothing ever
claims that name and mutated pods stick `Pending` forever. With
`scheduler.mode: extender`, the *default* scheduler already handles every
pod via the extender patch, so setting `schedulerName` would misroute them
away from it instead. `deploy/scripts/deploy_full_stack.sh` has the same coupling —
its webhook phase has no independent toggle at all, it's driven 1:1 by
whether the scheduler plugin is being deployed.

## Webhook TLS

`webhook.tls.mode` picks where the serving certificate comes from:

- **`helm`** (default) — the chart generates a self-signed CA and certificate
  (`genCA`/`genSignedCert`), stores them in the `webhook-server-cert` Secret
  and injects the CA as the webhook's `caBundle`. On `helm upgrade` the
  existing Secret is read back with `lookup` and reused, so the CA stays
  stable. Nothing else has to be installed, which is what makes a single
  install possible. There is no automatic rotation: it is valid for
  `validityDays` (default 10 years); delete the Secret and `helm upgrade` to
  reissue. `helm template` has no cluster to look up, so it prints a fresh
  certificate each time.
- **`certManager`** — a cert-manager `Issuer` + `Certificate`, rotated
  automatically. cert-manager must already be running: its CRDs cannot be
  created in the same install as the `Issuer`/`Certificate` that use them, so
  combining this with `prerequisites.certManager.enable=true` fails the render.
- **`existingSecret`** — you supply a `webhook-server-cert` Secret (`tls.crt`,
  `tls.key`) and the base64 CA in `webhook.tls.caBundle`.

The operator pod always mounts `webhook-server-cert` as an optional volume
and passes `--webhook-cert-path`; both are inert until `ENABLE_WEBHOOKS=true`
registers the webhook, because controller-runtime only starts its webhook
server then. That is why no post-render patching is needed.

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

Two behaviors from `deploy/scripts/deploy_full_stack.sh` aren't replicated here:

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

## Verification and CI

Three ways to exercise this chart end-to-end, beyond `helm lint`/`helm
template`:

- **`deploy/scripts/deploy_full_stack_with_helm.sh`** — local, against a Kind cluster.
  Regenerates `dist/chart` from source, builds the operator + scheduler
  images, loads them into Kind (no registry needed), deploys everything
  with one `helm upgrade --install`, and health-checks the result. Writes a standalone
  kubeconfig to `.kind-kubeconfig-<cluster-name>.yaml` and prints it.
  No `GITHUB_PAT_TOKEN`/registry needed — images build and load locally.
  Options: `KIND_CLUSTER_NAME`, `SCHED_K8S_VERSION`, `HELM_NAMESPACE`,
  `HELM_PLATFORM_RELEASE`, `SCHED_NAME`, `SCHEDULER_MODE`,
  `USE_MOCK_AGENT`, `DEPLOY_SAMPLES`, `DEPLOY_PROMETHEUS`, `DEPLOY_METRICS_SERVER` and
  `OTLP_ENDPOINT` to pick which components get deployed. Usage:
  `deploy/scripts/deploy_full_stack_with_helm.sh [deploy|clean|destroy]` — `clean`
  uninstalls the release and keeps the cluster, `destroy` deletes it.
- **`.github/workflows/platform-verify-chart.yml`** — same idea in CI, on
  every push/PR, against a throwaway Kind cluster created inside the job.
  Runs a manifests-drift check first (fails if `dist/chart`/`config`/`api`
  are out of date with the source), then `helm lint`/`helm
  template-check`/the same single-install deploy.
- **`.github/workflows/platform-deploy-chart.yml`** — deploys to a real
  target cluster. Runs on push to `main`, or manually via "Run workflow".
  Builds and pushes images to GHCR, then deploys using a kubeconfig from the
  `TARGET_KUBECONFIG` repo secret. See the file's own header comment for the
  full list of repo Secrets/Variables it reads.

## When to use this vs. `deploy_full_stack.sh`

`deploy/scripts/deploy_full_stack.sh` is the faster path to a working demo cluster:
every knob is scriptable in one imperative run, it auto-skips installing a
component that already exists, and it self-heals a previously failed
install. This chart trades those conveniences for repeatable, upgradeable
installs through standard Helm tooling (`helm upgrade`, `helm rollback`,
values files instead of exported shell variables). Neither replaces the
other — pick the shell script for a quick throwaway cluster, this chart for
anything you intend to keep upgrading.
