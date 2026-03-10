# locked-init

A lightweight Kubernetes utility that ensures an initContainer runs once across a scaled workload, using native Kubernetes Lease objects for leader election.

## When is this useful?

locked-init solves a specific problem: when multiple replicas of a Deployment start simultaneously, all of their initContainers run concurrently. This is fine for most init tasks, but causes issues for a narrow set of cases:

- **Migration tools that don't handle concurrency.** Mature tools like Flyway, Liquibase, and golang-migrate use database-level advisory locks internally. If your tool already does this, you don't need locked-init. But simpler migration scripts (hand-rolled SQL, `prisma migrate deploy`, some ORMs) will deadlock or corrupt state when run in parallel.
- **Non-idempotent init tasks.** Seed scripts that insert initial data, one-time provisioning calls to external APIs, or any setup command that breaks when run twice concurrently.
- **Cases where a Kubernetes Job isn't an option.** If your deployment pipeline requires the init task to be part of the Pod lifecycle (not a separate Job or Helm hook), locked-init wraps it in place without changing your deployment model.

If your migration tool already serializes at the database level, or you can use a `Job` with `completions: 1`, you probably don't need this.

## How it works

Two components, built from a single Go codebase:

### CLI wrapper (`/bin/locked-init`)

A statically compiled binary that wraps any command with Kubernetes Lease-based leader election:

```
/bin/locked-init --name=<lock-name> -- <command> [args...]
```

1. **Fast path:** Checks if the Lease already has the annotation `locked-init.io/status: completed`. If so, exits 0 immediately.
2. **Leader election:** Enters leader election for the named Lease.
3. **Leader wins:** Executes the wrapped command. On exit 0, annotates the Lease as completed and exits. On non-zero exit, crashes the init container so another pod can retry.
4. **Followers wait:** Poll the Lease every 2 seconds. When the completed annotation appears, exit 0 without running the command.

**Guarantees:** Mutual exclusion (at most one instance runs the command at a time) with at-least-once semantics. If the leader crashes after the command succeeds but before writing the annotation, a new leader will re-run it. Your wrapped command should be idempotent.

### Mutating admission webhook

A standard Kubernetes webhook that watches for Pods annotated with `locked-init.io/run-once: "true"` and automatically injects the wrapper:

1. Adds an `emptyDir` volume (`locked-init-bin`)
2. Prepends an initContainer that copies the `locked-init` binary into the volume
3. Mutates each existing initContainer's command to run through the wrapper

**Before:**
```yaml
initContainers:
  - name: migrate
    image: myapp:latest
    command: ["npm", "run", "migrate"]
```

**After (injected automatically):**
```yaml
initContainers:
  - name: locked-init-copy
    image: locked-init:latest
    command: ["/bin/locked-init", "copy", "/locked-init-bin/locked-init"]
  - name: migrate
    image: myapp:latest
    command: ["/locked-init-bin/locked-init", "--name=replicaset-myapp-7f8b9c-migrate", "--", "npm", "run", "migrate"]
```

The lock name is derived deterministically from the Pod's controller OwnerReference (the ReplicaSet name), so all replicas of the same rollout compete for the same Lease. Each new rollout gets a new ReplicaSet hash, which means a fresh Lease and a fresh election.

## Usage

### Opt in a Deployment

Add the annotation to your Pod template:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  replicas: 5
  template:
    metadata:
      annotations:
        locked-init.io/run-once: "true"
    spec:
      initContainers:
        - name: migrate
          image: myapp:latest
          command: ["npm", "run", "migrate"]
      containers:
        - name: app
          image: myapp:latest
```

### RBAC for application pods

Pods running the wrapper need permission to manage Leases. Bind the provided ClusterRole to your application's ServiceAccount:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: myapp-locked-init
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: locked-init-lease-manager
subjects:
  - kind: ServiceAccount
    name: myapp
    namespace: default
```

The `locked-init-lease-manager` ClusterRole grants `get`, `create`, `update`, `patch` on `coordination.k8s.io/leases`.

## Deployment

### Build

```sh
# CLI wrapper image
docker build --target locked-init -t locked-init:latest .

# Webhook server image
docker build --target webhook -t locked-init-webhook:latest .
```

### Install

```sh
kubectl apply -f deploy/
```

This creates:
- `locked-init-system` namespace
- Webhook Deployment (2 replicas) with auto-rotating TLS certificates
- Service, ServiceAccount, and RBAC
- `MutatingWebhookConfiguration` (caBundle injected automatically by the cert rotator)
- `locked-init-lease-manager` ClusterRole for application pods

Update `deploy/deployment.yaml` to point `--image` at your locked-init image registry path.

## Architecture

```
Pod Create → MutatingWebhook intercepts
  → Injects locked-init binary via emptyDir
  → Wraps initContainer commands

Pod starts initContainers:
  1. locked-init-copy: copies binary into shared volume
  2. migrate (wrapped):
     → Checks Lease annotation → already done? exit 0
     → Enters leader election on Lease
     → Leader: runs command → annotates Lease → exit 0
     → Follower: polls Lease → sees annotation → exit 0

All replicas proceed to main containers.
```

## Constraints

- **No CRDs.** Uses only native `coordination.k8s.io/v1` Lease objects.
- **No external dependencies.** No Redis, etcd, or databases. Relies 100% on the Kubernetes API server.
- **Static binary.** Built with `CGO_ENABLED=0` for distroless/scratch compatibility.
- **Lease accumulation.** Old Lease objects from previous rollouts are not automatically cleaned up. They are small (a few hundred bytes) and can be garbage collected with a simple cron job or TTL controller if needed.
