# kube-symbiont

> **Status: experimental.** The phantom-pod mechanism below is a homelab capstone
> experiment, not a production pattern. Validate its scheduling behaviour on a
> non-critical node before trusting it; the standard Kubernetes controls
> (`kubeReserved`, `systemReserved`, Node Allocatable, eviction thresholds) come first.

A Kubernetes operator that makes **bare-metal resource usage visible to the scheduler**.
Processes running alongside a cluster outside Kubernetes — game servers under Pterodactyl/Wings,
Docker daemons, systemd services — contribute **zero** to the scheduler's capacity math,
which silently over-commits nodes. kube-symbiont maintains one "phantom pod" per bare-metal
workload whose resource *requests* mirror that workload's measured CPU/RAM, so the scheduler
accounts for capacity it cannot otherwise see.

## Why this exists: requests vs usage

`allocatable` is static arithmetic computed once at kubelet startup:

```
allocatable = capacity − kube-reserved − system-reserved − hard-eviction-threshold
```

Nothing in that formula measures live usage. Placement is ledger arithmetic —
`allocatable − Σ(requests of pods already scheduled)` — and a bare-metal process is not a
Pod, so it sums as zero. The kernel *does* see the bare-metal process, but only reactively
(eviction manager / OOM killer), i.e. after the node is already starving. The failure mode:
the scheduler keeps packing pods because the ledger looks fine, then eviction fires.

kube-symbiont is the dynamic, per-workload version of `system-reserved`: it measures the
real footprint via Prometheus and injects it into the ledger as phantom-pod requests.

## How the phantom works

A `pause` container with `requests: 8Gi` occupies ~1 MB of real RAM. The kernel sees ~1 MB;
the scheduler sees 8 GiB subtracted from allocatable when placing *other* pods. That asymmetry
is the entire mechanism. Each phantom is:

| Property | Value |
|---|---|
| Image | `registry.k8s.io/pause:3.9` |
| Placement | hard pin via `spec.nodeName` (bare metal cannot migrate anyway) |
| QoS | Guaranteed — requests == limits, patched together (QoS class is immutable) |
| Resize policy | `NotRequired` for cpu **and** memory → in-place resize, no restart |
| PriorityClass | `symbiont-ballast` (value 1000) so priority-0 workloads cannot preempt it |
| Lifetime | controller ownerReference → deleting the ShadowWorkload GCs the phantom |

Reconcile loop per ShadowWorkload: ensure phantom exists → resolve source to PromQL →
instant-query Prometheus over the window → clamp to `[floor, ceiling]` → if relative drift
exceeds `deltaThresholdPercent`, PATCH the pod **resize subresource** (requests and limits
together) → update status → `RequeueAfter: pollInterval`. Metric-driven, so it re-measures
on a clock regardless of CR changes.

Requires **Kubernetes 1.29+** (In-Place Pod Vertical Scaling; GA and default-on at 1.35).

## Measured sources (v1alpha1)

Exactly one source per ShadowWorkload; each resolves to a CPU-cores + memory-bytes pair.

- **`docker`** — shadows Docker containers running directly on the host, discriminated from
  k8s pods by cgroup **id prefix**: `id=~"/system.slice/docker-.*"` on the standalone
  cAdvisor job. (Image-label matching was refuted by measurement: cAdvisor monitors the whole
  cgroup tree and k8s pod series carry `image` labels too.)
- **`promql`** — raw CPU/memory query passthrough; escape hatch for anything else.
- *(v0.2 planned: `cgroup` path globs and `systemd` unit sources.)*

## Quickstart

Prerequisites: Go 1.26+ (go.mod declares go 1.26.0), kubectl, kustomize (Makefile fetches tools locally), and a cluster
running Kubernetes ≥ 1.29 with Prometheus already scraping cAdvisor.

```sh
# 1. Install namespace + symbiont-ballast PriorityClass + CRDs + operator.
#    IMG must be pullable from the cluster.
make deploy IMG=<registry>/kube-symbiont:0.1.0

# 2. Point one ShadowWorkload at your bare-metal host.
kubectl apply -f config/samples/symbiont_v1alpha1_shadowworkload.yaml

# 3. Watch the phantom track reality.
kubectl get shadowworkloads -o wide     # or: kubectl get sw
```

Release installs can use the self-contained manifest or Helm chart:

```sh
kubectl apply -f https://github.com/michaelwetyyy/kube-symbiont/releases/download/v0.1.0/install.yaml

helm upgrade --install kube-symbiont ./charts/chart \
  --namespace kube-symbiont-system --create-namespace
```

Published images are multi-platform (`linux/amd64`, `linux/arm64`), run as numeric non-root
UID/GID 65532, use pinned build/runtime bases, and carry BuildKit SBOM plus GitHub provenance
attestations. Release installers pin the image manifest digest.

### Example

```yaml
apiVersion: symbiont.tensorhost.com/v1alpha1
kind: ShadowWorkload
metadata:
  name: lab-docker
spec:
  node: lab                      # phantom pinned here via spec.nodeName
  source:
    type: docker                 # id-prefix selector generated for you
    docker:
      selector: all              # every bare-metal container on the node
      cadvisorJob: cadvisor      # standalone cAdvisor's Prometheus job label
      cadvisorInstance: "192.0.2.10:4194"      # required per-node target; documentation address
  metrics:
    prometheusURL: http://kube-prometheus-stack-prometheus.monitoring:9090
    window: 5m                   # moving-average smoothing horizon
  update:
    deltaThresholdPercent: 10    # resize only when drift exceeds this
    pollInterval: 30s            # measure → clamp → resize cadence
    floor:   { cpu: 10m, memory: 32Mi }    # workload off / no metrics
    ceiling: { cpu: "8", memory: 32Gi }    # safety clamps
```

Raw-query alternative:

```yaml
source:
  type: promql
  promql:
    cpuCores: 'sum(rate(container_cpu_usage_seconds_total{id=~"/system.slice/docker-.*"}[5m]))'
    memoryBytes: 'sum(avg_over_time(container_memory_working_set_bytes{id=~"/system.slice/docker-.*"}[5m]))'
```

Behaviour on rough edges: workload off or emitting nothing → phantom settles on the floor;
brief spikes → absorbed by the moving average; phantom deleted externally → recreated;
in-place resize rejected → phantom keeps previous requests and the CR reports a `Degraded`
condition; controller restart → resumes from the phantom's current requests.

## Development

```sh
make test          # unit + envtest suite (envtest binaries fetched automatically)
make run           # run the manager against your current kubeconfig context
make manifests generate   # regenerate CRDs/RBAC/deepcopy after API edits
```

## Known limitations

- **Experimental mechanism.** Phantom reservations influence placement but protect nothing
  at the kernel level: under extreme pressure the OOM killer reads actual pages, not requests.
  This complements — never replaces — properly configured `kubeReserved`/`systemReserved`.
- **Scheduler race during resize.** A brief window exists where the node can be
  over-scheduled mid-resize (acknowledged upstream). Low risk at homelab scale.
- **Polling delay.** Between a spike and the phantom's resize there is up to one
  `pollInterval` plus window smoothing of lag, by design.
- **Prometheus dependency.** Measurement quality equals scrape coverage; a dead Prometheus
  freezes the phantom at its last size rather than shrinking it.
- **Trusted configuration boundary.** A `ShadowWorkload` author selects the Prometheus URL and,
  for raw `promql`, the query. Grant CR write access only to trusted operators; do not expose it
  as an untrusted multi-tenant API.
- **Single-node targeting.** Each ShadowWorkload pins one node; multi-host bare metal means
  one resource per host.

## License

Apache-2.0. Copyright 2026.
