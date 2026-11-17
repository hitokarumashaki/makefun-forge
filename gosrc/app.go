// The server: one HTTP router over the shared store. /healthz is the env's boot
// probe (unauthenticated); /api/v3 is REST, /api/graphql is GraphQL, /mcp is MCP
// and /{owner}/{repo}.git is git smart HTTP. Everything else is a JSON 404.
// The bearer token comes from MAKEFUN_SHOP_TOKEN; when it is unset (local runs
// without an env) the API surfaces accept any token, so local testing passes
// the same token to the runner.
//
// Concurrency: Node runs one event loop, so the TypeScript never locks. Go
// serves requests on many goroutines, so the router holds the store lock for
// the duration of a request: a read lock for reads (GET/HEAD, git fetch,
// GraphQL queries) and the write lock for writes (REST POST/PUT/PATCH/DELETE,
// git receive-pack, MCP, GraphQL mutations). The git response caches and the
// issue JSON cache carry their own synchronization, so they are safe under a
// read lock. A handler panic is recovered as a 500, exactly like the
// TypeScript's try/catch around the route.

package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const maxBodyBytes = 64 * 1024 * 1024

// isReadRequest reports whether the request only reads the store. Reads run
// under the read lock (so concurrent clones/fetches overlap); everything else
// takes the write lock.
func isReadRequest(method, path string, body []byte) bool {
	if method == "GET" || method == "HEAD" {
		return true
	}
	if method == "POST" && strings.HasSuffix(path, "/git-upload-pack") {
		return true
	}
	if method == "POST" && path == "/api/graphql" {
		return !graphqlHasMutation(body)
	}
	return false
}

// graphqlHasMutation reports whether a GraphQL request body carries a mutation
// operation. Only the first operation of each document runs, so only a leading
// "mutation" keyword counts; a query that merely mentions the word does not.
func graphqlHasMutation(body []byte) bool {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	var queries []string
	switch v := parsed.(type) {
	case map[string]any:
		if q, ok := v["query"].(string); ok {
			queries = append(queries, q)
		}
	case []any:
		for _, entry := range v {
			if m, ok := entry.(map[string]any); ok {
				if q, ok := m["query"].(string); ok {
					queries = append(queries, q)
				}
			}
		}
	}
	for _, q := range queries {
		if gqlStartsWithMutation(q) {
			return true
		}
	}
	return false
}

func gqlStartsWithMutation(query string) bool {
	i := 0
	for i < len(query) {
		c := query[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(query) && query[i] != '\n' {
				i++
			}
			continue
		}
		break
	}
	rest := query[i:]
	if !strings.HasPrefix(rest, "mutation") {
		return false
	}
	after := rest[len("mutation"):]
	if after == "" {
		return true
	}
	c := after[0]
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '{' || c == '('
}

func route(store *Store, token string, ctx *Ctx) Resp {
	// The env's boot check: 200 {"ok":true}, unauthenticated.
	if ctx.Method == "GET" && ctx.Path == "/healthz" {
		return jsonResp(200, map[string]any{"ok": true}, nil)
	}
	if ctx.Path == "/api/graphql" {
		if result := handleGraphQL(ctx, store, token); result != nil {
			return *result
		}
	}
	if ctx.Path == "/mcp" {
		if result := handleMCP(ctx, store, token); result != nil {
			return *result
		}
	}
	if ctx.Path == "/api/v3" || strings.HasPrefix(ctx.Path, "/api/v3/") {
		if result := handleRest(ctx, store, token); result != nil {
			return *result
		}
	}
	if result := handleGit(ctx, store, token); result != nil {
		return *result
	}
	return jsonErrorMsg(404, "Not Found")
}

// routeLocked runs one request under the store lock and recovers a handler
// panic as a 500, mirroring the TypeScript's try/catch around the route.
func routeLocked(store *Store, token string, ctx *Ctx) (resp Resp) {
	if isReadRequest(ctx.Method, ctx.Path, ctx.Body) {
		store.mu.RLock()
		defer store.mu.RUnlock()
	} else {
		store.mu.Lock()
		defer store.mu.Unlock()
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[app] handler error on %s %s: %v", ctx.Method, ctx.Path, rec)
			resp = jsonErrorMsg(500, "Internal Server Error")
		}
	}()
	return route(store, token, ctx)
}

// makeHandler builds the http.Handler the entry listens with.
func makeHandler(store *Store, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the headers any handler reads are copied into the context
		// (authorization, git-protocol, content-length, host).
		headers := map[string]string{}
		if v := r.Header.Get("Authorization"); v != "" {
			headers["authorization"] = v
		}
		if v := r.Header.Get("Git-Protocol"); v != "" {
			headers["git-protocol"] = v
		}
		if r.ContentLength >= 0 {
			headers["content-length"] = strconv.FormatInt(r.ContentLength, 10)
		}
		host := r.Host
		if host != "" {
			headers["host"] = host
		}
		origin := "http://" + host
		if host == "" {
			origin = "http://127.0.0.1"
		}

		// Most requests in the timed workloads carry no body (every GET, and
		// the git advertisements). Skip reading the stream entirely when
		// there cannot be one: GET/HEAD never carry a body here, and any
		// method with an explicit content-length of 0.
		var body []byte
		if r.Method != "GET" && r.Method != "HEAD" && r.ContentLength != 0 {
			data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
			if err != nil {
				log.Printf("[app] body read error on %s %s: %v", r.Method, r.URL.Path, err)
				writeResp(w, jsonErrorMsg(500, "Internal Server Error"))
				return
			}
			if int64(len(data)) > maxBodyBytes {
				log.Printf("[app] request body too large on %s %s", r.Method, r.URL.Path)
				writeResp(w, jsonErrorMsg(500, "Internal Server Error"))
				return
			}
			body = data
		} else {
			body = []byte{}
		}

		// The pathname stays percent-encoded, exactly as the TypeScript's
		// req.url pathname; splitPath decodes per segment.
		path := r.URL.EscapedPath()
		if path == "" {
			path = "/"
		}
		ctx := &Ctx{
			Method:   r.Method,
			Path:     path,
			RawQuery: r.URL.RawQuery,
			Query:    r.URL.Query(),
			Headers:  headers,
			Body:     body,
			Origin:   origin,
		}
		writeResp(w, routeLocked(store, token, ctx))
	})
}

func writeResp(w http.ResponseWriter, resp Resp) {
	for k, v := range resp.Headers {
		if strings.EqualFold(k, "content-length") {
			continue
		}
		w.Header().Set(k, v)
	}
	if resp.Body != nil {
		w.Header().Set("Content-Length", strconv.Itoa(len(resp.Body)))
		w.WriteHeader(resp.Status)
		_, _ = w.Write(resp.Body)
		return
	}
	w.WriteHeader(resp.Status)
}

// appToken is the bearer token the API surfaces require, from
// MAKEFUN_SHOP_TOKEN. Empty means no authentication (local runs).
func appToken() string {
	return os.Getenv("MAKEFUN_SHOP_TOKEN")
}
