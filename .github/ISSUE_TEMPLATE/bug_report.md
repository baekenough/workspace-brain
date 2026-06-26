---
name: Bug report
about: Report a reproducible defect
labels: "type:bug"
---

## What happened

<!-- Describe the actual behavior. -->

## Expected behavior

<!-- Describe what you expected instead. -->

## Steps to reproduce

1.
2.
3.

## Environment

- Go version:
- OS:
- workspace-brain version/commit:
- Adapter in use (`API_TOKEN` / `SLACK_SIGNING_SECRET` / demo):

## Relevant logs or output

```
<!-- Paste relevant output here. Redact secrets and tokens. -->
```

## Area (check one)

- [ ] `area:gateway` — binding, auth, ID generation
- [ ] `area:core` — local RAG memory, embeddings, scoring
- [ ] `area:adapter-http` — JSON HTTP API
- [ ] `area:adapter-slack` — Slack adapter
- [ ] `area:sources` — source loader
- [ ] `area:server` — HTTP server, health/readiness
- [ ] `area:ci` — build, test, coverage pipeline
- [ ] `area:docs` — documentation
