// The server: one HTTP router over the shared store. /healthz is the env's boot
// probe (unauthenticated); /api/v3 is REST, /api/graphql is GraphQL, /mcp is MCP
// and /{owner}/{repo}.git is git smart HTTP. Everything else is a JSON 404.
// The bearer token comes from MAKEFUN_SHOP_TOKEN; when it is unset (local runs
// without an env) the API surfaces accept any token, so local testing passes
// the same token to the runner.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './store.ts';
import { handleRest } from './rest.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { handleGit } from './git-http.ts';
import { jsonError, type Ctx, type Resp } from './http.ts';

const MAX_BODY_BYTES = 64 * 1024 * 1024;

const store = new Store();
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN === '' ? undefined : process.env.MAKEFUN_SHOP_TOKEN;

const EMPTY_QUERY = new URLSearchParams();
const EMPTY_BODY = Buffer.alloc(0);

function pathOf(req: IncomingMessage): { pathname: string; query: URLSearchParams } {
  // Fast path: the request-target is a plain origin-form path, so split on '?'
  // instead of building a URL object per request. The pathname stays
  // percent-encoded, exactly as URL.pathname would leave it; splitPath decodes
  // per segment. Falls back to '/' only when there is no target at all.
  // Requests without a query string share one empty URLSearchParams (read-only).
  const raw = req.url ?? '/';
  const q = raw.indexOf('?');
  if (q < 0) return { pathname: raw, query: EMPTY_QUERY };
  return { pathname: raw.slice(0, q), query: new URLSearchParams(raw.slice(q + 1)) };
}

function readBody(req: IncomingMessage, expectedLength?: number): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    // Fast path: a small body usually arrives with (or just after) the headers
    // and is already buffered in the socket. Drain it synchronously instead of
    // waiting for 'data'/'end' turns on the event loop. Fall back to the stream
    // when the buffered bytes are short of the declared length.
    const chunks: Buffer[] = [];
    let total = 0;
    if (expectedLength !== undefined && expectedLength > 0) {
      for (;;) {
        const chunk = req.read() as Buffer | null;
        if (chunk === null || chunk.length === 0) break;
        total += chunk.length;
        if (total > MAX_BODY_BYTES) {
          reject(new Error('request body too large'));
          req.destroy();
          return;
        }
        chunks.push(chunk);
        if (total >= expectedLength) break;
      }
      if (total >= expectedLength) {
        resolve(chunks.length === 1 ? (chunks[0] as Buffer) : Buffer.concat(chunks, total));
        return;
      }
    }
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        reject(new Error('request body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(chunks.length === 1 ? (chunks[0] as Buffer) : Buffer.concat(chunks, total)));
    req.on('error', reject);
  });
}

function send(res: ServerResponse, response: Resp): void {
  // Avoid allocating a Buffer for a string body: byteLength for the header, the string straight to end().
  const body = response.body === undefined ? '' : response.body;
  const length = typeof body === 'string' ? Buffer.byteLength(body, 'utf8') : body.length;
  const headers: Record<string, string> = { 'content-length': String(length), ...(response.headers ?? {}) };
  res.writeHead(response.status, headers);
  res.end(body);
}

async function route(req: IncomingMessage): Promise<Resp> {
  const { pathname, query } = pathOf(req);
  const method = req.method ?? 'GET';
  // Only the headers any handler reads are copied into the context map
  // (authorization, git-protocol, content-length, host). Copying every header
  // into a Map costs an allocation per header on every request.
  const headers = new Map<string, string>();
  const rawHeaders = req.headers;
  const auth = rawHeaders['authorization'];
  if (typeof auth === 'string') headers.set('authorization', auth);
  else if (Array.isArray(auth)) headers.set('authorization', auth.join(', '));
  const gitProtocol = rawHeaders['git-protocol'];
  if (typeof gitProtocol === 'string') headers.set('git-protocol', gitProtocol);
  else if (Array.isArray(gitProtocol)) headers.set('git-protocol', gitProtocol.join(', '));
  const contentLength = rawHeaders['content-length'];
  if (typeof contentLength === 'string') headers.set('content-length', contentLength);
  else if (Array.isArray(contentLength)) headers.set('content-length', contentLength.join(', '));
  const host = rawHeaders['host'];
  if (typeof host === 'string') headers.set('host', host);
  else if (Array.isArray(host)) headers.set('host', host.join(', '));
  const origin = `http://${headers.get('host') ?? '127.0.0.1'}`;
  // Most requests in the timed workloads carry no body (every GET, and the git
  // advertisements). Reading the body stream costs event-loop turns even when it
  // is empty, so skip the stream entirely when there cannot be one: GET/HEAD
  // never carry a body here, and any method with an explicit content-length of 0.
  const noBody = method === 'GET' || method === 'HEAD' || headers.get('content-length') === '0';
  const body = noBody ? EMPTY_BODY : await readBody(req, Number(headers.get('content-length') ?? '0') || undefined);
  const ctx: Ctx = { method, path: pathname, query, headers, body, origin };

  // The env's boot check: 200 {"ok":true}, unauthenticated.
  if (method === 'GET' && pathname === '/healthz') {
    return { status: 200, headers: { 'content-type': 'application/json; charset=utf-8' }, body: JSON.stringify({ ok: true }) };
  }

  try {
    if (pathname === '/api/graphql') {
      const result = handleGraphql(ctx, store, TOKEN);
      if (result) return result;
    }
    if (pathname === '/mcp') {
      const result = handleMcp(ctx, store, TOKEN);
      if (result) return result;
    }
    if (pathname === '/api/v3' || pathname.startsWith('/api/v3/')) {
      const result = handleRest(ctx, store, TOKEN);
      if (result) return result;
    }
    const git = handleGit(ctx, store, TOKEN);
    if (git) return git;
  } catch (err) {
    process.stderr.write(`[app] handler error on ${method} ${pathname}: ${err instanceof Error ? err.stack ?? err.message : String(err)}\n`);
    return jsonError(500, 'Internal Server Error');
  }
  return jsonError(404, 'Not Found');
}

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): Server {
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void route(req)
      .then((response) => send(res, response))
      .catch((err: unknown) => {
        process.stderr.write(`[app] ${err instanceof Error ? err.stack ?? err.message : String(err)}\n`);
        if (!res.headersSent) {
          res.writeHead(500, { 'content-type': 'application/json; charset=utf-8' });
          res.end(JSON.stringify({ message: 'Internal Server Error' }));
        } else {
          res.destroy();
        }
      });
  });
}
