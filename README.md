# OpenCode Workspaces

A Helm-deployable, multi-user gateway for isolated [OpenCode](https://github.com/anomalyco/opencode) web workspaces.

> This community project is not built by or affiliated with the OpenCode team.

## What it provides

- Atomic first-user administrator bootstrap.
- Administrator-managed local accounts with Argon2id passwords.
- Runtime-configurable generic OpenID Connect login (discovery, state, nonce, PKCE, verified email, domain/group policies).
- One root-capable but unprivileged Kubernetes pod per user, with no service-account token or host access.
- On-demand startup, administrator controls, configurable idle scale-to-zero, and retained per-user `ReadWriteOnce` storage.
- A single authenticated hostname that proxies OpenCode HTTP, SSE, and WebSocket traffic.
- Admin screens for users, live pod status, OIDC, branding, and audit events.
- A pinned official OpenCode submodule plus a small replayable patch queue for branding and the admin-only UI link.

## Install with Helm

```bash
helm repo add opencode-workspaces https://ricsam.github.io/opencode-workspaces
helm repo update
helm upgrade --install opencode-workspaces opencode-workspaces/opencode-workspaces \
  --namespace opencode-workspaces --create-namespace \
  --set ingress.enabled=true \
  --set ingress.className=traefik \
  --set ingress.host=code.example.com \
  --set publicURL=https://code.example.com
```

The chart defaults use the matching release tags from `ghcr.io/ricsam/opencode-workspaces-server` and `ghcr.io/ricsam/opencode-workspaces-runtime`.

The default chart runs bundled PostgreSQL and requests `rook-ceph-block` storage. For an external database:

```yaml
postgresql:
  enabled: false
externalDatabase:
  existingSecret: opencode-db
  secretKey: database-url
```

Then open `/platform/setup`. The first transaction to create an account becomes administrator; later registration through that endpoint is rejected.

## Images

```bash
docker build -f build/Dockerfile.control-plane -t opencode-workspaces-control-plane:dev .
docker build -f build/Dockerfile.workspace -t opencode-workspaces-workspace:dev .
```

Tagged releases publish multi-architecture control-plane and workspace images to GHCR. The workspace image builds the customized official web app into the OpenCode binary. Its persistent mount is `/workspace`; the application uses `/workspace/home` and `/workspace/projects`. Package installs elsewhere in the root filesystem are intentionally ephemeral when the pod is recreated.

## Documentation

The Mintlify source is in [`docs/`](docs/README.md). Connect this repository in Mintlify as a monorepo with `/docs` as the documentation path. Locally:

```bash
npm install --global mint
cd docs
mint dev
```

## OpenCode upstream workflow

The official `anomalyco/opencode` `dev` branch is pinned at `upstream/opencode`. Customization commits are exported to `upstream/patches`.

```bash
# Fetch upstream dev and replay the patch queue
./scripts/refresh-opencode.sh origin/dev
make opencode-verify

# After editing and committing inside the submodule
./scripts/export-opencode-patches.sh <new-upstream-base-commit>
```

Commit both the updated submodule pointer and refreshed patches in this parent repository. The patch integration degrades to stock OpenCode when `/platform/api/v1/bootstrap` is unavailable.

## Verification

```bash
make verify
```

This runs Go formatting/vetting/tests, Helm lint/render/client validation, Mintlify validation/link checks, patch replay, and the official web app typecheck/build. Cluster admission must additionally be checked before installation:

```bash
helm template opencode-workspaces ./charts/opencode-workspaces \
  --namespace opencode-workspaces -f values.production.yaml > /tmp/opencode-workspaces.yaml
kubectl apply --dry-run=server -f /tmp/opencode-workspaces.yaml
```

## Security model

Workspace users are root *inside their container* to install development tools. Workspaces are still unprivileged: all Linux capabilities are dropped, privilege escalation is disabled, runtime-default seccomp is used, service-account token mounting is disabled, and no host path/socket is mounted. NetworkPolicies restrict ingress to the control plane and deny tenant-to-tenant private network access. Internet egress is configurable.

The control plane strips browser cookies, CSRF tokens, and client authorization before proxying and injects a generated per-workspace Basic Auth credential. OIDC client secrets are AES-GCM encrypted at rest. Critical session/encryption/database secrets are generated once with Helm `lookup` and retained across upgrades.

Root inside a container is not equivalent to a hardened hostile-code sandbox. Use a strong runtime isolation class (Kata/gVisor), admission controls, egress policy, quotas, and a dedicated cluster or node pool for mutually untrusted tenants.

## Storage and uninstall

Dynamic Deployments, Services, and internal Secrets reference a release-owned anchor and are garbage-collected. User PVCs deliberately do not: stopping or uninstalling does not erase workspace data. Use the explicit admin purge operation or delete the PVC manually only after taking a backup.

PostgreSQL should be backed up before chart upgrades. Its StatefulSet PVC is retained by normal Kubernetes StatefulSet semantics.
