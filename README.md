# Kite Service

A production-grade microservice onboarding exercise demonstrating Kubernetes deployment,
GitOps, CI/CD, and observability on EKS.

---

## Architecture

```
┌─────────────┐   push    ┌──────────────────────────────────────────────┐
│  Developer  │──────────▶│              GitHub Actions                  │
└─────────────┘           │  ci.yaml: test → build → Docker Hub push → scan │
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
│   ├── ci.yaml                 # test → build → Docker Hub push → Trivy scan
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

`values-local.yaml` overrides the image to a locally built tag, switches ingress to nginx, and
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

# 2 — Bootstrap the app-of-apps (reads from GitHub)
kubectl apply -n argocd -f gitops/argocd/appproject.yaml
kubectl apply -n argocd -f gitops/argocd/app-of-apps.yaml

# 3 — Access the ArgoCD UI (keep this terminal open)
kubectl port-forward svc/argocd-server -n argocd 8443:443 &
# Then open https://localhost:8443
# Username: admin
# Password: $(kubectl -n argocd get secret argocd-initial-admin-secret \
#              -o jsonpath="{.data.password}" | base64 -d)
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

- **`kite-service-dev` stays OutOfSync in minikube.** `values-dev.yaml` enables
  the ServiceMonitor, which requires the `monitoring.coreos.com/ServiceMonitor`
  CRD from kube-prometheus-stack. Without it, ArgoCD cannot apply the resource
  and the app stays OutOfSync. The pods are healthy — this is an expected
  limitation of simulating a cloud-native stack locally.

- **Staging/prod show Progressing in minikube.** Their Ingress uses
  `className: alb`, which requires the AWS Load Balancer Controller to assign an
  external address. Without it the Ingress never becomes ready. All pods are
  Running — this is also expected when running without cloud infrastructure.

- **`kite-staging` and `kite-prod` namespaces must be created manually.**
  `CreateNamespace=true` requires cluster-admin RBAC that ArgoCD's default
  minikube install doesn't have. Run before syncing:
  ```bash
  kubectl create namespace kite-staging
  kubectl create namespace kite-prod
  ```

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

CI (GitHub Actions) runs `go vet` and `go test` on every push to `main` and
every pull request — keeping the test gate in place even without a registry.

To promote to staging or prod:

```bash
# Via GitHub Actions UI (workflow_dispatch on cd.yaml)
gh workflow run cd.yaml \
  -f environment=staging \
  -f image_tag=1.2.3

# Then sync in ArgoCD UI, or:
argocd app sync kite-service-staging
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

Prometheus and Grafana are managed by ArgoCD via the app-of-apps — no manual
install needed. ArgoCD deploys `kube-prometheus-stack` into the `monitoring`
namespace automatically when the stack syncs.

Once Grafana is running, import the kite dashboard:

```bash
# Port-forward Grafana
kubectl port-forward svc/kube-prometheus-stack-grafana -n monitoring 3000:80

# Import via the API (default credentials: admin / admin)
curl -X POST http://admin:admin@localhost:3000/api/dashboards/import \
  -H "Content-Type: application/json" \
  -d "{\"dashboard\": $(cat observability/dashboards/kite-service.json), \"overwrite\": true, \"folderId\": 0}"
```

The PrometheusRule is applied by ArgoCD as part of each environment's Helm
release (`serviceMonitor.enabled: true` in the env values files).
Access Grafana at `http://localhost:3000` while the port-forward is running.

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

**Separate image repositories per environment**
Currently all environments pull from the same Docker Hub image and the tag is
environment-specific. Proper isolation would use separate repositories (or a
private registry per environment) so a dev image can never accidentally land
in prod.

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
