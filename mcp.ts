// MCP at /mcp: JSON-RPC 2.0 over Streamable HTTP. The modern era (revision
// 2026-07-28) answers server/discover with supportedVersions, capabilities and
// serverInfo (mirrored under result._meta); the legacy era answers initialize
// (echoing the requested revision) plus notifications/initialized (202, empty).
// Tools act on the same store as REST, GraphQL and git.

import { VIEWER } from './scenario.ts';
import { Store } from './store.ts';
import { repoJson } from './rest.ts';
import { bearerToken, isRecord, json, jsonError, parseJsonBody, type Ctx, type Resp } from './http.ts';

const SERVER_NAME = 'makefun-forge';
const SERVER_VERSION = '1.0.0';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2024-11-05'];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description: string }>;
    required: string[];
  };
}

// One constant array, serialized in a fixed order: two tools/list calls answer
// the same list byte for byte.
const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Read one repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'open, closed or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch onto a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request onto its base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function rpcResult(id: unknown, result: unknown): Resp {
  return json(200, { jsonrpc: '2.0', id: id ?? null, result });
}

function rpcError(id: unknown, code: number, message: string, status = 200): Resp {
  return json(status, { jsonrpc: '2.0', id: id ?? null, error: { code, message } });
}

function toolResult(data: Record<string, unknown>): Record<string, unknown> {
  return { content: [{ type: 'text', text: JSON.stringify(data) }], structuredContent: data };
}

function toolError(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: JSON.stringify({ error: message }) }], isError: true };
}

function stringArg(args: Record<string, unknown>, name: string): string | undefined {
  const value = args[name];
  return typeof value === 'string' ? value : undefined;
}

function callTool(store: Store, origin: string, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const tool = TOOLS.find((entry) => entry.name === name);
  if (!tool) return { error: { code: -32602, message: `Unknown tool: ${name}` } };
  for (const required of tool.inputSchema.required) {
    if (args[required] === undefined || args[required] === null || args[required] === '') {
      return toolError(`Missing required argument: ${required}`);
    }
  }

  if (name === 'create_repo') {
    const repoName = stringArg(args, 'name') as string;
    const description = stringArg(args, 'description') ?? '';
    const isPrivate = args.private === true;
    const repo = store.createRepo(repoName, isPrivate, description);
    if (!repo) return toolError(`Repository ${repoName} already exists`);
    return toolResult(repoJson(repo, origin));
  }

  const owner = stringArg(args, 'owner') as string;
  const repoName = stringArg(args, 'repo') as string;
  const repo = store.getRepo(owner, repoName);
  if (!repo || repo.owner !== VIEWER.login) return toolError(`Repository ${owner}/${repoName} not found`);

  if (name === 'get_repo') {
    return toolResult(repoJson(repo, origin));
  }
  if (name === 'create_issue') {
    const title = stringArg(args, 'title') as string;
    const body = stringArg(args, 'body') ?? '';
    const issue = store.createIssue(repo, title, body, []);
    return toolResult({
      id: issue.id,
      number: issue.number,
      title: issue.title,
      body: issue.body,
      state: issue.state,
      labels: [],
      created_at: issue.createdAt,
      updated_at: issue.updatedAt,
      url: `${origin}/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    });
  }
  if (name === 'list_issues') {
    const state = stringArg(args, 'state') ?? 'open';
    let issues = [...repo.issues.values()];
    if (state !== 'all') issues = issues.filter((issue) => issue.state === state);
    issues.sort((a, b) => b.seq - a.seq);
    return toolResult({
      issues: issues.map((issue) => ({
        id: issue.id,
        number: issue.number,
        title: issue.title,
        body: issue.body,
        state: issue.state,
        labels: issue.labels.map((label) => ({ name: label.name, color: label.color })),
        created_at: issue.createdAt,
        updated_at: issue.updatedAt,
      })),
    });
  }
  if (name === 'create_pull_request') {
    const title = stringArg(args, 'title') as string;
    const head = stringArg(args, 'head') as string;
    const base = stringArg(args, 'base') as string;
    const pull = store.createPull(repo, title, '', head, base);
    if ('error' in pull) return toolError(pull.error);
    return toolResult({
      id: pull.id,
      number: pull.number,
      title: pull.title,
      body: pull.body,
      state: pull.state,
      head: { ref: pull.headRef, sha: pull.headSha },
      base: { ref: pull.baseRef, sha: pull.baseSha },
      merged: pull.merged,
      merged_at: pull.mergedAt,
      merge_commit_sha: pull.mergeCommitSha,
    });
  }
  if (name === 'merge_pull_request') {
    const pullNumber = args.pull_number as number;
    const pull = repo.pulls.get(pullNumber);
    if (!pull) return toolError(`Pull request #${pullNumber} not found`);
    const result = store.mergePull(repo, pull);
    if ('error' in result) return toolError(result.error === 'conflicted' ? 'Merge conflict' : 'Pull request is already merged');
    return toolResult({ number: pull.number, merged: true, state: pull.state, sha: result.sha, merge_commit_sha: result.sha });
  }
  return toolError(`Unknown tool: ${name}`);
}

export function handleMcp(ctx: Ctx, store: Store, token: string | undefined): Resp | null {
  if (ctx.path !== '/mcp') return null;

  const got = bearerToken(ctx);
  if (token !== undefined && got !== token) return jsonError(401, 'Bad credentials');

  if (ctx.method === 'DELETE') {
    // Session close: any answer is fine.
    return { status: 204, headers: {} };
  }
  if (ctx.method !== 'POST') return jsonError(405, 'Method not allowed');

  const parsed = parseJsonBody(ctx);
  if (!parsed.ok) {
    return rpcError(null, -32700, 'Parse error', 400);
  }
  const body = parsed.value;
  if (!isRecord(body) || typeof body.method !== 'string') {
    return rpcError(isRecord(body) ? body.id : null, -32600, 'Invalid Request', 400);
  }
  const id = body.id;
  const method = body.method as string;

  // Notifications carry no id and get an empty 202.
  if (id === undefined) {
    if (method.startsWith('notifications/')) return { status: 202, headers: {} };
    return { status: 202, headers: {} };
  }

  if (method === 'server/discover') {
    return rpcResult(id, {
      supportedVersions: SUPPORTED_VERSIONS,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
    });
  }
  if (method === 'initialize') {
    const params = isRecord(body.params) ? body.params : {};
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : SUPPORTED_VERSIONS[0];
    return rpcResult(id, {
      protocolVersion: requested,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    });
  }
  if (method === 'tools/list') {
    return rpcResult(id, { tools: TOOLS });
  }
  if (method === 'tools/call') {
    const params = isRecord(body.params) ? body.params : {};
    const toolName = typeof params.name === 'string' ? params.name : '';
    const args = isRecord(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
    const outcome = callTool(store, ctx.origin, toolName, args);
    if ('error' in outcome && isRecord(outcome.error)) {
      const error = outcome.error as { code: number; message: string };
      return rpcError(id, error.code, error.message);
    }
    return rpcResult(id, outcome);
  }
  if (method === 'ping') {
    return rpcResult(id, {});
  }
  return rpcError(id, -32601, `Method not found: ${method}`);
}
