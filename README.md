# Kite Service

A production-grade microservice onboarding exercise demonstrating Kubernetes deployment,
GitOps, CI/CD, and observability — runnable end-to-end on **minikube with no cloud account
required**. Anyone can clone this repo, run `minikube start`, and get a fully working
multi-environment GitOps system. The [Production path](#production-path-what-wed-do-differently)
section maps every PoC shortcut to its cloud-native equivalent.

---

## Architecture

```
┌─────────────┐   push     ┌───────────────────────────────────────────────┐
│  Developer  │───────────▶│           GitHub Actions (ci.yaml)             │
└──────┬──────┘            │           go vet + go test -race               │
       │                   └───────────────────────────────────────────────┘
       │ make release VERSION=x.y.z
       │  ├─ docker build into minikube daemon (no registry)
       │  ├─ sed-patch image tag in values-{dev,staging,prod}.yaml
       │  └─ git commit + git tag vX.Y.Z → push to GitHub
       ▼
┌──────────────────────────────────┐
│              ArgoCD               │
│  watches github.com/charliewyse/kite │
│  dev:     auto-sync (selfHeal)    │
│  staging: manual sync             │
│  prod:    manual sync             │
└──────────────┬───────────────────┘
               │ applies Helm chart
               ▼
┌─────────────────────────────────────────────┐
│                  minikube                    │
│                                              │
│  kite-dev / kite-staging / kite-prod         │
│    Deployment → Service (ClusterIP)          │
│      :8080  app    :9090  metrics            │
│    nginx Ingress (*.local hostnames)         │
│                                              │
│  monitoring namespace                        │
│    Prometheus ── scrapes :9090 ──▶ Grafana   │
│    (kube-prometheus-stack, ArgoCD-managed)   │
└─────────────────────────────────────────────┘
```

### Component inventory

| Component | Technology | Location |
|---|---|---|
| Application | Go 1.22, distroless image | `app/` |
| Packaging | Helm chart, per-env values | `helm/kite-service/` |
| GitOps | ArgoCD App-of-Apps | `gitops/` |
| CI | GitHub Actions (`ci.yaml` — test only) | `.github/workflows/` |
| Release | `Makefile` — build + tag + push locally | `Makefile` |
| Observability | kube-prometheus-stack + Grafana JSON | `observability/` |

---

## Repository structure

```
kite/
├── app/                        # Go HTTP service
│   ├── Dockerfile              # multi-stage, distroless final image
│   ├── main.go                 # graceful shutdown, signal handling
│   └── internal/server/
│       ├── server.go           # HTTP servers (app :8080, metrics :9090)
│       └── handlers.go         # /health, /ready, /ping + middleware
│
├── helm/kite-service/
│   ├── values.yaml             # production-safe defaults
│   ├── values-{dev,staging,prod}.yaml
│   ├── files/
│   │   └── kite-service-dashboard.json  # Grafana dashboard (auto-loaded via sidecar)
│   └── templates/
│       ├── deployment.yaml
│       ├── service.yaml
│       ├── ingress.yaml
│       ├── hpa.yaml
│       ├── configmap.yaml
│       ├── serviceaccount.yaml
│       ├── servicemonitor.yaml      # Prometheus scrape target
│       ├── prometheusrule.yaml      # 5 alerts + recording rules (per environment)
│       └── grafana-dashboard.yaml   # ConfigMap labelled grafana_dashboard=1 (auto-imported)
│
├── gitops/
│   ├── argocd/
│   │   ├── appproject.yaml     # scopes deployments to kite namespaces only
│   │   └── app-of-apps.yaml    # bootstrap — apply once
│   └── apps/
│       ├── {dev,staging,prod}/kite-service.yaml
│       └── monitoring/kube-prometheus-stack.yaml
│
├── Makefile                    # build image, bump tags, push git tag (local release flow)
├── .github/workflows/
│   └── ci.yaml                 # go vet + go test -race on every push/PR (tests only)
│
├── observability/
│   ├── alerts/kite-service-rules.yaml   # reference copy of PrometheusRule
│   └── dashboards/kite-service.json     # reference copy of Grafana dashboard
│
├── docs/debugging.md           # 502/504 runbook
└── SPEC.md                     # architecture decisions and rationale
```

---

## How to run locally

```bash
cd app
go run .
# App:     http://localhost:8080
# Metrics: http://localhost:9090/metrics

curl localhost:8080/health   # {"status":"ok"}
curl localhost:8080/ready    # {"status":"ready"}
curl localhost:8080/ping     # {"message":"pong","version":"dev"}
```

### Docker

```bash
cd app
docker build -t kite-service:local .
docker run --rm -p 8080:8080 -p 9090:9090 kite-service:local
```

### Local Kubernetes (minikube)

All three environment value files use `pullPolicy: Never` and `className: nginx`, so the
full ArgoCD GitOps flow works out of the box on minikube — no registry required. Use the
ArgoCD flow below rather than `helm install` directly.

### ArgoCD GitOps flow (recommended)

ArgoCD pulls directly from `https://github.com/charliewyse/kite` — no local
git daemon required. Minikube has outbound internet access by default, so the
public GitHub repo is reachable from inside the cluster.

```bash
# 1 — Install ArgoCD
kubectl create namespace argocd
kubectl apply -n argocd \
  -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
kubectl wait --for=condition=Ready pod \
  -l app.kubernetes.io/name=argocd-server -n argocd --timeout=180s

# 2 — Create app namespaces (CreateNamespace=true needs cluster-admin RBAC ArgoCD doesn't have by default)
kubectl create namespace kite-dev
kubectl create namespace kite-staging
kubectl create namespace kite-prod

# 3 — Bootstrap the app-of-apps (reads from GitHub)
kubectl apply -n argocd -f gitops/argocd/appproject.yaml
kubectl apply -n argocd -f gitops/argocd/app-of-apps.yaml
```

ArgoCD will discover and sync all applications automatically. The monitoring stack
(kube-prometheus-stack) will deploy first; dev/staging/prod will follow once the
ServiceMonitor CRDs are available.

---

### Accessing everything locally

All UIs require either a `kubectl port-forward` (for cluster services) or an `/etc/hosts`
entry (for the nginx ingress). Run these once per terminal session:

**Step 1 — Add hosts entries** (one-time setup):

```bash
echo "$(minikube ip) kite-dev.local kite-staging.local kite-prod.local" | sudo tee -a /etc/hosts
```

**Step 2 — Start port-forwards** (run each in the background or a separate terminal):

```bash
# ArgoCD UI
kubectl port-forward svc/argocd-server -n argocd 8443:443 &

# Grafana
kubectl port-forward svc/kube-prometheus-stack-grafana -n monitoring 3000:80 &

# Prometheus (use 9091 — port 9090 is taken by the app's metrics server)
kubectl port-forward svc/kube-prometheus-stack-prometheus -n monitoring 9091:9090 &
```

**URLs and credentials:**

| Service | URL | Credentials |
|---|---|---|
| **kite-service (dev)** | http://kite-dev.local | — |
| **kite-service (staging)** | http://kite-staging.local | — |
| **kite-service (prod)** | http://kite-prod.local | — |
| **ArgoCD UI** | https://localhost:8443 | admin / `kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath="{.data.password}" \| base64 -d` |
| **Grafana** | http://localhost:3000 | admin / admin |
| **Prometheus alerts** | http://localhost:9091/alerts | — |

**Gotchas learned the hard way:**

- **Don't `helm install` before ArgoCD takes ownership.** If you install
  manually first, ArgoCD will conflict with the existing release tracking
  metadata. Either let ArgoCD do the initial install, or `helm uninstall` first.

- **ArgoCD polls git every ~3 minutes.** After pushing a change, force an
  immediate refresh instead of waiting:
  ```bash
  kubectl -n argocd annotate application kite-service-dev \
    argocd.argoproj.io/refresh=hard --overwrite
  ```

- **`kite-service-dev` may show OutOfSync if kube-prometheus-stack isn't deployed yet.**
  The ServiceMonitor and PrometheusRule CRDs are provided by `kube-prometheus-stack`.
  ArgoCD deploys it automatically via the app-of-apps, but it can take a minute to come
  up on first boot. Once the monitoring stack is Healthy the app will reconcile on its own.

---

## How to deploy (cloud)

### 1 — Install ArgoCD and bootstrap GitOps

```bash
kubectl create namespace argocd
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml

kubectl apply -n argocd -f gitops/argocd/appproject.yaml
kubectl apply -n argocd -f gitops/argocd/app-of-apps.yaml
```

ArgoCD will discover and create `kite-service-dev`, `kite-service-staging`, and
`kite-service-prod` Applications automatically.

### 2 — Configure GitHub

No secrets required — images are built locally and never pushed to a registry.

### 3 — Deploy

Images are built locally into minikube's Docker daemon — no registry required.
The `Makefile` handles building, tagging values files, and pushing the git tag.

```bash
# Build and release version 1.2.3
make release VERSION=1.2.3
```

This will:
1. Build `kite-service:1.2.3` directly into minikube's daemon (`pullPolicy: Never`)
2. Bump the image tag to `1.2.3` in `values-dev.yaml`, `values-staging.yaml`, and `values-prod.yaml`
3. Commit and push to `main`
4. Create and push git tag `v1.2.3`

ArgoCD detects the values file change and auto-syncs dev within seconds.
Staging and prod require a manual sync in the ArgoCD UI.

CI (GitHub Actions `ci.yaml`) runs `go vet` and `go test` on every push to `main` and
every pull request — keeping the test gate in place even without a registry.

To promote to staging or prod, sync manually after `make release` pushes the updated
values files:

```bash
# ArgoCD UI — click Sync on kite-service-staging / kite-service-prod
# or via CLI:
argocd app sync kite-service-staging
argocd app sync kite-service-prod
```

### Rollback strategy

**Preferred: git revert (keeps audit trail)**

```bash
# Revert the tag bump commit — ArgoCD auto-syncs dev, staging/prod need manual sync
git revert HEAD --no-edit
git push
```

**Fast: ArgoCD revision rollback**

```bash
# List available history
argocd app history kite-service-dev

# Roll back to a specific revision (ArgoCD revision ID, not git SHA)
argocd app rollback kite-service-dev <revision-id>
```

**Emergency: kubectl only (bypasses GitOps — reconcile git afterwards)**

```bash
kubectl rollout undo deployment/kite-service -n kite-dev
```

The `maxUnavailable: 0` rolling update policy means the previous ReplicaSet is kept alive until the new pods pass readiness — so a rollback is instant and zero-downtime.

---

### 4 — Observability

Prometheus, Grafana, and the kite alerts are all managed by ArgoCD — no manual steps needed.

- **kube-prometheus-stack** is deployed by ArgoCD into the `monitoring` namespace via the app-of-apps
- **Grafana dashboard** is a ConfigMap in the Helm chart labelled `grafana_dashboard: "1"` — Grafana's sidecar detects it and loads it automatically on startup
- **PrometheusRule** (5 alerts + recording rules) is a Helm template deployed alongside each environment — Prometheus picks it up automatically via `ruleSelectorNilUsesHelmValues: false`

Access Grafana and Prometheus using the port-forwards and URLs in the [Accessing everything locally](#accessing-everything-locally) section above.

---

## Design decisions

**Why Go?**
Single static binary, tiny distroless image (~8 MB), excellent stdlib HTTP
primitives. No framework overhead for a service this simple.

**Why two HTTP servers (`:8080` and `:9090`)?**
Metrics must not be routable via the public Ingress. Running them on a separate
port means the ALB rule set only forwards `:8080` traffic, and the ServiceMonitor
scrapes `:9090` internally without any Ingress involvement.

**Why Helm over raw manifests?**
Three environments with different replica counts, resource budgets, and HPA
settings would mean maintaining three copies of every manifest. Helm's value
override files make env differences explicit and diffable.

**Why App-of-Apps over ApplicationSet?**
App-of-Apps is simpler to reason about and requires no CRD beyond the core
ArgoCD install. ApplicationSet adds power (generators, templating) that isn't
needed at this scale; it's an easy migration later.

**Why `target-type: ip` for the ALB?**
Pod IPs are registered directly — no NodePort hop, lower latency, and the ALB
health check talks directly to the pod's `/health` endpoint. The tradeoff is
that security group rules must explicitly allow ALB→pod traffic on port 8080.

**Why OIDC/IRSA instead of node IAM roles?**
IRSA binds an IAM role to a specific Kubernetes ServiceAccount. No other pod
on the same node can assume the role. Node IAM roles grant all pods on the node
the same permissions — a significant blast radius if one pod is compromised.

**Why `maxUnavailable: 0` in the rolling update?**
Guarantees zero downtime during deploys. A new pod must pass its readiness probe
before any old pod is terminated. The cost is slightly longer deploys when
cluster capacity is tight.

---

## Security

| Layer | Approach |
|---|---|
| **Secrets** | AWS Secrets Manager + CSI Secrets Store driver; values are mounted as files, never injected as env vars or stored in manifests |
| **IAM / RBAC** | IRSA binds a scoped IAM role to each service's `ServiceAccount` — no shared node-level roles; Kubernetes RBAC uses minimal `Role` bindings, no wildcards |
| **Network** | `NetworkPolicy` default-deny-all per namespace; explicit ingress rules for ALB→pod (:8080) and Prometheus→pod (:9090) only |
| **Image** | Distroless base image (no shell, no package manager); Trivy CRITICAL scan blocks the CI pipeline on every push to main |
| **Runtime** | `readOnlyRootFilesystem: true`, `runAsNonRoot: true` (UID 65532), all Linux capabilities dropped |
| **Ingress** | TLS terminates at the ALB; `ssl-redirect: "443"` annotation forces HTTPS; metrics port (:9090) is never exposed via Ingress |
| **Supply chain** | `go mod verify` in CI; GitHub Actions pinned to SHA instead of mutable tags |

---

## Tradeoffs

| Decision | Tradeoff accepted |
|---|---|
| `selfHeal: true` on dev only | Staging/prod require human intent for every sync; slower feedback |
| Trivy blocks only CRITICAL | MEDIUM/HIGH CVEs are visible but not blocking — reduces noise |
| `readOnlyRootFilesystem: true` | Requires the app to write nothing to disk (distroless guarantees this) |
| Go stdlib HTTP (no framework) | Less batteries-included routing but zero dependency surface |

---

## Production path — what we'd do differently

This project is a fully working proof of concept designed to run on **minikube without
any cloud account**. Every piece of it is intentionally swappable. Here is what the
production version of each layer would look like on AWS/EKS:

---

**Cluster: minikube → EKS**

Swap minikube for a real EKS cluster (eksctl, Terraform, or CDK). Node groups in
multiple AZs, managed node upgrades, and a VPC with public/private subnets. Nothing
in the application or Helm chart changes — only the `destination.server` in the
ArgoCD Application manifests.

**Ingress: nginx → AWS Application Load Balancer**

The current setup uses the nginx ingress controller, which works great locally but
isn't cloud-aware. In production we'd install the
[AWS Load Balancer Controller](https://kubernetes-sigs.github.io/aws-load-balancer-controller/)
and switch the ingress class to `alb`. This gives native target-group health checks,
WAF integration, and no extra hop through a NodePort. The Helm chart already has
the ALB annotations in `values.yaml` — enabling them is a one-line change in the
env values files.

**TLS / SSL: manual → Cloudflare**

Locally there is no TLS. In production we'd put Cloudflare in front of the ALB:

- Cloudflare terminates HTTPS and proxies traffic to the ALB, providing a globally
  distributed edge, DDoS protection, and a free managed SSL certificate with
  automatic renewal — zero cert-manager config required.
- The ALB→pod leg stays HTTP internally (traffic never leaves the AWS network).
- An alternative is `cert-manager` + Let's Encrypt on the cluster, but Cloudflare
  is simpler and adds the CDN and WAF layer for free.

**IAM: none → IRSA (IAM Roles for Service Accounts)**

In minikube there are no IAM permissions to worry about. On EKS every workload gets
its own `ServiceAccount` with an annotated IAM role via IRSA (OIDC-based). This
means the kite-service pod can read from Secrets Manager or write to S3 without
sharing credentials with any other pod on the same node — the blast radius of a
compromised pod is scoped to exactly that role's permissions.

**Image registry: minikube daemon → Amazon ECR**

Currently images are built directly into minikube's Docker daemon (`pullPolicy: Never`)
which means no registry is needed, but images only live on your laptop. In production:
- CI (GitHub Actions) builds and pushes to ECR on every merge to main
- Each environment pulls from its own ECR repository tag
- ECR image scanning runs on push; CRITICAL vulnerabilities block the pipeline

**Secrets: none → AWS Secrets Manager + CSI driver**

No real secrets exist in the PoC. In production, the AWS Secrets Store CSI driver
mounts secrets from Secrets Manager directly into the pod as files — never as
environment variables, never in manifests, never in git.

---

**What we'd polish with more time**

- **Service mesh (Istio or Linkerd)** — mTLS between pods, circuit breaking, and
  per-route traffic metrics without any app changes
- **OpenTelemetry tracing** — wire `go.opentelemetry.io/otel` into the HTTP middleware,
  ship to Tempo or Jaeger
- **ArgoCD notifications** — connect the `notifications.argoproj.io` annotations to
  a real Slack webhook so sync failures actually page someone
- **Pre-commit hooks** — `golangci-lint` and `helm lint` locally, not just in CI
- **Load testing baseline** — `k6` or `vegeta` in CI against dev to catch latency
  regressions before they reach staging

---

## Debugging runbook

See [`docs/debugging.md`](docs/debugging.md) for a structured 502/504 investigation
guide specific to this stack.
