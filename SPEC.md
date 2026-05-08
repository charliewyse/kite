# Kite Service — Architecture Spec

## Overview

A production-grade microservice onboarding exercise — fully runnable on minikube with no cloud
account required. GitHub Actions CI, ArgoCD GitOps, and Prometheus/Grafana observability.
The app is a lightweight Go HTTP service. See the README Production path section for how each
layer maps to a real EKS deployment.

---

## Technology Choices

| Concern | Choice | Reason |
|---|---|---|
| Language | Go | Small binary, tiny Docker image, excellent HTTP stdlib |
| Container registry | minikube daemon | No registry required locally; `pullPolicy: Never` uses the image built directly into minikube |
| Package format | Helm | Templated values per environment; ArgoCD natively supports it |
| GitOps engine | ArgoCD | App-of-Apps pattern, sync policies, drift detection |
| Ingress | nginx (minikube PoC) → AWS ALB in production | nginx works without cloud; ALB gives native EKS health checks and WAF |
| Observability | Prometheus + Grafana | Matches stated stack; ServiceMonitor for scrape discovery |
| Secrets | AWS Secrets Manager + ASCP | Mounts secrets as files; no env-var leakage |

---

## Repository Layout

```
kite/
├── SPEC.md                        # this file
├── README.md                      # public-facing docs
├── Makefile                       # local release: build → tag values → git tag → push
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
│       ├── files/
│       │   └── kite-service-dashboard.json   # Grafana dashboard (embedded in ConfigMap)
│       └── templates/
│           ├── _helpers.tpl
│           ├── deployment.yaml
│           ├── service.yaml
│           ├── ingress.yaml
│           ├── hpa.yaml
│           ├── serviceaccount.yaml
│           ├── configmap.yaml
│           ├── servicemonitor.yaml      # Prometheus scrape target
│           ├── prometheusrule.yaml      # 5 alerts + recording rules
│           └── grafana-dashboard.yaml   # ConfigMap auto-loaded by Grafana sidecar
│
├── gitops/
│   ├── argocd/
│   │   ├── appproject.yaml          # scopes deployments to kite namespaces
│   │   └── app-of-apps.yaml         # root ArgoCD Application
│   └── apps/
│       ├── dev/kite-service.yaml
│       ├── staging/kite-service.yaml
│       ├── prod/kite-service.yaml
│       └── monitoring/kube-prometheus-stack.yaml
│
├── observability/
│   ├── dashboards/kite-service.json     # reference copy of Grafana dashboard
│   └── alerts/kite-service-rules.yaml   # reference copy of PrometheusRule
│
├── docs/
│   └── debugging.md                 # 502/504 debugging walkthrough
│
└── .github/
    └── workflows/
        └── ci.yaml                  # go vet + go test -race (test gate only)
```

---

## CI/CD Flow

```
push to main
  └─ ci.yaml (GitHub Actions)
       └── go vet + go test -race (test gate only, no image build or push)

make release VERSION=x.y.z  (run locally)
  ├── docker build into minikube daemon (pullPolicy: Never, no registry)
  ├── sed -i: bump image.tag in values-{dev,staging,prod}.yaml
  ├── git commit + git push origin main
  └── git tag vX.Y.Z + git push origin vX.Y.Z
      ArgoCD detects values file change → auto-syncs dev
      staging/prod require a manual sync in the ArgoCD UI
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
Defined in `helm/kite-service/files/kite-service-dashboard.json` and packaged as a
ConfigMap labelled `grafana_dashboard: "1"` by `templates/grafana-dashboard.yaml`.
Grafana's sidecar detects the label and loads it automatically — no manual import needed.

Four panels:
1. RPS (requests/sec) by status code
2. p50 / p95 / p99 latency
3. Error rate (5xx / total)
4. Pod restarts

### Alerts (PrometheusRule)
Deployed via `templates/prometheusrule.yaml` into each environment namespace.
`ruleSelectorNilUsesHelmValues: false` in kube-prometheus-stack lets Prometheus
pick up rules from any namespace without requiring the `release` label.

Recording rules pre-compute expensive aggregations; alerts query the recordings:

```yaml
# Recording rules (scoped to release namespace)
kite:http_requests:rate5m
kite:http_requests:error_rate5m
kite:http_request_duration_seconds:p50 / p95 / p99

# Alerts
- KiteHighErrorRate       — error rate > 5% for 2m   (warning)
- KiteHighLatencyP95      — p95 > 1s for 5m           (warning)
- KiteHighLatencyP99      — p99 > 2s for 5m           (critical)
- KitePodCrashLooping     — >1 restart/min for 5m     (critical)
- KiteDeploymentUnavailable — any unavailable replica for 5m (critical)
```

---

## Security Posture

| Layer | Approach |
|---|---|
| Secrets | AWS Secrets Manager + CSI driver; never in env vars or manifests |
| RBAC | Dedicated `ServiceAccount` per service; minimal ClusterRole (no wildcards) |
| Network | `NetworkPolicy` default-deny-all; explicit allow for ingress and metrics scrape |
| Image | Distroless base; Trivy CRITICAL scan blocks CI on every push to main |
| IAM | IRSA per workload; no instance-level IAM roles for pods |
| Ingress | TLS termination at ALB; `force-ssl-redirect` annotation |
| Supply chain | `go mod verify` in CI; pinned action versions (SHA) |

---

## Tradeoffs & What I'd Improve

| PoC choice | Production equivalent |
|---|---|
| minikube | EKS with managed node groups across multiple AZs |
| nginx ingress | AWS ALB Ingress Controller (`ingressClassName: alb`) |
| No TLS | Cloudflare in front of ALB — managed SSL, CDN, DDoS protection, zero cert-manager config |
| `pullPolicy: Never` (minikube daemon) | ECR per environment; CI pushes on merge to main |
| No IAM | IRSA — each `ServiceAccount` gets a scoped IAM role via OIDC; no node-level roles |
| No secrets | AWS Secrets Manager + CSI Secrets Store driver; files mounted into pods, never env vars |
| Single NAT GW | Per-AZ NAT gateway — eliminates AZ failure as a network single point |

**Also with more time:**
- Service mesh (Istio or Linkerd) for mTLS, circuit-breaking, and per-route traffic metrics
- OpenTelemetry tracing — wire OTLP → Tempo or Jaeger
- ArgoCD notification controller — make the Slack annotations actually work
- Pre-commit hooks for `golangci-lint` and `helm lint`
