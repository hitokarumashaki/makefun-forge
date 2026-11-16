// GraphQL at /api/graphql: a small hand-rolled executor for the documents gh
// and the suite send (queries, mutations, __type introspection, node(id:),
// cursor pagination, and two-operation batches). Unauthenticated is exactly 401;
// an unknown field is HTTP 200 with an errors array naming it; an unknown
// repository is data.repository = null with no errors.

import { VIEWER } from './scenario.ts';
import {
  Store,
  decodeGlobalId,
  repoGlobalId,
  issueGlobalId,
  pullGlobalId,
  commentGlobalId,
  type Repo,
  type Issue,
  type PullRequest,
} from './store.ts';
import { bearerToken, json, jsonError, parseJsonBody, isRecord, type Ctx, type Resp } from './http.ts';

// ---------------------------------------------------------------------------------------------
// Parser (documents only: operations, variables, arguments, inline fragments, aliases)
// ---------------------------------------------------------------------------------------------

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: Array<{ name: string; value: ValueNode }> };

interface Selection {
  kind: 'field' | 'fragment';
  /** Response key (alias, or the field name). */
  name: string;
  /** The field name (for a field selection). */
  field?: string;
  args?: Array<{ name: string; value: ValueNode }>;
  selections?: Selection[];
  /** Type condition for an inline fragment. */
  typeCondition?: string;
}

interface Operation {
  type: 'query' | 'mutation';
  name?: string;
  variableNames: string[];
  selections: Selection[];
}

function tokenize(source: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  while (i < source.length) {
    const char = source[i] as string;
    if (/\s/.test(char) || char === ',') {
      i += 1;
      continue;
    }
    if (char === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (char === '.' && source.slice(i, i + 3) === '...') {
      tokens.push('...');
      i += 3;
      continue;
    }
    if ('{}()[]:!=$@&|'.includes(char)) {
      tokens.push(char);
      i += 1;
      continue;
    }
    if (char === '"') {
      let j = i + 1;
      let out = '';
      while (j < source.length && source[j] !== '"') {
        if (source[j] === '\\' && j + 1 < source.length) {
          const escaped = source[j + 1] as string;
          out += escaped === 'n' ? '\n' : escaped === 't' ? '\t' : escaped === 'r' ? '\r' : escaped;
          j += 2;
        } else {
          out += source[j] as string;
          j += 1;
        }
      }
      tokens.push(JSON.stringify(out));
      i = j + 1;
      continue;
    }
    const match = /^[A-Za-z_][A-Za-z0-9_]*|^-?\d+(\.\d+)?/.exec(source.slice(i));
    if (match) {
      tokens.push(match[0]);
      i += match[0].length;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(char)} at ${i}`);
  }
  return tokens;
}

class Parser {
  private pos = 0;
  private readonly tokens: string[];
  constructor(tokens: string[]) {
    this.tokens = tokens;
  }

  peek(): string | undefined {
    return this.tokens[this.pos];
  }

  next(): string {
    const token = this.tokens[this.pos];
    if (token === undefined) throw new Error('unexpected end of document');
    this.pos += 1;
    return token;
  }

  expect(token: string): void {
    const got = this.next();
    if (got !== token) throw new Error(`expected ${JSON.stringify(token)}, got ${JSON.stringify(got)}`);
  }

  parseDocument(): Operation[] {
    const operations: Operation[] = [];
    while (this.pos < this.tokens.length) {
      operations.push(this.parseOperation());
    }
    return operations;
  }

  private parseOperation(): Operation {
    let type: 'query' | 'mutation' = 'query';
    let name: string | undefined;
    const token = this.peek();
    if (token === 'query' || token === 'mutation') {
      type = this.next() as 'query' | 'mutation';
      const next = this.peek();
      if (next !== undefined && next !== '(' && next !== '{') name = this.next();
    }
    const variableNames: string[] = [];
    if (this.peek() === '(') {
      this.next();
      while (this.peek() !== ')') {
        this.expect('$');
        variableNames.push(this.next());
        this.expect(':');
        this.parseType();
        if (this.peek() === '=') {
          this.next();
          this.parseValue();
        }
      }
      this.expect(')');
    }
    const selections = this.parseSelectionSet();
    return { type, name, variableNames, selections };
  }

  private parseType(): void {
    if (this.peek() === '[') {
      this.next();
      this.parseType();
      this.expect(']');
    } else {
      this.next();
    }
    if (this.peek() === '!') this.next();
  }

  private parseSelectionSet(): Selection[] {
    this.expect('{');
    const selections: Selection[] = [];
    while (this.peek() !== '}') {
      selections.push(this.parseSelection());
    }
    this.expect('}');
    return selections;
  }

  private parseSelection(): Selection {
    if (this.peek() === '...') {
      this.next();
      let typeCondition: string | undefined;
      if (this.peek() === 'on') {
        this.next();
        typeCondition = this.next();
      }
      const selections = this.parseSelectionSet();
      return { kind: 'fragment', name: typeCondition ?? '', selections, typeCondition };
    }
    const first = this.next();
    let name = first;
    let field = first;
    if (this.peek() === ':') {
      this.next();
      field = this.next();
      name = first;
    }
    const args: Array<{ name: string; value: ValueNode }> = [];
    if (this.peek() === '(') {
      this.next();
      while (this.peek() !== ')') {
        const argName = this.next();
        this.expect(':');
        args.push({ name: argName, value: this.parseValue() });
      }
      this.expect(')');
    }
    let selections: Selection[] | undefined;
    if (this.peek() === '{') selections = this.parseSelectionSet();
    return { kind: 'field', name, field, args, selections };
  }

  private parseValue(): ValueNode {
    const token = this.next();
    if (token === '$') return { kind: 'variable', name: this.next() };
    if (token === '[') {
      const values: ValueNode[] = [];
      while (this.peek() !== ']') values.push(this.parseValue());
      this.expect(']');
      return { kind: 'list', values };
    }
    if (token === '{') {
      const fields: Array<{ name: string; value: ValueNode }> = [];
      while (this.peek() !== '}') {
        const name = this.next();
        this.expect(':');
        fields.push({ name, value: this.parseValue() });
      }
      this.expect('}');
      return { kind: 'object', fields };
    }
    if (token === 'true') return { kind: 'boolean', value: true };
    if (token === 'false') return { kind: 'boolean', value: false };
    if (token === 'null') return { kind: 'null' };
    if (/^"/.test(token)) return { kind: 'string', value: JSON.parse(token) as string };
    if (/^-?\d+$/.test(token)) return { kind: 'int', value: Number(token) };
    if (/^-?\d+\.\d+$/.test(token)) return { kind: 'float', value: Number(token) };
    return { kind: 'enum', value: token };
  }
}

function resolveValue(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'variable':
      return variables[node.name];
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.values.map((value) => resolveValue(value, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of node.fields) out[field.name] = resolveValue(field.value, variables);
      return out;
    }
  }
}

// ---------------------------------------------------------------------------------------------
// Schema (the fields each type answers; an unknown field is an error, not a crash)
// ---------------------------------------------------------------------------------------------

const SCHEMA_FIELDS: Record<string, readonly string[]> = {
  Query: ['viewer', 'repository', 'node', '__type', '__schema'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  User: ['login', 'id', 'name', 'email'],
  Repository: [
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
    'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'private',
  ],
  Issue: ['id', 'number', 'title', 'state', 'body'],
  IssueConnection: ['nodes', 'pageInfo', 'totalCount'],
  PageInfo: ['hasNextPage', 'endCursor', 'hasPreviousPage', 'startCursor'],
  PullRequest: ['id', 'number', 'title', 'body', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files'],
  PullRequestConnection: ['nodes', 'pageInfo', 'totalCount'],
  PullRequestChangedFile: ['path'],
  Comment: ['id', 'body'],
  CommentEdge: ['node', 'cursor'],
  CreateIssuePayload: ['issue'],
  CloseIssuePayload: ['issue'],
  CreatePullRequestPayload: ['pullRequest'],
  MergePullRequestPayload: ['pullRequest'],
  AddCommentPayload: ['commentEdge'],
  __Type: ['name', 'kind', 'fields', 'enumValues'],
  __Field: ['name'],
  __EnumValue: ['name'],
  Ref: ['name'],
};

const INTROSPECTION: Record<string, { fields: string[]; enumValues: string[] }> = {
  Issue: { fields: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'url'], enumValues: [] },
  PullRequest: {
    fields: ['id', 'number', 'title', 'body', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files'],
    enumValues: [],
  },
  Repository: {
    fields: [
      'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
      'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'private',
    ],
    enumValues: [],
  },
  SearchType: { fields: [], enumValues: ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'CODE'] },
  User: { fields: ['login', 'id', 'name', 'email'], enumValues: [] },
};

// ---------------------------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------------------------

interface ObjectNode {
  __typename: string;
  value: unknown;
}

interface Executor {
  store: Store;
  variables: Record<string, unknown>;
  errors: Array<{ message: string }>;
}

function argValue(selection: Selection, name: string, executor: Executor): unknown {
  const arg = selection.args?.find((entry) => entry.name === name);
  if (!arg) return undefined;
  return resolveValue(arg.value, executor.variables);
}

function issueCursor(issue: Issue): string {
  return Buffer.from(`issue:${issue.seq}`, 'utf8').toString('base64url');
}

function cursorSeq(cursor: string | undefined): number | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const match = /^issue:(\d+)$/.exec(text);
    return match ? Number(match[1]) : null;
  } catch {
    return null;
  }
}

function issueState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullState(pull: PullRequest): string {
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

function executeSelections(executor: Executor, parent: ObjectNode, selections: readonly Selection[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (selection.kind === 'fragment') {
      if (selection.typeCondition !== undefined && selection.typeCondition !== parent.__typename) continue;
      Object.assign(out, executeSelections(executor, parent, selection.selections ?? []));
      continue;
    }
    const field = selection.field as string;
    const known = SCHEMA_FIELDS[parent.__typename] ?? [];
    if (!known.includes(field)) {
      executor.errors.push({ message: `Cannot query field "${field}" on type "${parent.__typename}".` });
      out[selection.name] = null;
      continue;
    }
    out[selection.name] = resolveField(executor, parent, selection);
  }
  return out;
}

function resolveField(executor: Executor, parent: ObjectNode, selection: Selection): unknown {
  const { store } = executor;
  const field = selection.field as string;
  const sub = selection.selections ?? [];
  const value = parent.value;

  switch (parent.__typename) {
    case 'Query': {
      if (field === 'viewer') {
        return executeSelections(executor, { __typename: 'User', value: VIEWER }, sub);
      }
      if (field === 'repository') {
        const owner = argValue(selection, 'owner', executor) as string | undefined;
        const name = argValue(selection, 'name', executor) as string | undefined;
        const repo = owner !== undefined && name !== undefined ? store.getRepo(owner, name) : undefined;
        if (!repo || repo.owner !== VIEWER.login) return null;
        return executeSelections(executor, { __typename: 'Repository', value: repo }, sub);
      }
      if (field === 'node') {
        const id = argValue(selection, 'id', executor) as string | undefined;
        const decoded = id === undefined ? null : decodeGlobalId(id);
        if (!decoded) return null;
        const repo = store.getRepoById(decoded.repoId);
        if (!repo) return null;
        if (decoded.kind === 'repo') return executeSelections(executor, { __typename: 'Repository', value: repo }, sub);
        if (decoded.kind === 'issue') {
          const issue = repo.issues.get(decoded.number ?? -1);
          return issue ? executeSelections(executor, { __typename: 'Issue', value: { repo, issue } }, sub) : null;
        }
        if (decoded.kind === 'pull') {
          const pull = repo.pulls.get(decoded.number ?? -1);
          return pull ? executeSelections(executor, { __typename: 'PullRequest', value: { repo, pull } }, sub) : null;
        }
        return null;
      }
      if (field === '__type') {
        const name = argValue(selection, 'name', executor) as string | undefined;
        const info = name === undefined ? undefined : INTROSPECTION[name];
        if (!info) return null;
        return executeSelections(executor, { __typename: '__Type', value: { name, ...info } }, sub);
      }
      return null;
    }
    case 'User': {
      if (field === 'login') return VIEWER.login;
      if (field === 'id') return 'user:1000';
      if (field === 'name') return VIEWER.name;
      if (field === 'email') return VIEWER.email;
      return null;
    }
    case 'Repository': {
      const repo = value as Repo;
      if (field === 'id') return repoGlobalId(repo);
      if (field === 'databaseId') return repo.id;
      if (field === 'name') return repo.name;
      if (field === 'nameWithOwner') return repo.fullName;
      if (field === 'owner') return executeSelections(executor, { __typename: 'User', value: VIEWER }, sub);
      if (field === 'defaultBranchRef') {
        const tip = repo.branchTip();
        if (tip === undefined) return null;
        return executeSelections(executor, { __typename: 'Ref', value: { name: repo.defaultBranch } }, sub);
      }
      if (field === 'viewerPermission') return 'ADMIN';
      if (field === 'hasIssuesEnabled') return true;
      if (field === 'description') return repo.description;
      if (field === 'private') return repo.isPrivate;
      if (field === 'issue') {
        const number = argValue(selection, 'number', executor) as number | undefined;
        const issue = number === undefined ? undefined : repo.issues.get(number);
        if (!issue) return null;
        return executeSelections(executor, { __typename: 'Issue', value: { repo, issue } }, sub);
      }
      if (field === 'issues') {
        return issuesConnection(executor, repo, selection);
      }
      if (field === 'pullRequests') {
        return pullsConnection(executor, repo, selection);
      }
      if (field === 'pullRequest') {
        const number = argValue(selection, 'number', executor) as number | undefined;
        const pull = number === undefined ? undefined : repo.pulls.get(number);
        if (!pull) return null;
        return executeSelections(executor, { __typename: 'PullRequest', value: { repo, pull } }, sub);
      }
      return null;
    }
    case 'Issue': {
      const { repo, issue } = value as { repo: Repo; issue: Issue };
      if (field === 'id') return issueGlobalId(repo, issue);
      if (field === 'number') return issue.number;
      if (field === 'title') return issue.title;
      if (field === 'state') return issueState(issue);
      if (field === 'body') return issue.body;
      return null;
    }
    case 'PullRequest': {
      const { repo, pull } = value as { repo: Repo; pull: PullRequest };
      if (field === 'id') return pullGlobalId(repo, pull);
      if (field === 'number') return pull.number;
      if (field === 'title') return pull.title;
      if (field === 'body') return pull.body;
      if (field === 'state') return pullState(pull);
      if (field === 'merged') return pull.merged;
      if (field === 'headRefName') return pull.headRef;
      if (field === 'baseRefName') return pull.baseRef;
      if (field === 'mergeable') return store.pullMergeable(repo, pull);
      if (field === 'files') {
        const first = argValue(selection, 'first', executor) as number | undefined;
        const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
        const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
        let paths: string[] = [];
        if (baseTip !== undefined && headTip !== undefined) {
          const baseOid = mergeBase(headTip, baseTip, (oid: string) => repo.lookup(oid));
          if (baseOid !== null) {
            const baseObject = repo.lookup(baseOid);
            const headTree = repo.treeAt(`refs/heads/${pull.headRef}`);
            if (baseObject?.type === 'commit' && headTree !== undefined) {
              paths = store.diffFiles(repo, parseCommit(baseObject.data).tree, headTree).map((file) => file.filename);
            }
          }
        }
        const limited = first === undefined ? paths : paths.slice(0, first);
        const out: Record<string, unknown> = {};
        for (const fieldSelection of sub) {
          if (fieldSelection.kind === 'fragment') continue;
          const connectionField = fieldSelection.field as string;
          if (connectionField === 'nodes') {
            const nodeSelections = fieldSelection.selections ?? [];
            out[fieldSelection.name] = limited.map((path) =>
              executeSelections(executor, { __typename: 'PullRequestChangedFile', value: { path } }, nodeSelections),
            );
          } else if (connectionField === 'pageInfo') {
            out[fieldSelection.name] = executeSelections(
              executor,
              { __typename: 'PageInfo', value: { hasNextPage: false, endCursor: null } },
              fieldSelection.selections ?? [],
            );
          } else if (connectionField === 'totalCount') {
            out[fieldSelection.name] = paths.length;
          } else {
            executor.errors.push({ message: `Cannot query field "${connectionField}" on type "PullRequestChangedFileConnection".` });
            out[fieldSelection.name] = null;
          }
        }
        return out;
      }
      return null;
    }
    case 'PullRequestChangedFile': {
      if (field === 'path') return (value as { path: string }).path;
      return null;
    }
    case 'Comment': {
      const comment = value as { id: string; body: string };
      if (field === 'id') return comment.id;
      if (field === 'body') return comment.body;
      return null;
    }
    case 'CommentEdge': {
      const edge = value as { node: { id: string; body: string }; cursor: string };
      if (field === 'node') return executeSelections(executor, { __typename: 'Comment', value: edge.node }, sub);
      if (field === 'cursor') return edge.cursor;
      return null;
    }
    case 'IssueConnection':
    case 'PullRequestConnection': {
      const connection = value as { nodes: unknown[]; pageInfo: { hasNextPage: boolean; endCursor: string | null }; totalCount: number };
      if (field === 'nodes') return connection.nodes;
      if (field === 'pageInfo') return executeSelections(executor, { __typename: 'PageInfo', value: connection.pageInfo }, sub);
      if (field === 'totalCount') return connection.totalCount;
      return null;
    }
    case 'PageInfo': {
      const info = value as { hasNextPage: boolean; endCursor: string | null; hasPreviousPage?: boolean; startCursor?: string | null };
      if (field === 'hasNextPage') return info.hasNextPage;
      if (field === 'endCursor') return info.endCursor;
      if (field === 'hasPreviousPage') return info.hasPreviousPage ?? false;
      if (field === 'startCursor') return info.startCursor ?? null;
      return null;
    }
    case '__Type': {
      const info = value as { name: string; fields: string[]; enumValues: string[] };
      if (field === 'name') return info.name;
      if (field === 'kind') return info.enumValues.length > 0 ? 'ENUM' : 'OBJECT';
      if (field === 'fields') {
        return info.fields.map((name) => executeSelections(executor, { __typename: '__Field', value: { name } }, sub));
      }
      if (field === 'enumValues') {
        return info.enumValues.map((name) => executeSelections(executor, { __typename: '__EnumValue', value: { name } }, sub));
      }
      return null;
    }
    case '__Field':
    case '__EnumValue': {
      if (field === 'name') return (value as { name: string }).name;
      return null;
    }
    case 'Ref': {
      if (field === 'name') return (value as { name: string }).name;
      return null;
    }
    case 'Mutation': {
      return resolveMutation(executor, selection);
    }
    default: {
      const payload = PAYLOAD_FIELDS[parent.__typename];
      if (payload) return payload(executor, value, selection);
      return null;
    }
  }
}

function issuesConnection(executor: Executor, repo: Repo, selection: Selection): Record<string, unknown> {
  const first = argValue(selection, 'first', executor) as number | undefined;
  const after = argValue(selection, 'after', executor) as string | null | undefined;
  const states = argValue(selection, 'states', executor) as string[] | undefined;
  const orderBy = argValue(selection, 'orderBy', executor) as { field?: string; direction?: string } | undefined;
  const direction = orderBy?.direction ?? 'DESC';
  let issues = [...repo.issues.values()];
  if (states !== undefined) {
    const wanted = new Set(states.map((state) => String(state).toLowerCase()));
    issues = issues.filter((issue) => wanted.has(issue.state));
  }
  issues.sort((a, b) => (direction === 'ASC' ? a.seq - b.seq : b.seq - a.seq));
  const afterSeq = cursorSeq(after ?? undefined);
  if (afterSeq !== null) {
    issues = issues.filter((issue) => (direction === 'ASC' ? issue.seq > afterSeq : issue.seq < afterSeq));
  }
  const totalCount = issues.length;
  const page = first === undefined ? issues : issues.slice(0, first);
  const hasNextPage = first !== undefined && issues.length > first;
  const endCursor = page.length === 0 ? null : issueCursor(page[page.length - 1] as Issue);
  // Resolve the connection fields against the query's sub-selection.
  const sub = selection.selections ?? [];
  const out: Record<string, unknown> = {};
  for (const fieldSelection of sub) {
    if (fieldSelection.kind === 'fragment') continue;
    const field = fieldSelection.field as string;
    if (field === 'nodes') {
      const nodeSelections = fieldSelection.selections ?? [];
      out[fieldSelection.name] = page.map((issue) =>
        executeSelections(executor, { __typename: 'Issue', value: { repo, issue } }, nodeSelections),
      );
    } else if (field === 'pageInfo') {
      out[fieldSelection.name] = executeSelections(
        executor,
        { __typename: 'PageInfo', value: { hasNextPage, endCursor } },
        fieldSelection.selections ?? [],
      );
    } else if (field === 'totalCount') {
      out[fieldSelection.name] = totalCount;
    } else {
      executor.errors.push({ message: `Cannot query field "${field}" on type "IssueConnection".` });
      out[fieldSelection.name] = null;
    }
  }
  return out;
}

function pullsConnection(executor: Executor, repo: Repo, selection: Selection): Record<string, unknown> {
  const first = argValue(selection, 'first', executor) as number | undefined;
  const after = argValue(selection, 'after', executor) as string | null | undefined;
  let pulls = [...repo.pulls.values()];
  pulls.sort((a, b) => b.seq - a.seq);
  if (typeof after === 'string' && after !== '') {
    const afterSeq = cursorSeq(after);
    if (afterSeq !== null) pulls = pulls.filter((pull) => pull.seq < afterSeq);
  }
  const totalCount = pulls.length;
  const page = first === undefined ? pulls : pulls.slice(0, first);
  const hasNextPage = first !== undefined && pulls.length > first;
  const endCursor = page.length === 0 ? null : Buffer.from(`pull:${(page[page.length - 1] as PullRequest).seq}`, 'utf8').toString('base64url');
  const sub = selection.selections ?? [];
  const out: Record<string, unknown> = {};
  for (const fieldSelection of sub) {
    if (fieldSelection.kind === 'fragment') continue;
    const field = fieldSelection.field as string;
    if (field === 'nodes') {
      const nodeSelections = fieldSelection.selections ?? [];
      out[fieldSelection.name] = page.map((pull) =>
        executeSelections(executor, { __typename: 'PullRequest', value: { repo, pull } }, nodeSelections),
      );
    } else if (field === 'pageInfo') {
      out[fieldSelection.name] = executeSelections(
        executor,
        { __typename: 'PageInfo', value: { hasNextPage, endCursor } },
        fieldSelection.selections ?? [],
      );
    } else if (field === 'totalCount') {
      out[fieldSelection.name] = totalCount;
    } else {
      executor.errors.push({ message: `Cannot query field "${field}" on type "PullRequestConnection".` });
      out[fieldSelection.name] = null;
    }
  }
  return out;
}

function resolveMutation(executor: Executor, selection: Selection): unknown {
  const { store } = executor;
  const field = selection.field as string;
  const input = argValue(selection, 'input', executor) as Record<string, unknown> | undefined;
  const sub = selection.selections ?? [];

  if (field === 'createIssue') {
    const repositoryId = input?.repositoryId as string | undefined;
    const title = input?.title as string | undefined;
    const decoded = repositoryId === undefined ? null : decodeGlobalId(repositoryId);
    const repo = decoded ? store.getRepoById(decoded.repoId) : undefined;
    if (!repo || !title) {
      executor.errors.push({ message: 'createIssue: the repository does not exist.' });
      return null;
    }
    const issue = store.createIssue(repo, title, (input?.body as string | undefined) ?? '', []);
    return executeSelections(executor, { __typename: 'CreateIssuePayload', value: { repo, issue } }, sub);
  }
  if (field === 'closeIssue') {
    const issueId = input?.issueId as string | undefined;
    const decoded = issueId === undefined ? null : decodeGlobalId(issueId);
    const repo = decoded ? store.getRepoById(decoded.repoId) : undefined;
    const issue = repo && decoded?.number !== undefined ? repo.issues.get(decoded.number) : undefined;
    if (!issue || !repo) {
      executor.errors.push({ message: 'closeIssue: the issue does not exist.' });
      return null;
    }
    issue.state = 'closed';
    issue.updatedAt = new Date().toISOString();
    return executeSelections(executor, { __typename: 'CloseIssuePayload', value: { repo, issue } }, sub);
  }
  if (field === 'createPullRequest') {
    const repositoryId = input?.repositoryId as string | undefined;
    const decoded = repositoryId === undefined ? null : decodeGlobalId(repositoryId);
    const repo = decoded ? store.getRepoById(decoded.repoId) : undefined;
    const title = input?.title as string | undefined;
    const headRefName = input?.headRefName as string | undefined;
    const baseRefName = input?.baseRefName as string | undefined;
    if (!repo || !title || !headRefName || !baseRefName) {
      executor.errors.push({ message: 'createPullRequest: the repository or branches do not exist.' });
      return null;
    }
    const pull = store.createPull(repo, title, (input?.body as string | undefined) ?? '', headRefName, baseRefName);
    if ('error' in pull) {
      executor.errors.push({ message: `createPullRequest: ${pull.error}` });
      return null;
    }
    return executeSelections(executor, { __typename: 'CreatePullRequestPayload', value: { repo, pull } }, sub);
  }
  if (field === 'mergePullRequest') {
    const pullRequestId = input?.pullRequestId as string | undefined;
    const decoded = pullRequestId === undefined ? null : decodeGlobalId(pullRequestId);
    const repo = decoded ? store.getRepoById(decoded.repoId) : undefined;
    const pull = repo && decoded?.number !== undefined ? repo.pulls.get(decoded.number) : undefined;
    if (!pull || !repo) {
      executor.errors.push({ message: 'mergePullRequest: the pull request does not exist.' });
      return null;
    }
    const result = store.mergePull(repo, pull);
    if ('error' in result) {
      executor.errors.push({ message: `mergePullRequest: ${result.error}` });
      return null;
    }
    return executeSelections(executor, { __typename: 'MergePullRequestPayload', value: { repo, pull } }, sub);
  }
  if (field === 'addComment') {
    const subjectId = input?.subjectId as string | undefined;
    const body = input?.body as string | undefined;
    const decoded = subjectId === undefined ? null : decodeGlobalId(subjectId);
    const repo = decoded ? store.getRepoById(decoded.repoId) : undefined;
    const issue = repo && decoded?.number !== undefined ? repo.issues.get(decoded.number) : undefined;
    if (!issue || !repo || body === undefined) {
      executor.errors.push({ message: 'addComment: the subject does not exist.' });
      return null;
    }
    const comment = store.createComment(repo, issue.number, body);
    if (!comment) {
      executor.errors.push({ message: 'addComment: the subject does not exist.' });
      return null;
    }
    const node = { id: commentGlobalId(repo, issue.number, comment), body: comment.body };
    return executeSelections(
      executor,
      { __typename: 'AddCommentPayload', value: { node, cursor: issueCursor(issue) } },
      sub,
    );
  }
  return null;
}

// Mutation payload types resolve their single field by delegating to the object.
const PAYLOAD_FIELDS: Record<string, (executor: Executor, value: unknown, selection: Selection) => unknown> = {
  CreateIssuePayload: (executor, value, selection) => {
    const { repo, issue } = value as { repo: Repo; issue: Issue };
    if (selection.field === 'issue') {
      return executeSelections(executor, { __typename: 'Issue', value: { repo, issue } }, selection.selections ?? []);
    }
    return null;
  },
  CloseIssuePayload: (executor, value, selection) => {
    const { repo, issue } = value as { repo: Repo; issue: Issue };
    if (selection.field === 'issue') {
      return executeSelections(executor, { __typename: 'Issue', value: { repo, issue } }, selection.selections ?? []);
    }
    return null;
  },
  CreatePullRequestPayload: (executor, value, selection) => {
    const { repo, pull } = value as { repo: Repo; pull: PullRequest };
    if (selection.field === 'pullRequest') {
      return executeSelections(executor, { __typename: 'PullRequest', value: { repo, pull } }, selection.selections ?? []);
    }
    return null;
  },
  MergePullRequestPayload: (executor, value, selection) => {
    const { repo, pull } = value as { repo: Repo; pull: PullRequest };
    if (selection.field === 'pullRequest') {
      return executeSelections(executor, { __typename: 'PullRequest', value: { repo, pull } }, selection.selections ?? []);
    }
    return null;
  },
  AddCommentPayload: (executor, value, selection) => {
    const { node, cursor } = value as { node: { id: string; body: string }; cursor: string };
    if (selection.field === 'commentEdge') {
      return executeSelections(executor, { __typename: 'CommentEdge', value: { node, cursor } }, selection.selections ?? []);
    }
    return null;
  },
};

import { mergeBase, parseCommit } from './git-core.ts';

function executeOperation(store: Store, query: string, variables: Record<string, unknown>): Record<string, unknown> {
  const executor: Executor = { store, variables, errors: [] };
  let operations: Operation[];
  try {
    operations = new Parser(tokenize(query)).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'parse error' }] };
  }
  const operation = operations[0];
  if (!operation) return { data: null, errors: [{ message: 'empty document' }] };
  const rootType = operation.type === 'mutation' ? 'Mutation' : 'Query';
  const data = executeSelections(executor, { __typename: rootType, value: null }, operation.selections);
  const result: Record<string, unknown> = { data };
  if (executor.errors.length > 0) result.errors = executor.errors;
  return result;
}

export function handleGraphql(ctx: Ctx, store: Store, token: string | undefined): Resp | null {
  if (ctx.path !== '/api/graphql') return null;
  const got = bearerToken(ctx);
  if (token !== undefined && got !== token) return jsonError(401, 'Bad credentials');
  if (ctx.method !== 'POST') return jsonError(405, 'Method not allowed');
  const parsed = parseJsonBody(ctx);
  if (!parsed.ok) return jsonError(400, 'Problems parsing JSON');
  const body = parsed.value;
  if (Array.isArray(body)) {
    const results = body.map((entry) => {
      if (!isRecord(entry) || typeof entry.query !== 'string') return { data: null, errors: [{ message: 'each operation needs a query string' }] };
      return executeOperation(store, entry.query, (entry.variables ?? {}) as Record<string, unknown>);
    });
    return json(200, results);
  }
  if (!isRecord(body) || typeof body.query !== 'string') {
    return jsonError(400, 'query is required');
  }
  return json(200, executeOperation(store, body.query, (body.variables ?? {}) as Record<string, unknown>));
}
