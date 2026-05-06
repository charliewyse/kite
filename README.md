# Kite Service

A production-grade microservice onboarding exercise demonstrating Kubernetes deployment,
GitOps, CI/CD, and observability on EKS.

---

## Architecture

```
┌─────────────┐   push    ┌──────────────────────────────────────────────┐
│  Developer  │──────────▶│              GitHub Actions                  │
└─────────────┘           │  ci.yaml: test → build → ECR push → scan    │
                          │  cd.yaml: yq-patch values-dev.yaml → commit  │
                          └──────────────┬───────────────────────────────┘
                                         │ git commit (image tag bump)
                                         ▼
                          ┌──────────────────────────┐
                          │          ArgoCD           │
                          │  watches gitops/apps/     │
                          │  dev:     auto-sync       │
                          │  staging: manual sync     │
                          │  prod:    manual sync     │
                          └──────────────┬────────────┘
                                         │ applies Helm chart
                                         ▼
                    ┌────────────────────────────────────────┐
                    │                  EKS                    │
                    │                                         │
                    │  ┌─────────────────────────────────┐   │
                    │  │        kite-dev namespace        │   │
                    │  │                                  │   │
                    │  │  Deployment (2+ pods)            │   │
                    │  │    :8080  app traffic            │   │
                    │  │    :9090  Prometheus metrics     │   │
                    │  │        │                         │   │
                    │  │     Service (ClusterIP)          │   │
                    │  │        │                         │   │
                    │  │   ALB Ingress (HTTPS/443)        │   │
                    │  └─────────────────────────────────┘   │
                    │                                         │
                    │  Prometheus ──scrapes :9090──▶ Grafana  │
                    └────────────────────────────────────────┘
```

### Component inventory

| Component | Technology | Location |
|---|---|---|
| Application | Go 1.22, distroless image | `app/` |
| Packaging | Helm chart, per-env values | `helm/kite-service/` |
| GitOps | ArgoCD App-of-Apps | `gitops/` |
| CI/CD | GitHub Actions | `.github/workflows/` |
| Observability | PrometheusRule + Grafana JSON | `observability/` |

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
│   └── templates/              # deployment, service, ingress, hpa, servicemonitor, …
│
├── gitops/
│   ├── argocd/
│   │   ├── appproject.yaml     # scopes deployments to kite namespaces only
│   │   └── app-of-apps.yaml    # bootstrap — apply once
│   └── apps/{dev,staging,prod}/kite-service.yaml
│
├── .github/workflows/
│   ├── ci.yaml                 # test → build → ECR push → Trivy scan
│   └── cd.yaml                 # image tag bump (dev auto, staging/prod manual)
│
├── observability/
│   ├── alerts/kite-service-rules.yaml   # PrometheusRule (5 alerts + recording rules)
│   └── dashboards/kite-service.json     # Grafana dashboard (importable)
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

`values-local.yaml` overrides the ECR image, switches ingress to nginx, and
disables the ServiceMonitor (no Prometheus CRDs in a vanilla minikube).

```bash
# 1 — Build the image directly into minikube's Docker daemon (no registry needed)
eval $(minikube docker-env)
docker build -t kite-service:local ./app

# 2 — Install with local overrides layered on top of the dev values
kubectl create namespace kite-dev
helm install kite-service ./helm/kite-service \
  -n kite-dev \
  -f helm/kite-service/values.yaml \
  -f helm/kite-service/values-dev.yaml \
  -f helm/kite-service/values-local.yaml

# 3 — Hit the endpoints through the nginx ingress
MINIKUBE_IP=$(minikube ip)
curl -H "Host: kite.local" http://$MINIKUBE_IP/health
curl -H "Host: kite.local" http://$MINIKUBE_IP/ping
```

### Testing the ArgoCD GitOps flow locally

ArgoCD needs a git remote reachable from inside the cluster. The host machine
is accessible at `192.168.49.1` from minikube pods (Docker driver default).

```bash
# 1 — Initialise the repo and start a local git daemon on port 9418
git init && git add . && git commit -m "initial" && git branch -m main
git init --bare /tmp/kite-bare.git
git remote add local /tmp/kite-bare.git
git push local main
git daemon --base-path=/tmp --export-all --reuseaddr --port=9418 &

# 2 — Point the gitops manifests at the local daemon, then push
find gitops/ -name "*.yaml" -exec sed -i \
  's|https://github.com/ORG/kite|git://192.168.49.1/kite-bare.git|g' {} \;
# Also add "- values-local.yaml" to helm.valueFiles in
# gitops/apps/dev/kite-service.yaml so ArgoCD uses the local image overrides
git add gitops/ && git commit -m "chore: local git daemon URLs" && git push local main

# 3 — Install ArgoCD
kubectl create namespace argocd
kubectl apply -n argocd \
  -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
kubectl wait --for=condition=Ready pod \
  -l app.kubernetes.io/name=argocd-server -n argocd --timeout=180s

# 4 — Bootstrap the app-of-apps
kubectl apply -n argocd -f gitops/argocd/appproject.yaml
kubectl apply -n argocd -f gitops/argocd/app-of-apps.yaml
```

**Gotchas learned the hard way:**

- **Browser shows nginx 404 when hitting the minikube IP directly.** Nginx
  routes on the `Host` header, not the IP. Add an entry to `/etc/hosts` so
  the browser sends the right header:
  ```bash
  echo "$(minikube ip) kite.local" | sudo tee -a /etc/hosts
  ```
  Then `http://kite.local` works in the browser. Raw `curl` against the IP
  works as long as you pass `-H "Host: kite.local"`.


- **Don't `helm install` before ArgoCD takes ownership.** If you install
  manually first, ArgoCD will conflict with the existing release tracking
  metadata. Either let ArgoCD do the initial install, or `helm uninstall` first.
- **ArgoCD polls git every ~3 minutes.** After pushing a change, force an
  immediate refresh instead of waiting:
  ```bash
  kubectl -n argocd annotate application kite-service-dev \
    argocd.argoproj.io/refresh=hard --overwrite
  ```
- **`serviceMonitor.enabled: true` requires the Prometheus CRDs.** Without
  kube-prometheus-stack installed, the sync will fail with
  `could not find monitoring.coreos.com/ServiceMonitor`. The local overrides
  in `values-local.yaml` handle this, but they must be included in ArgoCD's
  `helm.valueFiles` list as well, not just the helm CLI invocation.
- **Revert before pushing to GitHub.** The local git daemon URLs and
  `values-local.yaml` additions to the Application manifests are local-only.
  Run `git diff` before pushing to make sure none of those changes leak into
  the production manifests.

---

## How to deploy (cloud)

### 1 — Install ArgoCD and bootstrap GitOps

```bash
kubectl create namespace argocd
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml

# Replace ORG in all gitops/ files with your GitHub org, then:
kubectl apply -n argocd -f gitops/argocd/appproject.yaml
kubectl apply -n argocd -f gitops/argocd/app-of-apps.yaml
```

ArgoCD will discover and create `kite-service-dev`, `kite-service-staging`, and
`kite-service-prod` Applications automatically.

### 2 — Configure GitHub

Set these on the repository before pushing:

| Key | Type | Value |
|---|---|---|
| `AWS_ROLE_ARN` | Secret | IAM role ARN for GitHub OIDC federation |
| `AWS_REGION` | Variable | e.g. `us-east-1` |
| `ECR_REPOSITORY` | Variable | e.g. `dev/kite-service` |

### 3 — Deploy

Push to `main`. CI runs tests, builds and pushes the image to ECR, scans it with
Trivy, then CD bumps `values-dev.yaml` with the new `sha-<7char>` tag. ArgoCD
auto-syncs dev within seconds.

To promote to staging or prod:

```bash
# Via GitHub Actions UI (workflow_dispatch on cd.yaml)
gh workflow run cd.yaml \
  -f environment=staging \
  -f image_tag=sha-abc1234

# Then sync in ArgoCD UI, or:
argocd app sync kite-service-staging
```

### 4 — Import observability

```bash
# PrometheusRule
kubectl apply -n monitoring -f observability/alerts/kite-service-rules.yaml

# Grafana dashboard: import observability/dashboards/kite-service.json
# via Grafana UI → Dashboards → Import, or via the Grafana API:
curl -X POST http://grafana/api/dashboards/import \
  -H "Content-Type: application/json" \
  -d "{\"dashboard\": $(cat observability/dashboards/kite-service.json), \"overwrite\": true}"
```

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

## Tradeoffs

| Decision | Tradeoff accepted |
|---|---|
| `selfHeal: true` on dev only | Staging/prod require human intent for every sync; slower feedback |
| Trivy blocks only CRITICAL | MEDIUM/HIGH CVEs are visible but not blocking — reduces noise |
| `readOnlyRootFilesystem: true` | Requires the app to write nothing to disk (distroless guarantees this) |
| Go stdlib HTTP (no framework) | Less batteries-included routing but zero dependency surface |

---

## What I would improve with more time

**Service mesh (Istio or Linkerd)**
mTLS between pods, circuit breaking, retry budgets, and richer traffic metrics
(per-route, per-upstream) without any app changes. Currently there is no
encryption on pod-to-pod traffic inside the cluster.

**OpenTelemetry tracing**
Wire `go.opentelemetry.io/otel` into the HTTP middleware so every request gets
a trace ID propagated through all downstream calls. Ship to Tempo or Jaeger.
Currently tracing is entirely absent.

**ArgoCD notifications**
Connect the `notifications.argoproj.io` annotations in the Application manifests
to a real Slack webhook so sync failures and successful deploys actually page
someone. The annotations are stubbed but the notification controller is not
installed.

**Separate ECR repositories per environment**
Currently the CD workflow pushes to a single repo and the tag is environment-
specific. Proper environment isolation uses separate repos with cross-account
pull permissions so a dev image can never accidentally land in prod.

**Pre-commit hooks**
`golangci-lint` and `helm lint` run in CI but not locally. Adding
`.pre-commit-config.yaml` gives developers the same checks before pushing.

**Load testing baseline**
No performance baseline exists. Adding a `k6` or `vegeta` script that runs in
CI against a dev deployment would catch latency regressions before they reach
staging.

---

## Debugging runbook

See [`docs/debugging.md`](docs/debugging.md) for a structured 502/504 investigation
guide specific to this stack (EKS + ALB Controller + `target-type: ip`).
