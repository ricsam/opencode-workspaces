# OpenCode Workspaces documentation

This directory is a Mintlify project.

## Local preview

Install Node.js 20.17 or newer and the Mintlify CLI, then run:

```bash
npm install --global mint
cd docs
mint dev
```

Validate changes before pushing:

```bash
cd docs
mint validate
mint broken-links --check-anchors --check-redirects
```

## Hosted deployment

Connect `ricsam/opencode-workspaces` in the Mintlify dashboard, select the `main` branch, enable **Set up as monorepo**, and set the documentation path to `/docs`. Mintlify deploys documentation changes from that directory.
