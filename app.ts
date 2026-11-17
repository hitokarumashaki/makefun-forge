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

function pathOf(req: IncomingMessage): { pathname: string; query: URLSearchParams } {
  try {
    const url = new URL(req.url ?? '/', 'http://entry');
    return { pathname: url.pathname, query: url.searchParams };
  } catch {
    return { pathname: '/', query: new URLSearchParams() };
  }
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        reject(new Error('request body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
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
  const headers = new Map<string, string>();
  for (const [key, value] of Object.entries(req.headers)) {
    if (typeof value === 'string') headers.set(key.toLowerCase(), value);
    else if (Array.isArray(value)) headers.set(key.toLowerCase(), value.join(', '));
  }
  const host = headers.get('host') ?? '127.0.0.1';
  const origin = `http://${host}`;
  // Most requests in the timed workloads carry no body (every GET, and the git
  // advertisements). Reading the body stream costs event-loop turns even when it
  // is empty, so skip the stream entirely when there cannot be one: GET/HEAD
  // never carry a body here, and any method with an explicit content-length of 0.
  const contentLength = headers.get('content-length');
  const noBody = method === 'GET' || method === 'HEAD' || contentLength === '0';
  const body = noBody ? Buffer.alloc(0) : await readBody(req);
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
