# Kite Service — Architecture Spec

## Overview

A production-grade microservice onboarding onto EKS, with GitHub Actions CI, ArgoCD GitOps, and
Prometheus/Grafana observability. The app is a lightweight Go HTTP service.

---

## Technology Choices

| Concern | Choice | Reason |
|---|---|---|
| Language | Go | Small binary, tiny Docker image, excellent HTTP stdlib |
| Container registry | minikube daemon | No registry required locally; `pullPolicy: Never` uses the image built directly into minikube |
| Package format | Helm | Templated values per environment; ArgoCD natively supports it |
| GitOps engine | ArgoCD | App-of-Apps pattern, sync policies, drift detection |
| Ingress | AWS ALB Ingress Controller | Native EKS integration, target-group health checks |
| Observability | Prometheus + Grafana | Matches stated stack; ServiceMonitor for scrape discovery |
| Secrets | AWS Secrets Manager + ASCP | Mounts secrets as files; no env-var leakage |

---

## Repository Layout

```
kite/
├── SPEC.md                        # this file
├── README.md                      # public-facing docs
│
├── app/                           # application source
│   ├── Dockerfile
│   ├── go.mod
│   ├── go.sum
│   └── internal/
│       └── server/
│           ├── server.go          # HTTP server wiring
│           └── handlers.go        # /health, /ready, /metrics, business routes
│
├── helm/
│   └── kite-service/
│       ├── Chart.yaml
│       ├── values.yaml            # shared defaults
│       ├── values-dev.yaml
│       ├── values-staging.yaml
│       ├── values-prod.yaml
│       └── templates/
│           ├── _helpers.tpl
│           ├── deployment.yaml
│           ├── service.yaml
│           ├── ingress.yaml
│           ├── hpa.yaml
│           ├── serviceaccount.yaml
│           ├── configmap.yaml
│           └── servicemonitor.yaml   # Prometheus scrape target
│
├── gitops/
│   ├── argocd/
│   │   └── app-of-apps.yaml         # root ArgoCD Application
│   └── apps/
│       ├── dev/
│       │   └── kite-service.yaml
│       ├── staging/
│       │   └── kite-service.yaml
│       └── prod/
│           └── kite-service.yaml
│
├── observability/
│   ├── dashboards/
│   │   └── kite-service.json        # Grafana dashboard definition
│   └── alerts/
│       └── kite-service-rules.yaml  # PrometheusRule
│
├── docs/
│   └── debugging.md                 # Part 5 — 502/504 debugging walkthrough
│
└── .github/
    └── workflows/
        ├── ci.yaml                  # build + push + lint + test
        └── cd.yaml                  # image tag update → triggers ArgoCD sync
```

---

## CI/CD Flow

```
push to main
  └─ ci.yaml
       ├── go test ./...
       └── go vet + go test -race (test gate only, no image build)

  └─ Makefile release target (run locally)
       ├── docker build into minikube daemon (pullPolicy: Never, no registry)
       ├── yq-patch values-{dev,staging,prod}.yaml image.tag → commit → push
       └── git tag vX.Y.Z → push
           ArgoCD auto-syncs dev; staging/prod require manual sync
```

Rollback: revert the values commit — ArgoCD self-heals to the previous image tag.

---

## GitOps / ArgoCD Design

- App-of-Apps root application in `gitops/argocd/app-of-apps.yaml`
- Each environment folder contains ArgoCD `Application` manifests pointing at
  `helm/kite-service` with the appropriate `values-{env}.yaml`
- Sync policy:
  - dev: `automated: {prune: true, selfHeal: true}`
  - staging/prod: automated sync OFF; requires manual sync or PR approval
- `syncPolicy.retry` with exponential backoff on all envs

---

## Observability Design

### Metrics (Prometheus)
- `go_*` runtime metrics via `promhttp` handler on `:9090/metrics`
- Custom counters: `http_requests_total{method, path, status}`, `http_request_duration_seconds`
- ServiceMonitor in `helm/templates/servicemonitor.yaml` picked up by kube-prometheus-stack

### Dashboard (Grafana)
One dashboard with four panels:
1. RPS (requests/sec) by status code
2. p50 / p95 / p99 latency
3. Error rate (5xx / total)
4. Pod restarts

### Alert (PrometheusRule)
```yaml
- alert: KiteHighErrorRate
  expr: rate(http_requests_total{status=~"5.."}[5m]) / rate(http_requests_total[5m]) > 0.05
  for: 2m
  severity: warning

- alert: KiteHighLatency
  expr: histogram_quantile(0.95, rate(http_request_duration_seconds_bucket[5m])) > 1
  for: 5m
  severity: warning
```

---

## Security Posture

| Layer | Approach |
|---|---|
| Secrets | AWS Secrets Manager + CSI driver; never in env vars or manifests |
| RBAC | Dedicated `ServiceAccount` per service; minimal ClusterRole (no wildcards) |
| Network | `NetworkPolicy` default-deny-all; explicit allow for ingress and metrics scrape |
| Image | Distroless base; Trivy CRITICAL scan blocks CI on every push; Docker Hub vulnerability scanning enabled |
| IAM | IRSA per workload; no instance-level IAM roles for pods |
| Ingress | TLS termination at ALB; `force-ssl-redirect` annotation |
| Supply chain | `go mod verify` in CI; pinned action versions (SHA) |

---

## Tradeoffs & What I'd Improve

- **Single NAT GW** saves cost in dev/staging but is an AZ failure point — would use per-AZ in prod
- **Helm over raw manifests** adds complexity for trivial apps but is the right call once you have envs
- **No service mesh yet** — would add Istio/Linkerd for mTLS, circuit-breaking, and richer traces
- **OpenTelemetry tracing** is stubbed; would wire OTLP → Tempo in a follow-up
- **ArgoCD notifications** (Slack on sync failure) not yet wired
