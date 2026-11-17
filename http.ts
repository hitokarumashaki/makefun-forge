// Shared HTTP plumbing for the four surfaces: request context, JSON responses with
// the request-id header every REST reply must carry, and JSON body parsing.

export interface Ctx {
  method: string;
  /** Decoded pathname (percent-decoding applied per segment by the router). */
  path: string;
  query: URLSearchParams;
  headers: Map<string, string>;
  body: Buffer;
  /** The request's own origin, for same-origin pagination links. */
  origin: string;
}

export interface Resp {
  status: number;
  headers?: Record<string, string>;
  body?: string | Buffer;
}

/** A per-request id: a counter, hex-encoded. The contract only needs a non-empty
 *  X-GitHub-Request-Id; a counter is unique per request, deterministic (good for
 *  replay determinism) and far cheaper than a crypto random draw per response. */
let requestCounter = 0;
export function newRequestId(): string {
  requestCounter = (requestCounter + 1) >>> 0;
  return requestCounter.toString(16).padStart(8, '0');
}

export function json(status: number, data: unknown, extraHeaders: Record<string, string> = {}): Resp {
  return {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8', 'x-github-request-id': newRequestId(), ...extraHeaders },
    body: JSON.stringify(data),
  };
}

export function jsonError(status: number, message: string, extraHeaders: Record<string, string> = {}): Resp {
  return json(status, { message }, extraHeaders);
}

export function text(status: number, body: string, contentType: string, extraHeaders: Record<string, string> = {}): Resp {
  return { status, headers: { 'content-type': contentType, ...extraHeaders }, body };
}

export function empty(status: number, extraHeaders: Record<string, string> = {}): Resp {
  return { status, headers: { 'x-github-request-id': newRequestId(), ...extraHeaders } };
}

/** Parse the body as JSON. Returns null when the body is empty or invalid. */
export function parseJsonBody(ctx: Ctx): { ok: true; value: unknown } | { ok: false } {
  if (ctx.body.length === 0) return { ok: true, value: undefined };
  try {
    return { ok: true, value: JSON.parse(ctx.body.toString('utf8')) };
  } catch {
    return { ok: false };
  }
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function stringField(value: unknown, name: string): string | undefined {
  if (!isRecord(value)) return undefined;
  const field = value[name];
  return typeof field === 'string' ? field : undefined;
}

export function bearerToken(ctx: Ctx): string | undefined {
  const header = ctx.headers.get('authorization');
  if (!header) return undefined;
  const match = /^Bearer\s+(.+)$/.exec(header.trim());
  return match ? (match[1] as string).trim() : undefined;
}

/** Build a Link header for a paginated collection (rel="next" first, then prev). */
export function paginationLinks(ctx: Ctx, page: number, perPage: number, total: number): string | undefined {
  const lastPage = Math.max(1, Math.ceil(total / perPage));
  const links: string[] = [];
  const urlFor = (p: number): string => {
    const url = new URL(ctx.origin + ctx.path);
    for (const [key, value] of ctx.query) if (key !== 'page' && key !== 'per_page') url.searchParams.set(key, value);
    url.searchParams.set('page', String(p));
    url.searchParams.set('per_page', String(perPage));
    return url.toString();
  };
  if (page < lastPage) links.push(`<${urlFor(page + 1)}>; rel="next"`);
  if (page > 1) links.push(`<${urlFor(page - 1)}>; rel="prev"`);
  if (lastPage > 1) {
    links.push(`<${urlFor(1)}>; rel="first"`);
    links.push(`<${urlFor(lastPage)}>; rel="last"`);
  }
  return links.length === 0 ? undefined : links.join(', ');
}

export function paginate<T>(items: readonly T[], ctx: Ctx): { page: T[]; total: number; pageNumber: number; perPage: number } {
  const perPage = Math.max(1, Math.min(100, Number(ctx.query.get('per_page') ?? '30') || 30));
  const pageNumber = Math.max(1, Number(ctx.query.get('page') ?? '1') || 1);
  const total = items.length;
  const start = (pageNumber - 1) * perPage;
  return { page: items.slice(start, start + perPage), total, pageNumber, perPage };
}

export function splitPath(path: string): string[] {
  return path.split('/').filter((segment) => segment.length > 0).map((segment) => decodeURIComponent(segment));
}
