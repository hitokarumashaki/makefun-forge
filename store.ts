// The forge's store: one in-memory store shared by REST, GraphQL, MCP and git
// smart HTTP, so an object created on one surface reads back identically on the
// others. Repositories own their git object stores and refs; issues, pull requests,
// labels and comments live beside them.

import {
  ZERO_OID,
  buildCommit,
  buildTreeFromPaths,
  collectReachable,
  commitHistory,
  flattenTree,
  hashObject,
  isAncestor,
  isObjectId,
  mergeBase,
  parseCommit,
  parseTree,
  type GitObject,
  type GitObjectType,
} from './git-core.ts';
import { VIEWER } from './scenario.ts';

export interface Label {
  name: string;
  color: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  seq: number;
  createdAt: string;
  updatedAt: string;
}

export interface Comment {
  id: number;
  body: string;
  createdAt: string;
}

export interface PullRequest {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  seq: number;
  createdAt: string;
}

export class Repo {
  readonly id: number;
  readonly owner: string;
  readonly name: string;
  privateField: boolean;
  description: string;
  readonly defaultBranch = 'main';
  readonly objects = new Map<string, GitObject>();
  readonly refs = new Map<string, string>();
  readonly labels = new Map<string, Label>();
  readonly issues = new Map<number, Issue>();
  readonly pulls = new Map<number, PullRequest>();
  readonly comments = new Map<number, Comment[]>();
  nextIssueNumber = 1;
  nextPullNumber = 1;
  seq = 0;

  constructor(id: number, owner: string, name: string, isPrivate: boolean, description: string) {
    this.id = id;
    this.owner = owner;
    this.name = name;
    this.privateField = isPrivate;
    this.description = description;
  }

  get fullName(): string {
    return `${this.owner}/${this.name}`;
  }

  get isPrivate(): boolean {
    return this.privateField;
  }

  putObject(type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
    return oid;
  }

  lookup(oid: string): GitObject | undefined {
    return this.objects.get(oid);
  }

  refTip(ref: string): string | undefined {
    return this.refs.get(ref);
  }

  /** The tip commit of a branch name, or of the default branch when `branch` is unset. */
  branchTip(branch?: string): string | undefined {
    return this.refs.get(`refs/heads/${branch ?? this.defaultBranch}`);
  }

  headCommit(): string | undefined {
    return this.branchTip();
  }

  /** The tree oid at a ref (branch name or full ref), or undefined when unborn. */
  treeAt(ref?: string): string | undefined {
    const tip = ref === undefined ? this.branchTip() : (this.refs.get(ref) ?? this.refs.get(`refs/heads/${ref}`));
    if (tip === undefined || tip === ZERO_OID) return undefined;
    const object = this.objects.get(tip);
    if (!object || object.type !== 'commit') return undefined;
    return parseCommit(object.data).tree;
  }
}

function isoNow(): string {
  return new Date().toISOString();
}

function b64url(text: string): string {
  return Buffer.from(text, 'utf8').toString('base64url');
}

function unb64url(text: string): string {
  return Buffer.from(text, 'base64url').toString('utf8');
}

/** Opaque global ids (GraphQL): never the plain database id or number. */
export function repoGlobalId(repo: Repo): string {
  return b64url(`repo:${repo.id}`);
}
export function issueGlobalId(repo: Repo, issue: Issue): string {
  return b64url(`issue:${repo.id}:${issue.number}`);
}
export function pullGlobalId(repo: Repo, pull: PullRequest): string {
  return b64url(`pull:${repo.id}:${pull.number}`);
}
export function commentGlobalId(repo: Repo, issueNumber: number, comment: Comment): string {
  return b64url(`comment:${repo.id}:${issueNumber}:${comment.id}`);
}

export interface DecodedGlobalId {
  kind: 'repo' | 'issue' | 'pull' | 'comment';
  repoId: number;
  number?: number;
  commentId?: number;
}

export function decodeGlobalId(id: string): DecodedGlobalId | null {
  try {
    const text = unb64url(id);
    const parts = text.split(':');
    if (parts[0] === 'repo' && parts.length === 2) return { kind: 'repo', repoId: Number(parts[1]) };
    if (parts[0] === 'issue' && parts.length === 3) return { kind: 'issue', repoId: Number(parts[1]), number: Number(parts[2]) };
    if (parts[0] === 'pull' && parts.length === 3) return { kind: 'pull', repoId: Number(parts[1]), number: Number(parts[2]) };
    if (parts[0] === 'comment' && parts.length === 4) {
      return { kind: 'comment', repoId: Number(parts[1]), number: Number(parts[2]), commentId: Number(parts[3]) };
    }
    return null;
  } catch {
    return null;
  }
}

export class Store {
  readonly repos = new Map<string, Repo>();
  readonly reposById = new Map<number, Repo>();
  private nextRepoId = 1;
  private nextIssueId = 1;
  private nextCommentId = 1;
  private nextPullId = 1;

  get viewerLogin(): string {
    return VIEWER.login;
  }

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.repoKey(owner, name));
  }

  getRepoById(id: number): Repo | undefined {
    return this.reposById.get(id);
  }

  listViewerRepos(): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner === VIEWER.login) out.push(repo);
    return out;
  }

  createRepo(name: string, isPrivate: boolean, description: string): Repo | null {
    if (this.repos.has(this.repoKey(VIEWER.login, name))) return null;
    const repo = new Repo(this.nextRepoId++, VIEWER.login, name, isPrivate, description);
    this.repos.set(this.repoKey(VIEWER.login, name), repo);
    this.reposById.set(repo.id, repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    const repo = this.getRepo(owner, name);
    if (!repo) return false;
    this.repos.delete(this.repoKey(owner, name));
    this.reposById.delete(repo.id);
    return true;
  }

  // -------------------------------------------------------------------------------------------
  // contents (file writes through the REST contents API)
  // -------------------------------------------------------------------------------------------

  /**
   * Commit one file on `branch` (creating the branch from the default tip when it
   * does not exist yet, and the repository's first commit when it is unborn).
   * `sha` is the blob oid the caller believes is current; a mismatch is a 409.
   * Returns the commit sha and the blob sha, or an error code.
   */
  commitFile(
    repo: Repo,
    path: string,
    content: Buffer,
    message: string,
    branch?: string,
  ): { commitSha: string; contentSha: string } | { error: 'stale-sha' | 'bad-branch' } {
    return this.commitFileChecked(repo, path, content, message, branch, undefined);
  }

  commitFileChecked(
    repo: Repo,
    path: string,
    content: Buffer,
    message: string,
    branch: string | undefined,
    expectedSha: string | undefined,
  ): { commitSha: string; contentSha: string } | { error: 'stale-sha' | 'bad-branch' } {
    const branchName = branch ?? repo.defaultBranch;
    const refName = `refs/heads/${branchName}`;
    const tip = repo.refs.get(refName);
    const blobOid = repo.putObject('blob', content);
    let parent: string | undefined;
    let baseTree: Map<string, string>;
    if (tip !== undefined && tip !== ZERO_OID) {
      parent = tip;
      const treeOid = repo.treeAt(refName);
      baseTree = treeOid === undefined ? new Map() : flattenTree(treeOid, (oid) => repo.lookup(oid));
    } else if (branchName === repo.defaultBranch) {
      parent = undefined;
      baseTree = new Map();
    } else {
      // A new non-default branch starts from the default branch tip.
      const defaultTip = repo.branchTip();
      if (defaultTip === undefined || defaultTip === ZERO_OID) return { error: 'bad-branch' };
      parent = defaultTip;
      const treeOid = repo.treeAt();
      baseTree = treeOid === undefined ? new Map() : flattenTree(treeOid, (oid) => repo.lookup(oid));
    }
    const current = baseTree.get(path);
    if (expectedSha !== undefined) {
      if (current === undefined || current.toLowerCase() !== expectedSha.toLowerCase()) return { error: 'stale-sha' };
    }
    const files = new Map<string, { mode: string; oid: string }>();
    for (const [filePath, oid] of baseTree) files.set(filePath, { mode: '100644', oid });
    files.set(path, { mode: '100644', oid: blobOid });
    const treeOid = buildTreeFromPaths(files, (type, data) => repo.putObject(type, data));
    const timestamp = Math.floor(Date.now() / 1000);
    const author = `${VIEWER.name} <${VIEWER.email}>`;
    const commitData = buildCommit({
      tree: treeOid,
      parents: parent === undefined ? [] : [parent],
      message,
      author,
      timestamp,
    });
    const commitSha = repo.putObject('commit', commitData);
    repo.refs.set(refName, commitSha);
    return { commitSha, contentSha: blobOid };
  }

  /** Delete one file on the default branch (or `branch`), committing the removal. */
  deleteFile(
    repo: Repo,
    path: string,
    message: string,
    branch?: string,
    expectedSha?: string,
  ): { commitSha: string } | { error: 'not-found' | 'stale-sha' } {
    const branchName = branch ?? repo.defaultBranch;
    const refName = `refs/heads/${branchName}`;
    const tip = repo.refs.get(refName);
    if (tip === undefined || tip === ZERO_OID) return { error: 'not-found' };
    const treeOid = repo.treeAt(refName);
    if (treeOid === undefined) return { error: 'not-found' };
    const baseTree = flattenTree(treeOid, (oid) => repo.lookup(oid));
    const current = baseTree.get(path);
    if (current === undefined) return { error: 'not-found' };
    if (expectedSha !== undefined && current.toLowerCase() !== expectedSha.toLowerCase()) return { error: 'stale-sha' };
    baseTree.delete(path);
    const files = new Map<string, { mode: string; oid: string }>();
    for (const [filePath, oid] of baseTree) files.set(filePath, { mode: '100644', oid });
    const newTreeOid = buildTreeFromPaths(files, (type, data) => repo.putObject(type, data));
    const timestamp = Math.floor(Date.now() / 1000);
    const author = `${VIEWER.name} <${VIEWER.email}>`;
    const commitSha = repo.putObject(
      'commit',
      buildCommit({ tree: newTreeOid, parents: [tip], message, author, timestamp }),
    );
    repo.refs.set(refName, commitSha);
    return { commitSha };
  }

  // -------------------------------------------------------------------------------------------
  // issues and comments
  // -------------------------------------------------------------------------------------------

  createIssue(repo: Repo, title: string, body: string, labels: Label[]): Issue {
    const number = repo.nextIssueNumber++;
    const now = isoNow();
    const issue: Issue = {
      id: this.nextIssueId++,
      number,
      title,
      body: body ?? '',
      state: 'open',
      labels: [...labels],
      seq: ++repo.seq,
      createdAt: now,
      updatedAt: now,
    };
    repo.issues.set(number, issue);
    return issue;
  }

  createComment(repo: Repo, issueNumber: number, body: string): Comment | null {
    if (!repo.issues.has(issueNumber)) return null;
    const comment: Comment = { id: this.nextCommentId++, body, createdAt: isoNow() };
    const list = repo.comments.get(issueNumber) ?? [];
    list.push(comment);
    repo.comments.set(issueNumber, list);
    return comment;
  }

  // -------------------------------------------------------------------------------------------
  // pull requests
  // -------------------------------------------------------------------------------------------

  createPull(repo: Repo, title: string, body: string, head: string, base: string): PullRequest | { error: string } {
    const headTip = repo.refs.get(`refs/heads/${head}`);
    const baseTip = repo.refs.get(`refs/heads/${base}`);
    if (headTip === undefined || baseTip === undefined) return { error: 'unknown branch' };
    for (const existing of repo.pulls.values()) {
      if (existing.headRef === head && existing.state === 'open') return { error: 'a pull request is already open from this branch' };
    }
    const now = isoNow();
    const pull: PullRequest = {
      id: this.nextPullId++,
      number: repo.nextPullNumber++,
      title,
      body: body ?? '',
      state: 'open',
      headRef: head,
      baseRef: base,
      headSha: headTip,
      baseSha: baseTip,
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      seq: ++repo.seq,
      createdAt: now,
    };
    repo.pulls.set(pull.number, pull);
    return pull;
  }

  /** The paths changed between two trees, with added/removed line counts. */
  diffFiles(
    repo: Repo,
    baseTreeOid: string,
    headTreeOid: string,
  ): { filename: string; status: 'added' | 'modified' | 'removed'; additions: number; deletions: number; changes: number }[] {
    const base = flattenTree(baseTreeOid, (oid) => repo.lookup(oid));
    const head = flattenTree(headTreeOid, (oid) => repo.lookup(oid));
    const out: { filename: string; status: 'added' | 'modified' | 'removed'; additions: number; deletions: number; changes: number }[] = [];
    const names = new Set([...base.keys(), ...head.keys()]);
    const countLines = (oid: string | undefined): string[] => {
      if (oid === undefined) return [];
      const object = repo.lookup(oid);
      if (!object) return [];
      const text = object.data.toString('utf8');
      if (text === '') return [];
      return text.split('\n');
    };
    for (const name of [...names].sort()) {
      const baseOid = base.get(name);
      const headOid = head.get(name);
      if (baseOid === headOid) continue;
      if (baseOid === undefined) {
        const lines = countLines(headOid);
        out.push({ filename: name, status: 'added', additions: lines.length, deletions: 0, changes: lines.length });
      } else if (headOid === undefined) {
        const lines = countLines(baseOid);
        out.push({ filename: name, status: 'removed', additions: 0, deletions: lines.length, changes: lines.length });
      } else {
        const oldLines = countLines(baseOid);
        const newLines = countLines(headOid);
        const oldCount = new Map<string, number>();
        for (const line of oldLines) oldCount.set(line, (oldCount.get(line) ?? 0) + 1);
        let additions = 0;
        for (const line of newLines) {
          const remaining = oldCount.get(line) ?? 0;
          if (remaining > 0) oldCount.set(line, remaining - 1);
          else additions += 1;
        }
        let deletions = 0;
        for (const remaining of oldCount.values()) deletions += remaining;
        out.push({ filename: name, status: 'modified', additions, deletions, changes: additions + deletions });
      }
    }
    return out;
  }

  /** Whether merging `pull` would conflict: both sides changed the same path differently vs the merge-base. */
  pullMergeable(repo: Repo, pull: PullRequest): 'MERGEABLE' | 'CONFLICTING' | 'UNKNOWN' {
    const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
    const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
    if (headTip === undefined || baseTip === undefined) return 'UNKNOWN';
    const base = mergeBase(headTip, baseTip, (oid) => repo.lookup(oid));
    if (base === null) return 'UNKNOWN';
    const baseCommit = repo.lookup(base);
    if (!baseCommit || baseCommit.type !== 'commit') return 'UNKNOWN';
    const baseTree = parseCommit(baseCommit.data).tree;
    const headTree = repo.treeAt(`refs/heads/${pull.headRef}`);
    const ontoTree = repo.treeAt(`refs/heads/${pull.baseRef}`);
    if (headTree === undefined || ontoTree === undefined) return 'UNKNOWN';
    const baseFiles = flattenTree(baseTree, (oid) => repo.lookup(oid));
    const headFiles = flattenTree(headTree, (oid) => repo.lookup(oid));
    const ontoFiles = flattenTree(ontoTree, (oid) => repo.lookup(oid));
    const names = new Set([...baseFiles.keys(), ...headFiles.keys(), ...ontoFiles.keys()]);
    for (const name of names) {
      const b = baseFiles.get(name);
      const h = headFiles.get(name);
      const o = ontoFiles.get(name);
      const headChanged = h !== b;
      const ontoChanged = o !== b;
      if (headChanged && ontoChanged && h !== o) return 'CONFLICTING';
    }
    return 'MERGEABLE';
  }

  /**
   * Squash-merge `pull` onto its base branch: one commit whose tree is the merged
   * tree (base tree + head changes), parented on the base tip. Returns the merge
   * commit sha, or an error code.
   */
  mergePull(repo: Repo, pull: PullRequest): { sha: string } | { error: 'conflicted' | 'already-merged' } {
    if (pull.merged || pull.state !== 'open') return { error: 'already-merged' };
    if (this.pullMergeable(repo, pull) === 'CONFLICTING') return { error: 'conflicted' };
    const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
    const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
    if (headTip === undefined || baseTip === undefined) return { error: 'conflicted' };
    const base = mergeBase(headTip, baseTip, (oid) => repo.lookup(oid));
    const baseCommit = base === null ? null : repo.lookup(base);
    const baseTree = baseCommit?.type === 'commit' ? parseCommit(baseCommit.data).tree : null;
    const headTree = repo.treeAt(`refs/heads/${pull.headRef}`);
    const ontoTree = repo.treeAt(`refs/heads/${pull.baseRef}`);
    if (headTree === undefined || ontoTree === undefined || baseTree === null) return { error: 'conflicted' };
    const baseFiles = flattenTree(baseTree, (oid) => repo.lookup(oid));
    const headFiles = flattenTree(headTree, (oid) => repo.lookup(oid));
    const ontoFiles = flattenTree(ontoTree, (oid) => repo.lookup(oid));
    const merged = new Map<string, { mode: string; oid: string }>();
    for (const [path, oid] of ontoFiles) merged.set(path, { mode: '100644', oid });
    for (const [path, oid] of headFiles) {
      if (baseFiles.get(path) !== oid) merged.set(path, { mode: '100644', oid });
    }
    for (const path of baseFiles.keys()) {
      if (!headFiles.has(path) && ontoFiles.has(path) && baseFiles.get(path) !== ontoFiles.get(path)) {
        // Head deleted it, base kept it unchanged vs merge-base: delete.
        if (baseFiles.get(path) === ontoFiles.get(path)) merged.delete(path);
      }
      if (!headFiles.has(path) && baseFiles.get(path) === ontoFiles.get(path)) merged.delete(path);
    }
    const treeOid = buildTreeFromPaths(merged, (type, data) => repo.putObject(type, data));
    const timestamp = Math.floor(Date.now() / 1000);
    const author = `${VIEWER.name} <${VIEWER.email}>`;
    const message = `Merge pull request #${pull.number} from ${pull.headRef}\n\n${pull.title}`;
    const sha = repo.putObject(
      'commit',
      buildCommit({ tree: treeOid, parents: [baseTip], message, author, timestamp }),
    );
    repo.refs.set(`refs/heads/${pull.baseRef}`, sha);
    pull.merged = true;
    pull.state = 'closed';
    pull.mergedAt = isoNow();
    pull.mergeCommitSha = sha;
    return { sha };
  }

  // -------------------------------------------------------------------------------------------
  // git helpers shared by the smart-HTTP surface
  // -------------------------------------------------------------------------------------------

  /** Every object id reachable from a ref tip (for advertisement / fetch exclusion). */
  reachableFrom(tip: string, repo: Repo): Set<string> {
    return collectReachable([tip], (oid) => repo.lookup(oid));
  }

  isAncestorInRepo(ancestor: string, oid: string, repo: Repo): boolean {
    return isAncestor(ancestor, oid, (id) => repo.lookup(id));
  }

  commitHistoryInRepo(tip: string, repo: Repo): string[] {
    return commitHistory(tip, (id) => repo.lookup(id));
  }
}

export function isValidOid(value: unknown): value is string {
  return typeof value === 'string' && isObjectId(value);
}
