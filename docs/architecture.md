# Architecture and operations

> The published Mintlify architecture page is maintained in [`architecture.mdx`](architecture.mdx). This source-oriented note remains available for contributors.

## Request flow

The control plane owns `/platform/*`; every other path is authenticated and forwarded to the caller's workspace Service. This lets the upstream web application continue to use root-relative browser and API routes. `httputil.ReverseProxy` preserves streaming responses and protocol upgrades. The gateway removes platform cookies and browser authorization before adding an internal OpenCode Basic Auth header.

## Resource reconciliation

A database workspace row is the desired state. The controller continuously reconciles:

- a retained `ReadWriteOnce` PVC;
- an internal password Secret;
- a ClusterIP Service;
- a `Recreate` Deployment with zero or one replica.

Deployments, Services, and Secrets are owned by a stable release ConfigMap. PVCs have labels but no owner reference. Activity is coalesced to one database update per minute. Workspaces older than `workspace.idleTimeout` become stopped.

## Identity

Local passwords use Argon2id (`64 MiB`, three iterations, two lanes). Session and CSRF values are random, only hashes are stored, and browser cookies are Secure/HttpOnly/SameSite where appropriate. The bootstrap transaction takes an exclusive users-table lock.

OIDC settings use discovery. Authorization uses state, nonce, and S256 PKCE. An ID token must contain a verified email. Existing identities key on `(issuer, subject)`. JIT provisioning, allowed domains, and admin groups are configured in the admin console. OIDC settings, including the client secret, are AES-GCM encrypted using `ENCRYPTION_KEY`.

## Backup and recovery

Back up PostgreSQL and all workspace PVCs. Restore PostgreSQL before the control plane, then restore PVCs with their original claim names. Critical Kubernetes Secret values must also be backed up; changing `ENCRYPTION_KEY` makes stored OIDC configuration unreadable and changing `SESSION_SECRET` signs users out.

## Upgrade

1. Back up PostgreSQL.
2. Build immutable control-plane and workspace image tags.
3. Run `make verify`.
4. Render the exact values and run server-side dry-run.
5. Run `helm upgrade --install` and wait for the StatefulSet and Deployment rollouts.
6. Run `helm test` and create a disposable workspace before broad use.

Migrations are serialized with a PostgreSQL advisory lock and applied transactionally at startup.

## OIDC provider setup

Register this exact redirect URL:

```text
https://<host>/platform/auth/oidc/callback
```

The provider must support discovery, authorization code flow, and ID tokens. Ensure `openid profile email` scopes expose a verified email. Start with JIT disabled, validate one pre-provisioned flow, then enable domain-restricted JIT if desired.

## Threat model

The design protects users from accidentally reaching another tenant's Service through the platform and avoids giving workspaces Kubernetes or host credentials. It is not a proof-grade hostile-code sandbox: users execute as UID 0 with a writable image filesystem. Production must use runtime isolation such as Kata, default-deny policies enforced by the CNI, resource quotas, restricted admission policy, and preferably dedicated infrastructure.
