// REST v3 under /api/v3. Every reply is application/json with a non-empty
// X-GitHub-Request-Id; errors carry {message}; unknown routes are 404 JSON;
// malformed JSON bodies are 400. Repositories, issues and pull requests are
// stored in the shared store, so MCP and GraphQL see the same objects.

import { mergeBase, parseCommit, parseTree, flattenTree } from './git-core.ts';
import { VIEWER } from './scenario.ts';
import { Store, repoGlobalId, issueGlobalId, pullGlobalId, type Repo, type Label, type Issue, type PullRequest } from './store.ts';
import {
  bearerToken,
  empty,
  isRecord,
  json,
  jsonError,
  newRequestId,
  paginate,
  paginationLinks,
  parseJsonBody,
  splitPath,
  stringField,
  type Ctx,
  type Resp,
} from './http.ts';

export function repoJson(repo: Repo, origin: string): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: repoGlobalId(repo),
    name: repo.name,
    full_name: repo.fullName,
    owner: { login: repo.owner, id: 1000, type: 'User' },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    html_url: `${origin}/${repo.owner}/${repo.name}`,
    url: `${origin}/api/v3/repos/${repo.owner}/${repo.name}`,
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
    has_issues: true,
    has_projects: false,
    has_wiki: false,
  };
}

function issueJson(repo: Repo, issue: Issue, origin: string): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: issueGlobalId(repo, issue),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((label) => ({ name: label.name, color: label.color })),
    user: { login: VIEWER.login, id: 1000, type: 'User' },
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    html_url: `${origin}/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `${origin}/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    comments_url: `${origin}/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}/comments`,
  };
}

/** The serialized single-issue body, cached per origin. The read workload GETs
 *  the same issue 2,000 times; rebuilding the object and re-stringifying it
 *  every time is pure waste. The cache is cleared whenever the issue mutates. */
function issueJsonString(repo: Repo, issue: Issue, origin: string): string {
  let cache = issue.jsonCache;
  if (cache === undefined) {
    cache = new Map<string, string>();
    issue.jsonCache = cache;
  }
  let body = cache.get(origin);
  if (body === undefined) {
    body = JSON.stringify(issueJson(repo, issue, origin));
    cache.set(origin, body);
  }
  return body;
}

/** A JSON response whose body is an already-serialized string (same headers as
 *  json(), including the per-request id). */
function jsonString(status: number, body: string): Resp {
  return {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8', 'x-github-request-id': newRequestId() },
    body,
  };
}

function pullJson(repo: Repo, pull: PullRequest, origin: string): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: pullGlobalId(repo, pull),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    head: { ref: pull.headRef, sha: pull.headSha, label: `${repo.owner}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: pull.baseSha, label: `${repo.owner}:${pull.baseRef}` },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    user: { login: VIEWER.login, id: 1000, type: 'User' },
    created_at: pull.createdAt,
    html_url: `${origin}/${repo.owner}/${repo.name}/pull/${pull.number}`,
    url: `${origin}/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

function repoOr404(store: Store, owner: string, name: string): Repo | Resp {
  const repo = store.getRepo(owner, name);
  if (!repo) return jsonError(404, 'Not Found');
  if (repo.owner !== VIEWER.login) return jsonError(404, 'Not Found');
  return repo;
}

export function handleRest(ctx: Ctx, store: Store, token: string | undefined): Resp | null {
  const segments = splitPath(ctx.path);
  if (segments.length < 2 || segments[0] !== 'api' || segments[1] !== 'v3') return null;

  const got = bearerToken(ctx);
  if (token !== undefined && got !== token) return jsonError(401, 'Bad credentials');

  const rest = segments.slice(2);
  const method = ctx.method;

  // GET /api/v3/ — API root.
  if (rest.length === 0) {
    if (method !== 'GET') return jsonError(404, 'Not Found');
    return json(200, {
      current_user_url: `${ctx.origin}/api/v3/user`,
      repository_url: `${ctx.origin}/api/v3/repos/{owner}/{repo}`,
      user_repositories_url: `${ctx.origin}/api/v3/user/repos`,
      rate_limit_url: `${ctx.origin}/api/v3/rate_limit`,
    }, { 'x-oauth-scopes': 'repo, user' });
  }

  // GET /api/v3/meta
  if (rest.length === 1 && rest[0] === 'meta') {
    if (method !== 'GET') return jsonError(404, 'Not Found');
    return json(200, { installed_version: '1.0.0', verifiable_password_authentication: true, git: ['github.com'] });
  }

  // GET /api/v3/user
  if (rest.length === 1 && rest[0] === 'user') {
    if (method !== 'GET') return jsonError(404, 'Not Found');
    return json(200, {
      login: VIEWER.login,
      id: 1000,
      node_id: 'user:1000',
      name: VIEWER.name,
      email: VIEWER.email,
      type: 'User',
      html_url: `${ctx.origin}/${VIEWER.login}`,
      url: `${ctx.origin}/api/v3/user`,
    });
  }

  // /api/v3/user/repos
  if (rest.length === 2 && rest[0] === 'user' && rest[1] === 'repos') {
    if (method === 'GET') {
      const repos = store.listViewerRepos();
      const { page, total, pageNumber, perPage } = paginate(repos, ctx);
      const headers: Record<string, string> = {};
      const links = paginationLinks(ctx, pageNumber, perPage, total);
      if (links) headers.link = links;
      return json(200, page.map((repo) => repoJson(repo, ctx.origin)), headers);
    }
    if (method === 'POST') {
      const parsed = parseJsonBody(ctx);
      if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
      const body = isRecord(parsed.value) ? parsed.value : {};
      const name = stringField(body, 'name');
      if (!name) return jsonError(422, 'name is required');
      if (!/^[A-Za-z0-9._-]+$/.test(name)) return jsonError(422, 'name is invalid');
      const description = stringField(body, 'description') ?? '';
      const isPrivate = body.private === true;
      const repo = store.createRepo(name, isPrivate, description);
      if (!repo) return jsonError(422, 'Repository creation failed. Name already exists on this account.');
      return json(201, repoJson(repo, ctx.origin));
    }
    return jsonError(404, 'Not Found');
  }

  // /api/v3/repos/{owner}/{repo}/...
  if (rest.length >= 3 && rest[0] === 'repos') {
    const owner = rest[1] as string;
    const repoName = rest[2] as string;
    const tail = rest.slice(3);

    // Repo itself: GET / PATCH / DELETE /repos/{owner}/{repo}
    if (tail.length === 0) {
      const found = repoOr404(store, owner, repoName);
      if ('status' in found) return found;
      const repo = found;
      if (method === 'GET') return json(200, repoJson(repo, ctx.origin));
      if (method === 'PATCH') {
        const parsed = parseJsonBody(ctx);
        if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
        const body = isRecord(parsed.value) ? parsed.value : {};
        const description = stringField(body, 'description');
        if (description !== undefined) repo.description = description;
        if (typeof body.private === 'boolean') repo.privateField = body.private;
        return json(200, repoJson(repo, ctx.origin));
      }
      if (method === 'DELETE') {
        store.deleteRepo(owner, repoName);
        return empty(204);
      }
      return jsonError(404, 'Not Found');
    }

    const repoFound = repoOr404(store, owner, repoName);
    if ('status' in repoFound) return repoFound;
    const repo = repoFound;

    // git refs: POST /git/refs, GET /git/refs/heads/{branch}
    if (tail[0] === 'git' && tail[1] === 'refs') {
      if (method === 'POST' && tail.length === 2) {
        const parsed = parseJsonBody(ctx);
        if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
        const body = isRecord(parsed.value) ? parsed.value : {};
        const ref = stringField(body, 'ref');
        const sha = stringField(body, 'sha');
        if (!ref || !sha) return jsonError(422, 'ref and sha are required');
        if (!/^refs\/heads\//.test(ref)) return jsonError(422, 'only refs/heads/* refs are supported');
        if (repo.refs.has(ref)) return jsonError(422, 'Reference already exists');
        const object = repo.lookup(sha);
        if (!object || object.type !== 'commit') return jsonError(422, 'sha is not a commit');
        repo.refs.set(ref, sha);
        return json(201, { ref, object: { sha } });
      }
      if (method === 'GET' && tail.length === 4 && tail[2] === 'heads') {
        const branch = tail[3] as string;
        const sha = repo.refs.get(`refs/heads/${branch}`);
        if (sha === undefined) return jsonError(404, 'Not Found');
        return json(200, { ref: `refs/heads/${branch}`, object: { sha } });
      }
      return jsonError(404, 'Not Found');
    }

    // branches: GET /branches, GET /branches/{branch}
    if (tail[0] === 'branches') {
      if (method === 'GET' && tail.length === 1) {
        const branches = [...repo.refs.keys()]
          .filter((ref) => ref.startsWith('refs/heads/'))
          .map((ref) => ({ name: ref.slice('refs/heads/'.length), commit: { sha: repo.refs.get(ref) as string } }));
        return json(200, branches);
      }
      if (method === 'GET' && tail.length === 2) {
        const sha = repo.refs.get(`refs/heads/${tail[1]}`);
        if (sha === undefined) return jsonError(404, 'Not Found');
        return json(200, { name: tail[1], commit: { sha } });
      }
      return jsonError(404, 'Not Found');
    }

    // commits: GET /commits[?sha=<ref>]
    if (tail[0] === 'commits' && tail.length === 1) {
      if (method !== 'GET') return jsonError(404, 'Not Found');
      const ref = ctx.query.get('sha') ?? undefined;
      const tip = ref === undefined ? repo.branchTip() : (repo.refs.get(ref) ?? repo.refs.get(`refs/heads/${ref}`));
      if (tip === undefined || tip === '0000000000000000000000000000000000000000') return json(200, []);
      const commits = store.commitHistoryInRepo(tip, repo).map((sha) => {
        const object = repo.lookup(sha);
        const view = object && object.type === 'commit' ? parseCommit(object.data) : null;
        return {
          sha,
          commit: {
            message: view?.message ?? '',
            author: { name: VIEWER.name, email: VIEWER.email, date: '2025-01-01T00:00:00Z' },
            committer: { name: VIEWER.name, email: VIEWER.email, date: '2025-01-01T00:00:00Z' },
          },
          parents: (view?.parents ?? []).map((parent) => ({ sha: parent })),
        };
      });
      const { page, total, pageNumber, perPage } = paginate(commits, ctx);
      const headers: Record<string, string> = {};
      const links = paginationLinks(ctx, pageNumber, perPage, total);
      if (links) headers.link = links;
      return json(200, page, headers);
    }

    // contents: GET/PUT/DELETE /contents/{path}
    if (tail[0] === 'contents' && tail.length >= 2) {
      const filePath = tail.slice(1).join('/');
      const ref = ctx.query.get('ref') ?? undefined;
      if (method === 'GET') {
        const treeOid = repo.treeAt(ref);
        if (treeOid === undefined) return jsonError(404, 'Not Found');
        const files = flattenTree(treeOid, (oid) => repo.lookup(oid));
        const direct = files.get(filePath);
        if (direct !== undefined) {
          const object = repo.lookup(direct);
          if (!object) return jsonError(404, 'Not Found');
          return json(200, {
            type: 'file',
            encoding: 'base64',
            content: object.data.toString('base64'),
            sha: direct,
            size: object.data.length,
            name: filePath.split('/').pop() ?? '',
            path: filePath,
          });
        }
        // Directory listing (or 404 when nothing lives under the path).
        const prefix = `${filePath}/`;
        const children = new Map<string, { type: 'file' | 'dir'; sha: string; size?: number }>();
        for (const [path, oid] of files) {
          if (!path.startsWith(prefix)) continue;
          const rest2 = path.slice(prefix.length);
          const first = rest2.split('/')[0] as string;
          if (rest2.includes('/')) {
            if (!children.has(first)) {
              const tree = repo.treeAt(ref);
              let dirSha = '';
              if (tree) {
                const walk = (treeOid2: string, segments: string[]): string => {
                  const object2 = repo.lookup(treeOid2);
                  if (!object2 || object2.type !== 'tree') return '';
                  for (const entry of parseTree(object2.data)) {
                    if (entry.name === segments[0]) {
                      return segments.length === 1 ? entry.oid : walk(entry.oid, segments.slice(1));
                    }
                  }
                  return '';
                };
                dirSha = walk(tree, filePath.split('/').concat(first));
              }
              children.set(first, { type: 'dir', sha: dirSha });
            }
          } else {
            const object = repo.lookup(oid);
            children.set(first, { type: 'file', sha: oid, size: object?.data.length ?? 0 });
          }
        }
        if (children.size === 0) return jsonError(404, 'Not Found');
        return json(200, [...children.entries()].map(([name, info]) => ({
          type: info.type,
          name,
          path: `${filePath}/${name}`,
          sha: info.sha,
          ...(info.size !== undefined ? { size: info.size } : {}),
        })));
      }
      if (method === 'PUT') {
        const parsed = parseJsonBody(ctx);
        if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
        const body = isRecord(parsed.value) ? parsed.value : {};
        const contentB64 = stringField(body, 'content');
        if (contentB64 === undefined) return jsonError(422, 'content is required');
        const message = stringField(body, 'message') ?? `Update ${filePath}`;
        const branch = stringField(body, 'branch');
        const sha = stringField(body, 'sha');
        const content = Buffer.from(contentB64.replace(/\s/g, ''), 'base64');
        const existed = repo.treeAt(branch) !== undefined && flattenTree(repo.treeAt(branch) as string, (oid) => repo.lookup(oid)).has(filePath);
        const result = store.commitFileChecked(repo, filePath, content, message, branch, sha);
        if ('error' in result) {
          if (result.error === 'stale-sha') return jsonError(409, 'sha does not match the current file');
          return jsonError(422, 'branch does not exist');
        }
        return json(existed ? 200 : 201, {
          content: { name: filePath.split('/').pop() ?? '', path: filePath, sha: result.contentSha, size: content.length, type: 'file', encoding: 'base64' },
          commit: { sha: result.commitSha, message },
        });
      }
      if (method === 'DELETE') {
        const parsed = parseJsonBody(ctx);
        if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
        const body = isRecord(parsed.value) ? parsed.value : {};
        const message = stringField(body, 'message') ?? `Delete ${filePath}`;
        const branch = stringField(body, 'branch');
        const sha = stringField(body, 'sha');
        const result = store.deleteFile(repo, filePath, message, branch, sha);
        if ('error' in result) {
          if (result.error === 'stale-sha') return jsonError(409, 'sha does not match the current file');
          return jsonError(404, 'Not Found');
        }
        return json(200, { commit: { sha: result.commitSha, message } });
      }
      return jsonError(404, 'Not Found');
    }

    // labels: POST /labels
    if (tail[0] === 'labels' && tail.length === 1) {
      if (method !== 'POST') return jsonError(404, 'Not Found');
      const parsed = parseJsonBody(ctx);
      if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
      const body = isRecord(parsed.value) ? parsed.value : {};
      const name = stringField(body, 'name');
      if (!name) return jsonError(422, 'name is required');
      const color = stringField(body, 'color') ?? 'ededed';
      const label: Label = { name, color };
      repo.labels.set(name, label);
      return json(201, { name, color });
    }

    // issues: POST /issues, GET /issues, GET/PATCH /issues/{n}, comments
    if (tail[0] === 'issues') {
      if (tail.length === 1) {
        if (method === 'POST') {
          const parsed = parseJsonBody(ctx);
          if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
          const body = isRecord(parsed.value) ? parsed.value : {};
          const title = stringField(body, 'title');
          if (!title) return jsonError(422, 'title is required');
          const bodyText = stringField(body, 'body') ?? '';
          const rawLabels = body.labels;
          const labelNames: string[] = Array.isArray(rawLabels)
            ? rawLabels.map((item) => (typeof item === 'string' ? item : isRecord(item) ? stringField(item, 'name') ?? '' : '')).filter(Boolean)
            : [];
          const labels: Label[] = [];
          for (const labelName of labelNames) {
            const label = repo.labels.get(labelName);
            if (!label) return jsonError(422, `Label "${labelName}" does not exist`);
            labels.push(label);
          }
          const issue = store.createIssue(repo, title, bodyText, labels);
          return json(201, issueJson(repo, issue, ctx.origin));
        }
        if (method === 'GET') {
          const state = ctx.query.get('state') ?? 'open';
          const sort = ctx.query.get('sort') ?? 'created';
          const direction = ctx.query.get('direction') ?? 'desc';
          let issues = [...repo.issues.values()];
          if (state !== 'all') issues = issues.filter((issue) => issue.state === state);
          issues.sort((a, b) => {
            const bySeq = sort === 'created' ? a.seq - b.seq : a.seq - b.seq;
            return direction === 'asc' ? bySeq : -bySeq;
          });
          const { page, total, pageNumber, perPage } = paginate(issues, ctx);
          const headers: Record<string, string> = {};
          const links = paginationLinks(ctx, pageNumber, perPage, total);
          if (links) headers.link = links;
          return json(200, page.map((issue) => issueJson(repo, issue, ctx.origin)), headers);
        }
        return jsonError(404, 'Not Found');
      }
      const issueNumber = Number(tail[1]);
      if (!Number.isInteger(issueNumber)) return jsonError(404, 'Not Found');
      const issue = repo.issues.get(issueNumber);
      if (tail.length === 2) {
        if (!issue) return jsonError(404, 'Not Found');
        if (method === 'GET') return jsonString(200, issueJsonString(repo, issue, ctx.origin));
        if (method === 'PATCH') {
          const parsed = parseJsonBody(ctx);
          if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
          const body = isRecord(parsed.value) ? parsed.value : {};
          const state = stringField(body, 'state');
          if (state === 'closed' || state === 'open') {
            issue.state = state;
            issue.updatedAt = new Date().toISOString();
          }
          const title = stringField(body, 'title');
          if (title !== undefined) issue.title = title;
          const bodyText = stringField(body, 'body');
          if (bodyText !== undefined) issue.body = bodyText;
          // The serialized body changed: drop the cached string.
          issue.jsonCache = undefined;
          return json(200, issueJson(repo, issue, ctx.origin));
        }
        return jsonError(404, 'Not Found');
      }
      if (tail.length === 3 && tail[2] === 'comments') {
        if (!issue) return jsonError(404, 'Not Found');
        if (method === 'GET') {
          const list = repo.comments.get(issueNumber) ?? [];
          return json(200, list.map((comment) => ({
            id: comment.id,
            body: comment.body,
            created_at: comment.createdAt,
            user: { login: VIEWER.login, id: 1000, type: 'User' },
          })));
        }
        if (method === 'POST') {
          const parsed = parseJsonBody(ctx);
          if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
          const body = isRecord(parsed.value) ? parsed.value : {};
          const commentBody = stringField(body, 'body');
          if (!commentBody) return jsonError(422, 'body is required');
          const comment = store.createComment(repo, issueNumber, commentBody);
          if (!comment) return jsonError(404, 'Not Found');
          return json(201, { id: comment.id, body: comment.body, created_at: comment.createdAt, user: { login: VIEWER.login, id: 1000, type: 'User' } });
        }
        return jsonError(404, 'Not Found');
      }
      return jsonError(404, 'Not Found');
    }

    // pulls: POST /pulls, GET /pulls, GET /pulls/{n}, GET /pulls/{n}/files, PUT /pulls/{n}/merge
    if (tail[0] === 'pulls') {
      if (tail.length === 1) {
        if (method === 'POST') {
          const parsed = parseJsonBody(ctx);
          if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
          const body = isRecord(parsed.value) ? parsed.value : {};
          const title = stringField(body, 'title');
          const head = stringField(body, 'head');
          const base = stringField(body, 'base');
          if (!title || !head || !base) return jsonError(422, 'title, head and base are required');
          const bodyText = stringField(body, 'body') ?? '';
          const pull = store.createPull(repo, title, bodyText, head, base);
          if ('error' in pull) return jsonError(422, pull.error);
          return json(201, pullJson(repo, pull, ctx.origin));
        }
        if (method === 'GET') {
          const state = ctx.query.get('state') ?? 'open';
          let pulls = [...repo.pulls.values()];
          if (state === 'open') pulls = pulls.filter((pull) => pull.state === 'open');
          else if (state === 'closed') pulls = pulls.filter((pull) => pull.state === 'closed');
          pulls.sort((a, b) => b.seq - a.seq);
          const { page, total, pageNumber, perPage } = paginate(pulls, ctx);
          const headers: Record<string, string> = {};
          const links = paginationLinks(ctx, pageNumber, perPage, total);
          if (links) headers.link = links;
          return json(200, page.map((pull) => pullJson(repo, pull, ctx.origin)), headers);
        }
        return jsonError(404, 'Not Found');
      }
      const pullNumber = Number(tail[1]);
      if (!Number.isInteger(pullNumber)) return jsonError(404, 'Not Found');
      const pull = repo.pulls.get(pullNumber);
      if (tail.length === 2) {
        if (!pull) return jsonError(404, 'Not Found');
        if (method === 'GET') return json(200, pullJson(repo, pull, ctx.origin));
        return jsonError(404, 'Not Found');
      }
      if (tail.length === 3 && tail[2] === 'files') {
        if (!pull) return jsonError(404, 'Not Found');
        if (method !== 'GET') return jsonError(404, 'Not Found');
        const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
        const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
        if (baseTip === undefined || headTip === undefined) return jsonError(404, 'Not Found');
        // Diff merge-base tree vs head tree.
        const baseOid = mergeBase(headTip, baseTip, (oid: string) => repo.lookup(oid));
        if (baseOid === null) return jsonError(404, 'Not Found');
        const baseObject = repo.lookup(baseOid);
        if (!baseObject || baseObject.type !== 'commit') return jsonError(404, 'Not Found');
        const baseTree = parseCommit(baseObject.data).tree;
        const headTree = repo.treeAt(`refs/heads/${pull.headRef}`);
        if (headTree === undefined) return jsonError(404, 'Not Found');
        return json(200, store.diffFiles(repo, baseTree, headTree));
      }
      if (tail.length === 3 && tail[2] === 'merge') {
        if (!pull) return jsonError(404, 'Not Found');
        if (method !== 'PUT') return jsonError(404, 'Not Found');
        const parsed = parseJsonBody(ctx);
        if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
        const result = store.mergePull(repo, pull);
        if ('error' in result) {
          if (result.error === 'conflicted') return jsonError(405, 'Merge conflict');
          return jsonError(409, 'Pull request is already merged');
        }
        return json(200, { merged: true, sha: result.sha, message: `Pull request #${pull.number} merged` });
      }
      return jsonError(404, 'Not Found');
    }

    return jsonError(404, 'Not Found');
  }

  return jsonError(404, 'Not Found');
}
