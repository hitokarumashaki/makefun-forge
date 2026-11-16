# makefun-forge

[![make.fun](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01) [![Rank](https://make.fun/badge/github-core-clone-01/hitokarumashaki/makefun-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01) [![Reward](https://make.fun/badge/github-core-clone-01/hitokarumashaki/makefun-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01)

A GitHub-compatible git forge, built for the make.fun bounty
[github-core-clone-01](https://make.fun/bounty/github-core-clone-01). One process,
standard library only, Node 22. It serves four surfaces from a single in-memory
store, so an object created on one surface reads back identically on the others:

- **Git smart HTTP** at `/{owner}/{repo}.git` — upload-pack protocol v2 (and v0)
  and receive-pack v0, with its own object store (blobs, trees, commits, tags),
  refs, packfile generation and parsing (plain objects plus OFS/REF delta reading),
  shallow/deepen support and per-ref fast-forward enforcement.
- **REST v3** at `/api/v3` — repositories, refs, branches, commits, contents
  (create/update/delete with real git commits), labels, issues, comments and
  pull requests (create, files, squash-merge with three-way conflict detection).
- **GraphQL** at `/api/graphql` — viewer, repository, issue and pull-request
  reads, `__type` introspection, `node(id:)` global ids, cursor pagination and
  the create/close/merge/addComment mutations, plus two-operation batches.
- **MCP** at `/mcp` — JSON-RPC 2.0 Streamable HTTP, `server/discover`
  (revision 2026-07-28) with the legacy `initialize` handshake, and six tools:
  `create_repo`, `get_repo`, `create_issue`, `list_issues`,
  `create_pull_request`, `merge_pull_request`.

## Run it

```sh
node --experimental-strip-types main.ts
```

The entry binds an ephemeral port, registers it with `$MAKEFUN_ENV_URL`, and
polls `/done` until the trial ends (see `main.ts`, kept as the starter ships
it). Authentication is `Authorization: Bearer $MAKEFUN_SHOP_TOKEN` on every API
surface; `/healthz` is unauthenticated. For a local run without an env, start
the server directly (`app.ts` via a small runner or the local conformance
runner) and use any token value consistently.

## Layout

| File | What it holds |
| --- | --- |
| `main.ts` | Entry ↔ env lifecycle (starter, unchanged). |
| `app.ts` | HTTP router over the shared store. |
| `http.ts` | Request context, JSON responses, pagination helpers. |
| `store.ts` | The shared store: repos, objects, refs, issues, pulls, merges. |
| `git-core.ts` | Git object model, pkt-lines, packfiles, reachability. |
| `git-http.ts` | Git smart-HTTP framing (v2/v0 upload-pack, v0 receive-pack). |
| `rest.ts` | REST v3. |
| `graphql.ts` | GraphQL parser + executor. |
| `mcp.ts` | MCP (server/discover, initialize, tools). |
| `scenario.ts` | The seeded scenario's viewer login, ported from the kit's wire. |

## License

MIT — see `LICENSE`.
