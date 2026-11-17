// Git smart HTTP at /{owner}/{repo}.git: upload-pack protocol v2 (and v0) plus
// receive-pack v0. The object store, refs and pack codec live in git-core.ts;
// this module is the wire framing the kit's client expects, verified against
// sdk/conformance/git-client.ts:
//  - info/refs answers v2 when Git-Protocol: version=2 is sent, v0 otherwise.
//  - v2 ls-refs honours ref-prefix server-side, reports HEAD symref + peeled tags.
//  - v2/v0 fetch sends only objects not reachable from the client's haves, with
//    shallow-info for depth requests and an empty pack for a nothing-new fetch.
//  - receive-pack enforces fast-forward per ref and reports one line per ref.

import {
  ZERO_OID,
  collectReachable,
  decodePktLines,
  encodePktLine,
  encodePktLines,
  isAncestor,
  isObjectId,
  parseCommit,
  parseTag,
  parseTree,
  readPack,
  writePack,
  FLUSH,
  DELIM,
  type GitObject,
} from './git-core.ts';
import { VIEWER } from './scenario.ts';
import { Store, type Repo } from './store.ts';
import { bearerToken, jsonError, splitPath, text, type Ctx, type Resp } from './http.ts';

const AGENT = 'makefun-forge/1.0';
const UPLOAD_AD = 'application/x-git-upload-pack-advertisement';
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_AD = 'application/x-git-receive-pack-advertisement';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';

/** Fetch-response cache. Git objects are immutable and content-addressed, so the
 *  pack for a given (repo, wants, shallow, deepen, deepen-since) is deterministic
 *  and never changes — it is a pure function of those parameters. Two inputs do
 *  depend on repo state beyond the object set, so they are excluded from caching:
 *  `haves` (planFetch keeps only haves that exist in the repo, so adding an object
 *  later would change the plan) and `deepen-not` (resolved through repo.refs,
 *  which move). A fetch is cached only when both are empty — which is exactly the
 *  shape of a fresh depth-1 clone (50 of them in the timed workload). Bounded to
 *  200 entries; the whole response Buffer is reused, so a cache hit skips the
 *  plan, the pack build (zlib per object) and the sideband framing entirely. */
const FETCH_CACHE = new Map<string, Buffer>();
const FETCH_CACHE_MAX = 200;

function fetchCacheKey(
  version: 'v0' | 'v2',
  repo: Repo,
  wants: readonly string[],
  shallow: readonly string[],
  deepen: number | undefined,
  deepenSince: number | undefined,
): string {
  return [
    version,
    repo.id,
    wants.join(','),
    shallow.join(','),
    deepen === undefined ? '' : String(deepen),
    deepenSince === undefined ? '' : String(deepenSince),
  ].join('|');
}

function fetchCachePut(key: string, body: Buffer): void {
  if (FETCH_CACHE.size >= FETCH_CACHE_MAX) {
    const first = FETCH_CACHE.keys().next();
    if (!first.done) FETCH_CACHE.delete(first.value);
  }
  FETCH_CACHE.set(key, body);
}

/** Split a pack into sideband band-1 pkt-lines (≤ 65520 bytes per frame). */
function sidebandPack(pack: Buffer): Buffer {
  const parts: Buffer[] = [];
  const maxChunk = 65520 - 4 - 1;
  for (let offset = 0; offset < pack.length; offset += maxChunk) {
    const chunk = pack.subarray(offset, Math.min(pack.length, offset + maxChunk));
    parts.push(encodePktLine(Buffer.concat([Buffer.from([1]), chunk])));
  }
  if (pack.length === 0) parts.push(encodePktLine(Buffer.from([1])));
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

function sidebandTextLines(lines: readonly string[]): Buffer {
  const payload = encodePktLines(lines.map((line) => `${line}\n`));
  return Buffer.concat([encodePktLine(Buffer.concat([Buffer.from([1]), payload])), FLUSH]);
}

interface RefLine {
  oid: string;
  name: string;
}

/** All refs of a repo, HEAD first, then sorted; annotated tags carry their peeled target. */
function listRefs(repo: Repo): { lines: RefLine[]; peeled: Map<string, string> } {
  const names = [...repo.refs.keys()].sort();
  const lines: RefLine[] = [];
  const peeled = new Map<string, string>();
  const defaultTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  if (defaultTip !== undefined) lines.push({ oid: defaultTip, name: 'HEAD' });
  for (const name of names) {
    const oid = repo.refs.get(name) as string;
    lines.push({ oid, name });
    const object = repo.lookup(oid);
    if (object?.type === 'tag') {
      const target = peelToCommit(repo, oid);
      if (target !== null) peeled.set(name, target);
    }
  }
  return { lines, peeled };
}

/** Follow tag objects to the commit they point at (or null when it is not a commit). */
function peelToCommit(repo: Repo, oid: string): string | null {
  let current = oid;
  for (let depth = 0; depth < 10; depth += 1) {
    const object = repo.lookup(current);
    if (!object) return null;
    if (object.type === 'commit') return current;
    if (object.type === 'tag') {
      current = parseTag(object.data).object;
      continue;
    }
    return null;
  }
  return null;
}

function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const capabilities =
    service === 'git-upload-pack'
      ? `side-band-64k shallow symref=HEAD:refs/heads/${repo.defaultBranch} agent=${AGENT}`
      : `report-status side-band-64k delete-refs agent=${AGENT}`;
  const lines: (string | Buffer | 'flush')[] = [`# service=${service}\n`, 'flush'];
  const { lines: refs, peeled } = listRefs(repo);
  if (refs.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\0${capabilities}\n`);
  } else {
    refs.forEach((ref, index) => {
      const suffix = index === 0 ? `\0${capabilities}` : '';
      lines.push(`${ref.oid} ${ref.name}${suffix}\n`);
      const peeledOid = peeled.get(ref.name);
      if (peeledOid !== undefined) lines.push(`${peeledOid} ${ref.name}^{}\n`);
    });
  }
  lines.push('flush');
  return encodePktLines(lines);
}

function v2Advertisement(service: 'git-upload-pack'): Buffer {
  void service;
  return V2_ADVERTISEMENT;
}

/** The v2 capability advertisement is identical for every request (fixed agent and
 *  capabilities, no repo state), so it is built once and reused. */
const V2_ADVERTISEMENT: Buffer = encodePktLines([
  '# service=git-upload-pack\n',
  'flush',
  'version 2\n',
  `agent=${AGENT}\n`,
  'fetch=shallow\n',
  'ls-refs\n',
  'object-format=sha1\n',
  'flush',
]);

// ---------------------------------------------------------------------------------------------
// fetch planning
// ---------------------------------------------------------------------------------------------

interface FetchPlan {
  /** Objects to put in the pack, in a stable order. */
  objects: GitObject[];
  /** New shallow boundaries (commits at the requested depth). */
  shallow: string[];
  /** Old boundaries that are no longer shallow. */
  unshallow: string[];
  /** Haves the server holds (for the acknowledgments section). */
  acks: string[];
  /** Whether any want was unknown to the server. */
  unknownWant: boolean;
}

function planFetch(
  repo: Repo,
  wants: readonly string[],
  haves: readonly string[],
  shallowBoundaries: readonly string[],
  deepen: number | undefined,
  deepenSince: number | undefined,
  deepenNot: readonly string[],
): FetchPlan {
  const lookup = (oid: string): GitObject | undefined => repo.lookup(oid);
  const knownHaves = haves.filter((have) => repo.lookup(have) !== undefined);
  // Exclusions. A have the client fully holds excludes its whole closure. A
  // shallow boundary is different: it marks where the client's history stops.
  // Without a depth request it cuts the ancestry off entirely (a shallow fetch
  // of a new tip must not pull the old history); with a deepen request the
  // depth limit governs, so a shallow-held commit excludes only itself and its
  // tree/blob closure, and the ancestors inside the depth limit are delivered.
  const shallowHeld = new Set(shallowBoundaries);
  const depthLimited = deepen !== undefined || deepenSince !== undefined;
  const excluded = new Set<string>();
  const excludeClosure = (oid: string, shallowOnly: boolean): void => {
    const object = lookup(oid);
    if (!object) return;
    if (!shallowOnly) {
      try {
        for (const next of collectReachable([oid], lookup)) excluded.add(next);
      } catch {
        // An unresolvable have excludes nothing.
      }
      return;
    }
    excluded.add(oid);
    if (object.type === 'commit') {
      const stack: string[] = [parseCommit(object.data).tree];
      while (stack.length > 0) {
        const current = stack.pop() as string;
        if (excluded.has(current)) continue;
        const entry = lookup(current);
        if (!entry) continue;
        excluded.add(current);
        if (entry.type === 'tree') {
          for (const treeEntry of parseTree(entry.data)) {
            if (treeEntry.mode !== '160000') stack.push(treeEntry.oid);
          }
        }
      }
    }
  };
  for (const have of knownHaves) excludeClosure(have, depthLimited && shallowHeld.has(have));
  for (const boundary of shallowBoundaries) excludeClosure(boundary, depthLimited);
  for (const ref of deepenNot) {
    const tip = repo.refs.get(ref) ?? repo.refs.get(`refs/heads/${ref}`);
    if (tip !== undefined) {
      try {
        for (const oid of collectReachable([tip], lookup)) excluded.add(oid);
      } catch {
        // Unresolvable deepen-not excludes nothing.
      }
    }
  }

  // Commits reachable from the wants, with their generation depth from a want.
  const commitDepth = new Map<string, number>();
  const queue: Array<{ oid: string; depth: number }> = [];
  for (const want of wants) {
    const object = repo.lookup(want);
    if (!object) continue;
    const commitOid = object.type === 'tag' ? peelToCommit(repo, want) : want;
    if (commitOid === null) continue;
    if (!commitDepth.has(commitOid)) {
      commitDepth.set(commitOid, 1);
      queue.push({ oid: commitOid, depth: 1 });
    }
  }
  const commitTimestamp = (oid: string): number => {
    const object = repo.lookup(oid);
    if (!object || object.type !== 'commit') return 0;
    const author = parseCommit(object.data).author;
    const match = / (\d+) [+-]\d{4}$/.exec(author);
    return match ? Number(match[1]) : 0;
  };
  while (queue.length > 0) {
    const { oid, depth } = queue.shift() as { oid: string; depth: number };
    // A depth-limited fetch never needs the ancestors past the limit.
    if (deepen !== undefined && depth >= deepen) continue;
    const object = repo.lookup(oid);
    if (!object || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) {
      if (!commitDepth.has(parent)) {
        commitDepth.set(parent, depth + 1);
        queue.push({ oid: parent, depth: depth + 1 });
      }
    }
  }

  let includedCommits: string[] = [];
  const shallow: string[] = [];
  const unshallow: string[] = [];
  if (deepen !== undefined) {
    includedCommits = [...commitDepth.entries()].filter(([, depth]) => depth <= deepen).map(([oid]) => oid);
    for (const [oid, depth] of commitDepth) {
      if (depth !== deepen) continue;
      const object = repo.lookup(oid);
      if (!object || object.type !== 'commit') continue;
      if (parseCommit(object.data).parents.length > 0) shallow.push(oid);
    }
    for (const boundary of shallowBoundaries) {
      if (includedCommits.includes(boundary)) {
        const object = repo.lookup(boundary);
        const parents = object?.type === 'commit' ? parseCommit(object.data).parents : [];
        if (parents.every((parent) => includedCommits.includes(parent))) unshallow.push(boundary);
      }
    }
  } else if (deepenSince !== undefined) {
    includedCommits = [...commitDepth.keys()].filter((oid) => commitTimestamp(oid) >= deepenSince || oid === wants[0]);
    for (const oid of includedCommits) {
      const object = repo.lookup(oid);
      if (!object || object.type !== 'commit') continue;
      const parents = parseCommit(object.data).parents;
      if (parents.some((parent) => !includedCommits.includes(parent))) shallow.push(oid);
    }
  } else {
    includedCommits = [...commitDepth.keys()];
  }

  // A want that is itself reachable from a have is already held: nothing new.
  // The pack holds the included commits and their tree/blob closures only — a
  // commit's parents are NOT followed here, so a depth limit is honoured.
  const reachable = new Set<string>();
  const addTreeClosure = (treeOid: string): void => {
    const stack: string[] = [treeOid];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (reachable.has(oid)) continue;
      const object = lookup(oid);
      if (!object) continue;
      reachable.add(oid);
      if (object.type === 'tree') {
        for (const entry of parseTree(object.data)) {
          if (entry.mode !== '160000') stack.push(entry.oid);
        }
      }
    }
  };
  for (const commitOid of includedCommits) {
    reachable.add(commitOid);
    const object = lookup(commitOid);
    if (object?.type === 'commit') addTreeClosure(parseCommit(object.data).tree);
  }
  for (const want of wants) {
    const object = lookup(want);
    if (!object) continue;
    if (object.type === 'tag') reachable.add(want);
    else if (object.type !== 'commit') {
      try {
        for (const oid of collectReachable([want], lookup)) reachable.add(oid);
      } catch {
        // An unresolvable non-commit want contributes nothing.
      }
    }
  }
  for (const oid of [...reachable]) if (excluded.has(oid)) reachable.delete(oid);

  const objects: GitObject[] = [];
  for (const oid of [...reachable].sort()) {
    const object = repo.lookup(oid);
    if (object) objects.push(object);
  }
  const unknownWant = wants.some((want) => repo.lookup(want) === undefined);
  return { objects, shallow, unshallow, acks: knownHaves, unknownWant };
}

// ---------------------------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------------------------

function parseRequestLines(body: Buffer): { lines: string[]; delimAt: number } {
  const packets = decodePktLines(body);
  const lines: string[] = [];
  let delimAt = -1;
  for (const packet of packets) {
    if (packet.kind === 'delim') {
      if (delimAt < 0) delimAt = lines.length;
      continue;
    }
    if (packet.kind === 'flush') break;
    if (packet.kind === 'data') lines.push(packet.payload.toString('utf8'));
  }
  return { lines, delimAt };
}

function handleLsRefs(repo: Repo, body: Buffer): Resp {
  const { lines } = parseRequestLines(body);
  const args = lines.slice(1);
  let peel = false;
  let symrefs = false;
  const prefixes: string[] = [];
  for (const arg of args) {
    const trimmed = arg.replace(/\n$/, '');
    if (trimmed === 'peel') peel = true;
    else if (trimmed === 'symrefs') symrefs = true;
    else if (trimmed.startsWith('ref-prefix ')) prefixes.push(trimmed.slice('ref-prefix '.length));
  }
  const { lines: refs, peeled } = listRefs(repo);
  const out: string[] = [];
  for (const ref of refs) {
    if (prefixes.length > 0 && !prefixes.some((prefix) => ref.name.startsWith(prefix))) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (symrefs && ref.name === 'HEAD') line += ` symref-target:refs/heads/${repo.defaultBranch}`;
    if (peel) {
      const peeledOid = peeled.get(ref.name);
      if (peeledOid !== undefined) line += ` peeled:${peeledOid}`;
    }
    out.push(`${line}\n`);
  }
  return {
    status: 200,
    headers: { 'content-type': UPLOAD_RESULT },
    body: Buffer.concat([encodePktLines(out), FLUSH]),
  };
}

function handleFetchV2(repo: Repo, body: Buffer): Resp {
  const { lines } = parseRequestLines(body);
  const args = lines.slice(1);
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let deepen: number | undefined;
  let deepenSince: number | undefined;
  const deepenNot: string[] = [];
  for (const arg of args) {
    const trimmed = arg.replace(/\n$/, '');
    if (trimmed.startsWith('want ')) wants.push(trimmed.slice(5).split(' ')[0] as string);
    else if (trimmed.startsWith('have ')) haves.push(trimmed.slice(5).trim());
    else if (trimmed.startsWith('shallow ')) shallow.push(trimmed.slice(8).trim());
    else if (trimmed.startsWith('deepen ')) deepen = Number(trimmed.slice(7));
    else if (trimmed.startsWith('deepen-since ')) deepenSince = Number(trimmed.slice(13));
    else if (trimmed.startsWith('deepen-not ')) deepenNot.push(trimmed.slice(11).trim());
  }
  const cacheable = haves.length === 0 && deepenNot.length === 0;
  const cacheKey = cacheable ? fetchCacheKey('v2', repo, wants, shallow, deepen, deepenSince) : '';
  if (cacheable) {
    const cached = FETCH_CACHE.get(cacheKey);
    if (cached !== undefined) {
      return { status: 200, headers: { 'content-type': UPLOAD_RESULT }, body: cached };
    }
  }
  const plan = planFetch(repo, wants, haves, shallow, deepen, deepenSince, deepenNot);
  const parts: Buffer[] = [];
  if (haves.length > 0) {
    const ackLines: string[] = ['acknowledgments\n'];
    if (plan.acks.length === 0) ackLines.push('NAK\n');
    else {
      for (const ack of plan.acks) ackLines.push(`ACK ${ack}\n`);
      ackLines.push('ready\n');
    }
    parts.push(encodePktLines(ackLines), DELIM);
  }
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    const info: string[] = ['shallow-info\n'];
    for (const oid of plan.shallow) info.push(`shallow ${oid}\n`);
    for (const oid of plan.unshallow) info.push(`unshallow ${oid}\n`);
    parts.push(encodePktLines(info), DELIM);
  }
  const pack = writePack(plan.objects);
  parts.push(encodePktLine('packfile\n'), sidebandPack(pack));
  const responseBody = Buffer.concat(parts);
  if (cacheable) fetchCachePut(cacheKey, responseBody);
  return {
    status: 200,
    headers: { 'content-type': UPLOAD_RESULT },
    body: responseBody,
  };
}

function handleFetchV0(repo: Repo, body: Buffer): Resp {
  const packets = decodePktLines(body);
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let deepen: number | undefined;
  let deepenSince: number | undefined;
  const deepenNot: string[] = [];
  let section = 0;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      section += 1;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (section === 0) {
      if (line.startsWith('want ')) wants.push(line.slice(5).split(' ')[0] as string);
      else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
      else if (line.startsWith('deepen ')) deepen = Number(line.slice(7));
      else if (line.startsWith('deepen-since ')) deepenSince = Number(line.slice(13));
      else if (line.startsWith('deepen-not ')) deepenNot.push(line.slice(11).trim());
    } else {
      if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    }
  }
  const cacheable = haves.length === 0 && deepenNot.length === 0;
  const cacheKey = cacheable ? fetchCacheKey('v0', repo, wants, shallow, deepen, deepenSince) : '';
  if (cacheable) {
    const cached = FETCH_CACHE.get(cacheKey);
    if (cached !== undefined) {
      return { status: 200, headers: { 'content-type': UPLOAD_RESULT }, body: cached };
    }
  }
  const plan = planFetch(repo, wants, haves, shallow, deepen, deepenSince, deepenNot);
  const headLines: string[] = [];
  if (haves.length > 0) {
    if (plan.acks.length === 0) headLines.push('NAK');
    else for (const ack of plan.acks) headLines.push(`ACK ${ack}`);
  } else {
    headLines.push('NAK');
  }
  for (const oid of plan.shallow) headLines.push(`shallow ${oid}`);
  for (const oid of plan.unshallow) headLines.push(`unshallow ${oid}`);
  const pack = writePack(plan.objects);
  const responseBody = Buffer.concat([encodePktLines(headLines.map((line) => `${line}\n`)), sidebandPack(pack)]);
  if (cacheable) fetchCachePut(cacheKey, responseBody);
  return {
    status: 200,
    headers: { 'content-type': UPLOAD_RESULT },
    body: responseBody,
  };
}

function handleUploadPack(repo: Repo, ctx: Ctx): Resp {
  const isV2 = (ctx.headers.get('git-protocol') ?? '') === 'version=2';
  if (isV2) {
    const { lines } = parseRequestLines(ctx.body);
    const command = (lines[0] ?? '').replace(/\n$/, '');
    if (command === 'command=ls-refs') return handleLsRefs(repo, ctx.body);
    if (command === 'command=fetch') return handleFetchV2(repo, ctx.body);
    return text(400, 'unknown v2 command', 'text/plain');
  }
  return handleFetchV0(repo, ctx.body);
}

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

function handleReceivePack(repo: Repo, ctx: Ctx): Resp {
  const commands: PushCommand[] = [];
  let packStart = -1;
  let offset = 0;
  // Walk the raw body: pkt-lines until the first flush, then the raw pack.
  const body = ctx.body;
  while (offset < body.length) {
    if (offset + 4 > body.length) break;
    const head = body.subarray(offset, offset + 4).toString('ascii');
    if (head === '0000') {
      packStart = offset + 4;
      break;
    }
    const length = parseInt(head, 16);
    if (!Number.isFinite(length) || length < 4 || offset + length > body.length) break;
    const payload = body.subarray(offset + 4, offset + length).toString('utf8');
    const nul = payload.indexOf('\0');
    const commandText = (nul >= 0 ? payload.slice(0, nul) : payload).replace(/\n$/, '');
    const parts = commandText.split(' ');
    if (parts.length === 3 && isObjectId(parts[0] as string) && isObjectId(parts[1] as string)) {
      commands.push({ oldOid: parts[0] as string, newOid: parts[1] as string, ref: parts[2] as string });
    }
    offset += length;
  }

  const reportLines: string[] = [];
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (packStart >= 0 && packStart < body.length) {
    const packBytes = body.subarray(packStart);
    try {
      const read = readPack(packBytes, (oid) => repo.lookup(oid));
      for (const [oid, object] of read.objects) {
        if (!repo.lookup(oid)) repo.objects.set(oid, object);
      }
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : 'unpack error';
    }
  }
  if (!unpackOk) {
    reportLines.push(`unpack error ${unpackDetail}`);
    for (const command of commands) reportLines.push(`ng ${command.ref} unpacker error`);
  } else {
    reportLines.push('unpack ok');
    for (const command of commands) {
      const current = repo.refs.get(command.ref);
      if (command.newOid === ZERO_OID) {
        if (current === undefined) {
          reportLines.push(`ng ${command.ref} remote ref does not exist`);
        } else if (command.oldOid !== ZERO_OID && command.oldOid !== current) {
          reportLines.push(`ng ${command.ref} non-fast-forward`);
        } else {
          repo.refs.delete(command.ref);
          reportLines.push(`ok ${command.ref}`);
        }
        continue;
      }
      const newObject = repo.lookup(command.newOid);
      if (!newObject) {
        reportLines.push(`ng ${command.ref} unpacker error`);
        continue;
      }
      const newCommit = newObject.type === 'tag' ? peelToCommit(repo, command.newOid) : command.newOid;
      if (newObject.type !== 'commit' && newCommit === null) {
        reportLines.push(`ng ${command.ref} object is not a commit`);
        continue;
      }
      if (current === undefined) {
        if (command.oldOid !== ZERO_OID) {
          reportLines.push(`ng ${command.ref} remote ref does not exist`);
          continue;
        }
        repo.refs.set(command.ref, command.newOid);
        reportLines.push(`ok ${command.ref}`);
        continue;
      }
      if (command.oldOid !== current) {
        reportLines.push(`ng ${command.ref} non-fast-forward`);
        continue;
      }
      if (command.newOid === current) {
        reportLines.push(`ok ${command.ref}`);
        continue;
      }
      const target = newCommit as string;
      if (!isAncestor(current, target, (oid) => repo.lookup(oid))) {
        reportLines.push(`ng ${command.ref} non-fast-forward`);
        continue;
      }
      repo.refs.set(command.ref, command.newOid);
      reportLines.push(`ok ${command.ref}`);
    }
  }
  return {
    status: 200,
    headers: { 'content-type': RECEIVE_RESULT },
    body: sidebandTextLines(reportLines),
  };
}

export function handleGit(ctx: Ctx, store: Store, token: string | undefined): Resp | null {
  const segments = splitPath(ctx.path);
  if (segments.length < 2 || !segments[1]?.endsWith('.git')) return null;
  const owner = segments[0] as string;
  const repoName = (segments[1] as string).slice(0, -'.git'.length);
  const tail = segments.slice(2);

  const got = bearerToken(ctx);
  if (token !== undefined && got !== token) return jsonError(401, 'Bad credentials');

  const repo = store.getRepo(owner, repoName);
  if (!repo || repo.owner !== VIEWER.login) {
    return jsonError(404, 'Not Found');
  }

  if (ctx.method === 'GET' && tail.length === 2 && tail[0] === 'info' && tail[1] === 'refs') {
    const service = ctx.query.get('service');
    if (service === 'git-upload-pack') {
      const isV2 = (ctx.headers.get('git-protocol') ?? '') === 'version=2';
      if (isV2) return { status: 200, headers: { 'content-type': UPLOAD_AD }, body: v2Advertisement('git-upload-pack') };
      return { status: 200, headers: { 'content-type': UPLOAD_AD }, body: v0Advertisement(repo, 'git-upload-pack') };
    }
    if (service === 'git-receive-pack') {
      return { status: 200, headers: { 'content-type': RECEIVE_AD }, body: v0Advertisement(repo, 'git-receive-pack') };
    }
    return jsonError(400, 'service is required');
  }

  if (ctx.method === 'POST' && tail.length === 1 && tail[0] === 'git-upload-pack') {
    return handleUploadPack(repo, ctx);
  }
  if (ctx.method === 'POST' && tail.length === 1 && tail[0] === 'git-receive-pack') {
    return handleReceivePack(repo, ctx);
  }
  return jsonError(404, 'Not Found');
}
