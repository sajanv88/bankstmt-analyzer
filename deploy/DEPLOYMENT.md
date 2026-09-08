# Deploying bankstmt-analyzer

The [README](../README.md) covers what the service does, how to configure
it, and how to run it on a laptop. This document covers getting it into a
Kubernetes cluster and keeping it running there: what you have to provide,
what the chart does on your behalf, and what to check when it goes wrong.

Everything here assumes the published chart and image. To deploy from a
working tree instead, swap the chart reference
`oci://ghcr.io/sajanv88/charts/bankstmt-analyzer --version <x.y.z>` for the
local path `deploy/helm/bankstmt-analyzer`; nothing else changes.

## What a release consists of

A `v*` tag builds three artifacts, all from `release.yml` and all with
`GITHUB_TOKEN` alone:

| Artifact | Where | Tagged |
| --- | --- | --- |
| Container image, `linux/amd64` and `linux/arm64` | `ghcr.io/sajanv88/bankstmt-analyzer` | `1.2.3`, `1.2`, and `latest` for non-prerelease tags |
| Helm chart, OCI | `ghcr.io/sajanv88/charts/bankstmt-analyzer` | `1.2.3` |
| Binaries and checksums | GitHub Release | `v1.2.3` |

Chart `version` and `appVersion` are both the tag with its leading `v`
removed, which is exactly the image tag `docker/metadata-action` publishes.
That is what lets `image.tag` default to the chart's `appVersion`: a chart
always resolves to an image that exists. Pin the chart version on every
install and upgrade — an unpinned `helm upgrade` silently follows whatever
was published most recently.

## What the chart installs

Two Deployments, because the API and the worker scale on entirely different
signals — request rate for one, queue depth and Azure latency for the other.

| Object | Name | Notes |
| --- | --- | --- |
| Deployment | `<release>-api` | Runs `--api`. Liveness `/healthz`, readiness `/readyz`, startup probe allowing up to 60 s. |
| Deployment | `<release>-worker` | Runs `--worker`. No ports and no probes: it serves nothing, and its real liveness is the queue draining, which Kubernetes cannot observe. |
| Job | `<release>-migrate-<revision>` | `pre-install`/`pre-upgrade` hook at weight `-5`. Runs `--migrate --api=false --worker=false` and exits. |
| Service | `<release>` | ClusterIP, port 80 → container 8080. |
| Secret | `<release>-config` | Only when `config.existingSecret` is empty. |
| Ingress, HPAs, PDBs, PVC | — | All optional, all off or minimal by default. |
| ServiceAccount | `<release>` | Created by default; no RBAC is bound to it, because the service calls no Kubernetes API. |

Both workloads and the migration Job share one environment block from
`_helpers.tpl`, so all three read identical configuration — a migration
cannot run against a different database than the pods.

## What you must provide

The chart deploys the service and nothing else. Four dependencies are
yours, and all four are checked at startup, so a pod with any of them wrong
fails immediately and visibly rather than on the first upload.

| Dependency | Requirement |
| --- | --- |
| PostgreSQL | 16 is what CI and compose use. One database holding both the application tables and the taskQ broker. The role needs DDL rights — migrations create tables and take a Postgres advisory lock. |
| S3-compatible bucket | Must already exist. MinIO, AWS S3, Ceph and R2 are all fine. Credentials need read, write and delete on it. |
| Azure Mistral OCR | Endpoint and key. |
| Azure OpenAI | Endpoint, key, and a deployment name. The endpoint is the resource base URL — the client appends the deployment path, and an endpoint that already carries one is rejected at startup. |

Postgres is the only stateful thing the service owns. The bucket holds
uploaded PDFs, which are reproducible only by re-uploading.

## First install

### 1. Namespace

```sh
kubectl create namespace bankstmt
```

### 2. Credentials

The credentials go in a Secret **you** manage, referenced by
`config.existingSecret`. The alternative — letting the chart build a Secret
from values — puts every key into Helm's release history, where
`helm get values` will hand them to anyone who can read the release.

The keys are the environment variable names the service reads, and the pods
consume the whole Secret with `envFrom`. There is no mapping layer: a
renamed or misspelled key becomes an unset variable, and the process
refuses to start.

```sh
kubectl create secret generic bankstmt-analyzer-credentials \
  --namespace bankstmt \
  --from-literal=DATABASE_URL='postgres://user:pass@host:5432/bankstmt?sslmode=require' \
  --from-literal=AZURE_OCR_ENDPOINT='https://<resource>.services.ai.azure.com' \
  --from-literal=AZURE_OCR_API_KEY='<ocr-key>' \
  --from-literal=AZURE_OCR_MODEL='mistral-ocr-2503' \
  --from-literal=AZURE_OPENAI_ENDPOINT='https://<resource>.openai.azure.com' \
  --from-literal=AZURE_OPENAI_API_KEY='<openai-key>' \
  --from-literal=AZURE_OPENAI_DEPLOYMENT='<deployment>' \
  --from-literal=S3_ACCESS_KEY_ID='<access-key>' \
  --from-literal=S3_SECRET_ACCESS_KEY='<secret-key>'
```

[`helm/bankstmt-analyzer/secret.yaml`](helm/bankstmt-analyzer/secret.yaml)
is the same Secret as a manifest to edit and `kubectl apply`. It is
gitignored and excluded from the packaged chart: it is operator input, not
chart content.

For anything long-lived, generate the Secret from something that never puts
plaintext in a file — External Secrets, Sealed Secrets, or a CSI driver
against your credential store. The chart does not care how the Secret gets
there, only that it exists before the release.

### 3. A values file

Keep one per environment, in your own repository rather than this one:

```yaml
# values-prod.yaml
config:
  existingSecret: bankstmt-analyzer-credentials

env: production        # no Swagger UI
logLevel: info

storage:
  backend: s3
  s3:
    endpoint: https://s3.eu-west-1.amazonaws.com
    bucket: bankstmt-uploads
    region: eu-west-1
    usePathStyle: false  # AWS S3 proper; MinIO and most others need true
    prefix: prod

api:
  autoscaling:
    enabled: true
    minReplicas: 3
    maxReplicas: 12
  podDisruptionBudget:
    enabled: true
    minAvailable: 2

worker:
  replicaCount: 3
  terminationGracePeriodSeconds: 660   # see "Draining workers" below

extraEnv:
  # Default is "*", which is not what you want in front of real data.
  HTTP_CORS_ALLOWED_ORIGINS: "https://app.example.com"

ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    # See "Request body size" — without this, uploads fail with 413.
    nginx.ingress.kubernetes.io/proxy-body-size: 256m
  hosts:
    - host: bankstmt.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: bankstmt-tls
      hosts:
        - bankstmt.example.com
```

### 4. Install

```sh
helm install bankstmt oci://ghcr.io/sajanv88/charts/bankstmt-analyzer \
  --version 1.2.3 \
  --namespace bankstmt \
  --values values-prod.yaml \
  --wait --timeout 10m
```

`--wait` matters more than usual here: without it Helm returns as soon as
the objects are accepted, and the migration hook's outcome is the first
thing you actually want to know about.

### 5. Verify

```sh
kubectl -n bankstmt get pods
kubectl -n bankstmt rollout status deployment/bankstmt-api
kubectl -n bankstmt logs job/bankstmt-migrate-1

kubectl -n bankstmt port-forward svc/bankstmt 8080:80
curl -s localhost:8080/healthz   # {"status":"ok"}
curl -s localhost:8080/readyz    # {"status":"ok"} once Postgres answers
```

`/healthz` is a liveness signal only. `/readyz` pings the database, so it is
the one that tells you the release is actually wired up.

## Exposing the API

The chart renders one Ingress from `ingress.className`, `ingress.hosts` and
`ingress.tls`, pointing at the API Service on port 80. It always renders
`spec.rules` — there is no `defaultBackend` option — which matters for
Tailscale below.

Only the API Service has endpoints. The worker runs no HTTP server, and the
Service selector excludes it deliberately.

### ingress-nginx

```yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-body-size: 256m   # see below
  hosts:
    - host: bankstmt.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: bankstmt-tls
      hosts:
        - bankstmt.example.com
```

### Tailscale operator

With the [Tailscale Kubernetes operator][ts-ingress] the shape is different
in three ways worth knowing before you copy the block above.

**The hostname comes from `spec.tls[0].hosts[0]`, and it is a short name,
not an FQDN.** The operator uses that field as the Tailscale Service name
and appends your tailnet, so `bankstmt` becomes
`bankstmt.<your-tailnet>.ts.net`. There is no `secretName`: the operator
provisions a Let's Encrypt certificate itself, which is also why
[HTTPS must be enabled for the tailnet][ts-https] before any of this works.

**No cert-manager annotation, and the nginx annotations are inert.**
Anything `nginx.ingress.kubernetes.io/*` is ignored under
`ingressClassName: tailscale` — including the body-size annotation, which
is the one people carry over from an nginx setup and then wonder about.

**Tailscale's own examples use `defaultBackend`**, which this chart does not
render. Set `hosts[0].host` to the same short name you put in `tls`, so the
rule host and the Tailscale Service name agree:

```yaml
ingress:
  enabled: true
  className: tailscale
  hosts:
    - host: bankstmt
      paths:
        - path: /
          pathType: Prefix
  tls:
    - hosts:
        - bankstmt
```

Then read the MagicDNS name back off the object — the `ADDRESS` column is
populated once the proxy is up and the certificate is issued, which takes a
minute or two:

```sh
kubectl -n bankstmt get ingress bankstmt
# NAME       CLASS       HOSTS   ADDRESS                        PORTS
# bankstmt   tailscale   *       bankstmt.<tailnet>.ts.net      80, 443
```

`tailscale.com/funnel: "true"` as an Ingress annotation would publish it to
the open internet. Do not set it here: the API has no authentication (see
[Security posture](#security-posture)), so on a tailnet it is reachable by
your devices, and behind Funnel it is reachable by everyone.

**The simpler option for local work** is to skip the Ingress entirely and
let the operator expose the Service directly. The chart already passes
`service.annotations` through, so this needs no template change and no TLS
setup:

```yaml
ingress:
  enabled: false
service:
  annotations:
    tailscale.com/expose: "true"
    tailscale.com/hostname: bankstmt-dev
```

That gives a Layer 3 tailnet address for the Service rather than an HTTPS
proxy — plain HTTP on port 80, no certificate, and no body-size middleware
anywhere in the path.

[ts-ingress]: https://tailscale.com/docs/kubernetes-operator/ingress
[ts-https]: https://tailscale.com/kb/1153/enabling-https

### Request body size

The upload endpoint accepts up to `UPLOAD_MAX_FILES` (12) files of
`UPLOAD_MAX_FILE_BYTES` (20 MB) each — a single request of up to roughly
240 MB. Whether that survives the trip depends entirely on what is in front
of the Service.

| In front | Default body limit | What to do |
| --- | --- | --- |
| ingress-nginx | 1 MB | `nginx.ingress.kubernetes.io/proxy-body-size: 256m` |
| HAProxy | 100 KB | `haproxy.org/client-body-max-size: 256m` |
| Traefik | none by default | Nothing, unless a `buffering` middleware sets one |
| Tailscale operator | none documented | Nothing |
| Cloud load balancers | Varies, often generous | Check the backend service's limit |

ingress-nginx is the one that bites: at 1 MB **every multi-statement upload
fails with 413** and the service never sees the request. The Tailscale
proxy documents no equivalent cap, so a tailnet install is one of the few
where this needs no thought at all.

If you lower the two `UPLOAD_MAX_*` values through `extraEnv`, keep the
proxy limit the looser of the two. Rejections should come from the service,
which answers with an RFC 7807 problem document naming the offending file,
rather than from a proxy answering with an opaque error page.

## Storage backend

`storage.backend: s3` is the default and the right answer under this chart.
The API writes an upload's PDFs and the worker reads them, and they are
separate Deployments on separate nodes.

`local` remains supported, but only with `storage.persistence.enabled: true`
and a **ReadWriteMany** claim. With ReadWriteOnce, or with persistence off,
each pod gets its own volume, the worker cannot see what the API wrote, and
every upload fails at the OCR step. The chart's `NOTES.txt` warns about this
on install; the failure is otherwise silent until the first upload. The
volume must also be writable by uid 65532 — `fsGroup` is set to match, but
some CSI drivers ignore it.

## Sizing and scaling

| Workload | Driven by | Knob |
| --- | --- | --- |
| API | Request rate, upload size | `api.replicaCount`, or `api.autoscaling` |
| Worker | Queue depth, Azure latency | `worker.replicaCount`, `WORKER_CONCURRENCY` |
| Postgres | Both, plus the taskQ broker's polling | Sized outside this chart |

Two things about the worker are worth understanding before you tune it.

**The HPA is a poor proxy for the real signal.** Autoscaling here is on CPU
and optionally memory, but the worker spends most of a saga hop blocked on
an Azure call, using almost no CPU. A backlog of a thousand uploads can sit
in the queue without moving the CPU average at all. Treat
`worker.autoscaling` as a safety valve and set `replicaCount` for the load
you expect; if you need queue-depth scaling, KEDA against the taskQ broker
table is the shape that actually works.

**Concurrency multiplies with replicas.** Each worker pod runs
`WORKER_CONCURRENCY` (4) hops in parallel, so three replicas mean twelve
concurrent Azure calls. Check that against your OCR and OpenAI quota before
raising either number — exceeding it turns into 429s, which are retried,
which makes the backlog worse.

**`TASKQ_LEASE_DURATION` (15 m) must stay above the slowest single step.** A
lease that expires mid-OCR gets the message redelivered to a second worker
while the first is still working on it. The work is idempotent per upload
id, so this is waste rather than corruption — but it is waste that compounds
exactly when you are already behind.

## Draining workers

`worker.terminationGracePeriodSeconds` defaults to 120. The pool stops
dequeuing on SIGTERM and waits for in-flight hops — but a single hop can be
an `AZURE_OCR_TIMEOUT` of 5 minutes or an `AZURE_OPENAI_TIMEOUT` of 10.
**The default grace period is shorter than either**, so a rolling update or
a node drain can SIGKILL a worker in the middle of a model call.

Nothing is lost when that happens: the lease expires, taskQ redelivers, and
the saga resumes from the interrupted hop. But the run stalls for up to
`TASKQ_LEASE_DURATION` first, which users see as an upload that sat in
`processing` for a quarter of an hour.

If your deployments are frequent enough for that to matter, raise the grace
period above your longest Azure timeout:

```yaml
worker:
  terminationGracePeriodSeconds: 660   # AZURE_OPENAI_TIMEOUT 10m, plus slack
```

The cost is slower rollouts and slower node drains. Lowering the two
timeouts through `extraEnv` and keeping a short grace period is the other
valid trade — just make the two numbers agree deliberately rather than by
default.

## Upgrades

```sh
helm upgrade bankstmt oci://ghcr.io/sajanv88/charts/bankstmt-analyzer \
  --version 1.3.0 \
  --namespace bankstmt \
  --values values-prod.yaml \
  --wait --timeout 10m
```

What happens, in order:

1. The migration Job runs as a `pre-upgrade` hook and Helm waits for it. Its
   name carries the release revision, so it never collides with the previous
   one. On failure the upgrade stops here and **the running release is
   untouched** — old pods keep serving.
2. Both Deployments roll. The API rolls behind its readiness probe; the
   worker has no probe, so its rollout is only as safe as the grace period
   discussed above.
3. `migration.backoffLimit` (3) and `activeDeadlineSeconds` (600) bound how
   long a broken migration can hold the upgrade open.

Because the schema always moves before the code, **migrations must be
backward compatible with the release currently running**. During the roll,
and after any rollback, old pods talk to the new schema. Additive changes
are safe; a dropped or renamed column needs two releases — one that stops
using it, one that removes it.

A failed hook Job is deliberately retained (`hook-delete-policy:
before-hook-creation,hook-succeeded`) so you can read its logs. The next
attempt deletes it before creating its own.

If you use `config.existingSecret` and rotate a credential, the chart cannot
see the change and will not roll the pods — that Secret is outside the
release. Restart them yourself:

```sh
kubectl -n bankstmt rollout restart \
  deployment/bankstmt-api deployment/bankstmt-worker
```

The chart-managed Secret does carry a `checksum/config` annotation, so that
path rolls automatically.

## Rollback

```sh
helm rollback bankstmt <revision> --namespace bankstmt --wait
helm history bankstmt --namespace bankstmt
```

**Rollback does not undo migrations.** The Job is annotated `pre-install`
and `pre-upgrade` only, so `helm rollback` runs no migration at all: the
schema stays where the failed upgrade left it and the old image runs against
it. That is the safe default — automatic down-migrations lose data — and it
is why the backward-compatibility rule above matters.

If a migration itself is the problem, roll the application back first to
stop the bleeding, then decide about the schema deliberately, with a backup
in hand. The image ships no shell and no goose binary, so a manual
down-migration means running it from a checkout against the production
connection string:

```sh
goose -dir db/migrations postgres "$DATABASE_URL" status
goose -dir db/migrations postgres "$DATABASE_URL" down
```

Treat that as a break-glass procedure, not a routine one.

## Smoke test

Worth running after any upgrade that touched the pipeline. Against a
port-forward, with a small real statement:

```sh
kubectl -n bankstmt port-forward svc/bankstmt 8080:80 &

id=$(curl -s -X POST localhost:8080/api/v1/uploads \
  -F "files=@statement.pdf" | jq -r .id)

# pending -> processing -> completed, typically a minute or two
watch -n5 "curl -s localhost:8080/api/v1/uploads/$id/status | jq"

curl -s "localhost:8080/api/v1/uploads/$id/visualization" | jq .summary
```

This exercises every external dependency in one pass: Postgres on upload,
the bucket on write and read, Azure OCR, and Azure OpenAI. A `failed` status
carries `failure_reason` in the form `"<step>: <reason>"`, which names the
hop that broke. Configured secrets are stripped from that string before it
is logged, queued or stored.

## Runbook

| Symptom | Likely cause | Where to look |
| --- | --- | --- |
| Pods `CrashLoopBackOff`, logs name a config variable | A key missing or misspelled in the Secret; with `envFrom` a typo is simply an unset variable | `kubectl -n bankstmt logs deploy/bankstmt-api` |
| Pods crash on startup naming the bucket | Bucket does not exist, wrong endpoint, or credentials cannot write | Startup does a bucket check on purpose, so this fails now rather than at the first upload |
| `/readyz` failing while `/healthz` passes | Postgres unreachable or refusing the connection | `DATABASE_URL`, `sslmode`, network policy, Postgres connection limit |
| Uploads return 413 | Proxy body limit below ~240 MB (ingress-nginx defaults to 1 MB) | [Request body size](#request-body-size) |
| Uploads stick in `pending` | No worker running, or it cannot reach Postgres | `kubectl -n bankstmt logs deploy/bankstmt-worker` |
| Uploads stick in `processing` for ~15 minutes | A worker was killed mid-hop; the lease has to expire before redelivery | [Draining workers](#draining-workers) |
| `failed` with `ocr:` or `analyze:` | Azure rejected the call — bad key, wrong endpoint shape, quota, or content filter | Worker logs carry the retryable/non-retryable classification; the budget is `WORKER_MAX_RETRY` per hop |
| Upgrade hangs, then fails | Migration Job never completed | `kubectl -n bankstmt logs job/bankstmt-migrate-<revision>` — the failed Job is kept for exactly this |
| `visualization` returns 409 | The upload is not `completed` yet | Poll `/status` first; this is the documented contract, not a fault |
| Swagger UI reachable in production | `env` is not `production` | Set `env: production`; the route is then absent entirely |

## Backup and recovery

Postgres holds everything that matters: uploads, OCR output, analyses,
transactions, and the taskQ broker state. Back it up with whatever your
platform provides — the service adds no requirements beyond a consistent
snapshot, and migrations are the only thing that changes the schema.

The bucket holds the source PDFs. Losing it does not corrupt existing
analyses, which are already in Postgres; it only means OCR cannot be re-run
for those uploads. Versioning and lifecycle rules are yours to set. Note
that nothing in the service ever deletes an uploaded PDF, so without a
lifecycle rule the bucket grows without bound.

To restore: restore Postgres, restore or accept the loss of the bucket, and
`helm upgrade` at the chart version matching the schema you restored.

## Security posture

What the chart already does, so you know what you would be weakening:

- Distroless static base — no shell, no package manager — running as uid
  65532 with `runAsNonRoot`, `readOnlyRootFilesystem`, no privilege
  escalation, all capabilities dropped, and `seccompProfile: RuntimeDefault`.
  `/tmp` is an `emptyDir` because multipart handling needs somewhere to
  spill.
- Credentials reach the pods only through `envFrom` on a Secret, so nothing
  sensitive is rendered into a pod spec where `kubectl describe` would show
  it.
- The ServiceAccount has no RBAC bound to it.

What is yours:

- `HTTP_CORS_ALLOWED_ORIGINS` defaults to `*`. Set it via `extraEnv`.
- There is **no authentication on the API**. Anyone who can reach the
  Ingress can upload statements, and anyone holding an upload's UUID can
  read its analysis. Put it behind your own gateway, or keep it internal.
- TLS is the Ingress's job; the service speaks plain HTTP on 8080.
- Network policy: the pods need egress to Postgres, the bucket, and the two
  Azure endpoints, and nothing else.

## Observability

Deliberately minimal, and you should know the shape of the gap before you
rely on it.

- Structured JSON logs on stdout via slog, at `LOG_LEVEL`. Every HTTP
  response carries a request id, which also appears in problem documents.
- `/healthz` and `/readyz` on the API.
- **No metrics endpoint and no tracing.** There is no `/metrics`, no
  Prometheus client, and no OpenTelemetry in the dependency tree.

So queue depth, saga step durations and Azure latencies are visible in the
logs and nowhere else. Until that changes, the practical monitors are: a
log-based alert on `failed` uploads, a Postgres query against the taskQ
broker table for backlog, and the ordinary Kubernetes signals — restarts,
readiness, and the migration Job's completion.
