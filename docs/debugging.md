# Debugging: Ingress 502 / 504 with Healthy-Looking Pods

## Scenario

The service is deployed. ArgoCD shows Synced/Healthy. `kubectl get pods` shows
`Running`. But every request through the Ingress returns a 502 or 504.

---

## Step 0 — Distinguish 502 from 504 before touching anything

These two codes point at different failure modes in the ALB:

| Code | What ALB is saying | Where to look first |
|---|---|---|
| **502** Bad Gateway | ALB reached the target but got a bad response — connection refused, RST, or no healthy targets at all | Endpoints, readiness, port mapping, security groups |
| **504** Gateway Timeout | ALB reached the target but it never replied within the timeout window | App latency, deadlocks, missing downstream deps, ALB idle timeout |

Knowing which one you have cuts the search space in half immediately.

```bash
curl -sv https://kite-dev.example.com/ping 2>&1 | grep "< HTTP"
# Also check ALB access logs if enabled — they include target_status_code
# which tells you whether the ALB even forwarded the request to a pod
```

---

## Step 1 — Check the ALB target group in AWS (outside-in)

With `alb.ingress.kubernetes.io/target-type: ip`, the ALB registers pod IPs
directly as targets and health-checks them independently of Kubernetes probes.
A pod can pass its Kubernetes readiness probe and still be `unhealthy` in the
ALB target group if the ALB cannot reach it.

```bash
# Get the ALB ARN from the Ingress
kubectl describe ingress kite-service -n kite-dev | grep "LoadBalancer Ingress"

# Then in the AWS console or CLI:
aws elbv2 describe-target-health \
  --target-group-arn <arn> \
  --query 'TargetHealthDescriptions[*].[Target.Id,TargetHealth.State,TargetHealth.Reason]'
```

**What to look for:**

- `initial` — targets just registered, health check hasn't run yet (transient)
- `unhealthy: Target.ResponseCodeMismatch` — ALB health check is getting a non-2xx from `/health`
- `unhealthy: Target.Timeout` — ALB cannot reach the pod at all on the health check port
- `unused: Target.NotRegistered` — the target group is empty (Ingress controller hasn't registered pods yet)

**Why this step first:** if targets are `unhealthy` in AWS, the ALB will 502 every
request regardless of what Kubernetes thinks. Diagnosing at the Kubernetes layer
when the problem is actually the AWS target group is a common time sink.

---

## Step 2 — Verify there are actual Endpoints

"Pod appears healthy" usually means `kubectl get pods` shows `Running`. But
`Running` is a phase, not a readiness state. A pod can be `Running 0/1` —
the container is up but the readiness probe is failing, so the Service
controller has removed it from the Endpoints object.

```bash
kubectl get endpoints kite-service -n kite-dev
# Healthy: kite-service   10.0.5.12:8080,10.0.6.44:8080   5m
# Broken:  kite-service   <none>                           5m

kubectl get pods -n kite-dev -o wide
# READY column: "1/1" = ready, "0/1" = running but not ready
```

**If Endpoints is empty or the READY column shows 0/1:**

```bash
kubectl describe pod <pod-name> -n kite-dev
# Look at:
#   Conditions: Ready = False
#   Events: Readiness probe failed: ...
#   Last State: shows if the container was OOMKilled or crashed recently
```

The readiness probe hitting `/ready` at `:8080` is correct — but if the app
crashed and restarted, there's a window between the container starting and the
probe passing where the pod is `Running 0/1`. During that window every request
through the Service gets a 502.

---

## Step 3 — Bypass the ALB and Service with a port-forward

This isolates whether the problem is in the pod itself or in the networking
layers above it (Service, Ingress, security groups).

```bash
kubectl port-forward pod/<pod-name> 8080:8080 -n kite-dev &
curl -s localhost:8080/health   # should return {"status":"ok"}
curl -s localhost:8080/ping     # should return {"message":"pong",...}
```

| port-forward result | What it tells you |
|---|---|
| Works fine | The pod is healthy. Problem is above — Service, Ingress, SG, or ALB config |
| Connection refused | The app is not listening on 8080 inside the container |
| Hangs / times out | The app is listening but not responding — deadlock, slow init, or blocked on a dep |
| `{}` or wrong body | The app is up but the handler is broken — check app logs |

```bash
kubectl logs <pod-name> -n kite-dev --tail=50
kubectl logs <pod-name> -n kite-dev -p   # -p = previous crashed container
```

---

## Step 4 — Check the Service port mapping

A mismatch between the Service's `targetPort` and the container's actual port
causes silent failures — the Service forwards traffic to a port where nothing
is listening, which the ALB sees as a connection refused → 502.

```bash
kubectl describe svc kite-service -n kite-dev
# Should show:
#   Port:       http  8080/TCP
#   TargetPort: http/TCP     ← named port — must match the container's port name
#   Endpoints:  10.0.5.12:8080,...
```

```bash
kubectl describe pod <pod-name> -n kite-dev | grep -A5 Ports
# Should show:
#   Ports: 8080/TCP  (name: http)
#          9090/TCP  (name: metrics)
```

Named ports (`targetPort: http`) resolve through the pod spec. If the Helm
chart's `service.port` value does not match `containerPort` in the Deployment,
traffic lands on the wrong port.

---

## Step 5 — Check security groups (the most common miss with ALB target-type: ip)

With `target-type: ip`, the ALB sends traffic directly to pod IPs over the VPC
network — it does **not** go through NodePort or kube-proxy. The traffic source
is the ALB's network interfaces, which sit in the VPC.

The EKS managed node security group must allow inbound TCP on port 8080 from
the ALB's security group. If the cluster was created without this rule, the ALB
health check and all traffic will be silently dropped → 502.

```bash
# Find the ALB security group
aws elbv2 describe-load-balancers --query 'LoadBalancers[*].[LoadBalancerArn,SecurityGroups]'

# Find the node security group
aws eks describe-cluster --name kite --query 'cluster.resourcesVpcConfig.clusterSecurityGroupId'

# Check that inbound 8080 from the ALB SG is allowed on the node SG
aws ec2 describe-security-groups --group-ids <node-sg-id> \
  --query 'SecurityGroups[*].IpPermissions'
```

**Fix:** add an inbound rule to the node security group:
- Type: Custom TCP
- Port: 8080 (or the container port range)
- Source: ALB security group ID

The AWS Load Balancer Controller can manage this automatically with the
`alb.ingress.kubernetes.io/manage-backend-security-group-rules: "true"` annotation,
but it requires the correct IAM permissions on the controller's IRSA role.

---

## Step 6 — Check for NetworkPolicy blocking traffic

If a default-deny NetworkPolicy is in place (recommended in production), traffic
from the ALB to the pods will be silently dropped unless an explicit allow rule
exists. This looks identical to a security group block from the pod's perspective.

```bash
kubectl get networkpolicy -n kite-dev
kubectl describe networkpolicy <name> -n kite-dev
```

For ALB `target-type: ip`, the source of traffic hitting the pod is the ALB's
ENI IP, which lives in a public subnet. The pod needs an ingress rule that allows
TCP on 8080 from the VPC CIDR (or the specific subnet CIDRs where the ALB lives).

---

## Step 7 — Check the ALB health check configuration

The ALB runs its own health check independently of the Kubernetes readiness probe.
If the health check path, port, or expected status code is wrong, targets will be
marked unhealthy and all traffic gets a 502 even if pods are perfectly fine.

```bash
kubectl describe ingress kite-service -n kite-dev | grep -i health
# Annotations should include:
#   alb.ingress.kubernetes.io/healthcheck-path: /health
#   alb.ingress.kubernetes.io/healthcheck-protocol: HTTP
```

Check the ALB target group health check settings in AWS console — the default
health check port for `target-type: ip` is `traffic-port` (the service port).
If this doesn't match what the app is listening on, health checks will fail.

---

## Step 8 — Check ArgoCD sync state (for 504s after a deploy)

504s immediately after a deploy often indicate that ArgoCD synced a new image
but the pods are still starting up. The readiness probe passes before the
application is fully warmed up.

```bash
argocd app get kite-service-dev   # check sync status and health
kubectl rollout status deployment/kite-service -n kite-dev
kubectl rollout history deployment/kite-service -n kite-dev
```

If the new version is consistently slow: check if the new image introduced a
slow initialisation path (loading a large model, running migrations, etc.) that
should have a longer startup probe budget.

---

## Root Cause Summary

| Symptom | Layer | Most likely cause | Fix |
|---|---|---|---|
| 502, targets `unhealthy` in AWS | ALB | SG not allowing ALB→pod on 8080 | Add inbound rule to node SG |
| 502, targets `unused` in AWS | ALB/Ingress | LBC not registered pods (IRSA missing perms?) | Check LBC pod logs; fix IAM |
| 502, Endpoints `<none>` | Service | Readiness probe failing | Fix probe config or the underlying issue |
| 502, Endpoints populated | Service/Pod | Wrong port in Service targetPort | Align port names in Deployment and Service |
| 502 on specific path only | Pod | App returning 502 itself (proxy, panic) | Check app logs; port-forward to reproduce |
| 504, pod responds via port-forward | ALB | ALB idle timeout too short | Increase `alb.ingress.kubernetes.io/load-balancer-attributes: idle_timeout.timeout_seconds=120` |
| 504, pod hangs on port-forward | Pod | App deadlock or blocked on downstream | Check app logs, traces; check dependency health |
| 504 only after deploy | Pod/App | Slow startup / warmup | Increase startupProbe budget; add warmup logic |
| 504 intermittent | Pod | Pod being terminated mid-request | Ensure `terminationGracePeriodSeconds` > longest request |

---

## Rollback if needed

```bash
# Revert the values file commit — ArgoCD will self-heal to the previous image
git revert HEAD -- helm/kite-service/values-dev.yaml
git push

# Or directly via ArgoCD
argocd app rollback kite-service-dev <revision>

# Emergency: pin a known-good image tag without waiting for git
argocd app set kite-service-dev --helm-set image.tag=sha-abc1234
argocd app sync kite-service-dev
```
