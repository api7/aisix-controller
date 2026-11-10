# aisix-controller

A Kubernetes controller for the [AISIX](https://github.com/api7/aisix) AI gateway. It reconciles AISIX custom resources into a standalone gateway's resources file, so models, provider credentials and caller keys are managed with `kubectl apply`.

## How it works

```
ProviderKey / Model / APIKey + Secrets
        │
        ▼
aisix-controller ── render ── aisix validate (bundled binary) ──► Secret aisix-resources
        │                                                            │ mounted by the chart
        │ stamps aisix.api7.ai/resources-hash on the pod template    ▼
        └──────────────────────────────────────────────────► gateway Deployment rolls
        ▲                                                            │
        └──── GET <pod>:9090/status/config  (applied.config_hash) ◄──┘
```

1. Every change to a custom resource, a referenced Secret, the gateway Deployment or its pods re-renders the whole resources file (`_format_version: "1"`).
2. The document is checked with `aisix validate` from the same aisix release, run with an empty environment. A resource the gateway would reject keeps its previously served entry, or is left out if it was never served. The Secret never holds a file the gateway cannot load, which matters because a gateway pod with an invalid file fails to start.
3. A changed document is written to the `aisix-resources` Secret, next to `aisix-controller.index.json`, which records the resource each entry belongs to so a resource's served entry is found even after its `display_name` changes. The document's SHA-256 is stamped on the gateway Deployment's pod template. The gateway reads its resources file at boot and on SIGHUP only, so the stamp rolls the pods.
4. `Programmed` turns true once every ready gateway pod reports the written file's hash as `applied.config_hash` on its status listener.

## Resources

All kinds are namespaced, in `aisix.api7.ai/v1alpha1`. A `spec` is the AISIX resource document with AISIX's own snake_case field names, minus credentials and id-form references, plus the references below. `display_name` defaults to `metadata.name`; set it when the name is not a valid Kubernetes name (for example `openai/gpt-4o`).

| Kind | Short name | Reference fields |
|---|---|---|
| `ProviderKey` | `aipk` | `api_key_secret_ref: {name, key}`: the upstream credential |
| `Model` | `aimodel` | `provider_key_ref: {name}`: a ProviderKey's `metadata.name` (direct models only) |
| `APIKey` | `aikey` | `key_secret_ref: {name, key}`: the caller's plaintext key; only its SHA-256 is written |

Secret refs may name any Secret in the namespace except the controller's own `aisix-resources`. A `provider_key_ref` must name a ProviderKey that is served; one that lost a shared `display_name` to an older ProviderKey is rejected.

```yaml
apiVersion: aisix.api7.ai/v1alpha1
kind: ProviderKey
metadata:
  name: openai
spec:
  provider: openai
  api_key_secret_ref: {name: llm-creds, key: openai}
---
apiVersion: aisix.api7.ai/v1alpha1
kind: Model
metadata:
  name: gpt-4o
spec:
  display_name: openai/gpt-4o
  provider: openai
  model_name: gpt-4o
  provider_key_ref: {name: openai}
---
apiVersion: aisix.api7.ai/v1alpha1
kind: APIKey
metadata:
  name: team-a
spec:
  allowed_models: ["openai/gpt-4o"]
  key_secret_ref: {name: caller-keys, key: team-a}
```

Conditions:

| Condition | True when |
|---|---|
| `Accepted` | The current spec is valid for the gateway |
| `Programmed` | Every ready gateway pod serves a resources file containing the current spec |

The CRD schemas are generated from the strict JSON Schemas of the aisix release in `third_party/aisix/VERSION`. Rules a structural schema cannot hold (`oneOf` branches, `if`/`then`) are still enforced by `aisix validate`, and show up as `Accepted=False`.

## Install

The controller and the gateway run in the same namespace. Install the controller first, so the resources Secret exists:

```bash
kubectl apply --server-side -k config/crd
kubectl -n aisix apply -k config/controller

helm install aisix aisix --repo https://charts.api7.ai -n aisix \
  --set controlPlane.enabled=false \
  --set standalone.existingSecret=aisix-resources \
  --set fullnameOverride=aisix
```

Flags (see `aisix-controller --help`): `--secret`, `--secret-key`, `--deployment`, `--gateway-status-port` (the chart's metrics port, default 9090), `--aisix-binary`, `--resync`, `--poll`, `--leader-elect`.

The controller reads each gateway pod's `/status/config` on the metrics port, which the gateway serves without authentication. Keep the chart's `metrics.enabled` on, and allow controller-to-gateway traffic on that port if a NetworkPolicy applies.

## Limitations in v0.1

- Every configuration change restarts the gateway pods, at least `preStopSleepSeconds` (30 s by default) per batch, and resets in-memory rate-limit counters.
- One gateway Deployment per controller, and one namespace.
- No admission webhook: invalid specs are reported through `Accepted=False`.
- The resources file must fit in one Secret (1 MiB).
- Kinds beyond ProviderKey, Model and APIKey are not supported yet.

## Development

```bash
make build
make test          # unit + envtest suites
make test-aisix    # unit tests against the real aisix binary (Docker)
make lint
make crds          # regenerate config/crd from third_party/aisix
make crds-check    # fail if generated code or CRDs are stale
make e2e           # kind cluster + AISIX chart + controller
```

To move to another aisix release: `make sync-schemas AISIX_TAG=v1.x.y && make crds`, then bump `AISIX_VERSION` in the Dockerfile.
