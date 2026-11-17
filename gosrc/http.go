// Shared HTTP plumbing for the four surfaces: request context, JSON responses
// with the request-id header every REST reply must carry, JSON body parsing,
// pagination and path splitting.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
)

// Ctx is the per-request context handed to every surface handler. RawQuery
// is kept verbatim so paginationLinks can rebuild link URLs with the
// caller's original parameter order, exactly as the TypeScript
// URLSearchParams version does.
type Ctx struct {
	Method   string
	Path     string // decoded pathname (percent-decoding applied per segment by the router)
	RawQuery string
	Query    url.Values
	Headers  map[string]string // only the headers handlers read
	Body     []byte
	Origin   string // the request's own origin, for same-origin pagination links
}

// Resp is one handler's answer.
type Resp struct {
	Status  int
	Headers map[string]string
	Body    []byte // nil means empty
}

var requestCounter uint32

// newRequestID returns a per-request id: a counter, hex-encoded, padded to 8
// chars. The contract only needs a non-empty X-GitHub-Request-Id; a counter
// is unique per request, deterministic (good for replay determinism) and far
// cheaper than a crypto random draw per response.
func newRequestID() string {
	n := atomic.AddUint32(&requestCounter, 1)
	return fmt.Sprintf("%08x", n)
}

// marshalJSON renders v as JSON exactly the way the TS JSON.stringify does for
// these shapes: no HTML escaping of <, >, & (Go's default escapes them).
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	// Encoder appends a trailing newline; strip it.
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func jsonResp(status int, data any, extraHeaders map[string]string) Resp {
	h := map[string]string{
		"content-type":        "application/json; charset=utf-8",
		"x-github-request-id": newRequestID(),
	}
	for k, v := range extraHeaders {
		h[k] = v
	}
	return Resp{Status: status, Headers: h, Body: marshalJSON(data)}
}

func jsonError(status int, message string, extraHeaders map[string]string) Resp {
	return jsonResp(status, map[string]any{"message": message}, extraHeaders)
}

// jsonErrorMsg is jsonError without extra headers.
func jsonErrorMsg(status int, message string) Resp {
	return jsonError(status, message, nil)
}

func textResp(status int, body, contentType string, extraHeaders map[string]string) Resp {
	h := map[string]string{"content-type": contentType}
	for k, v := range extraHeaders {
		h[k] = v
	}
	return Resp{Status: status, Headers: h, Body: []byte(body)}
}

func emptyResp(status int, extraHeaders map[string]string) Resp {
	h := map[string]string{"x-github-request-id": newRequestID()}
	for k, v := range extraHeaders {
		h[k] = v
	}
	return Resp{Status: status, Headers: h}
}

// parseJSONBody parses the body as JSON. An empty body parses as ok with a
// nil value (matching the TypeScript, which returns {ok:true, value:undefined}
// for an empty body); only malformed JSON fails.
func parseJSONBody(ctx *Ctx) (any, bool) {
	if len(ctx.Body) == 0 {
		return nil, true
	}
	var v any
	// Unmarshal (not Decoder.Decode) so trailing garbage after the value is
	// rejected, matching JSON.parse.
	if err := json.Unmarshal(ctx.Body, &v); err != nil {
		return nil, false
	}
	return v, true
}

func isRecord(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func stringField(v any, name string) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m[name].(string)
	return s
}

func hasStringField(v any, name string) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := m[name].(string)
	return s, ok
}

func bearerToken(ctx *Ctx) string {
	header := ctx.Headers["authorization"]
	if len(header) < 7 || header[:6] != "Bearer" {
		return ""
	}
	// The scheme is a fixed prefix followed by whitespace (space or tab).
	first := header[6]
	if first != ' ' && first != '\t' {
		return ""
	}
	return strings.TrimSpace(header[7:])
}

// paginationLinks builds a Link header for a paginated collection
// (rel="next" first, then prev, first, last), rebuilding each URL from
// origin + path with the caller's other query parameters in their original
// order, then page, then per_page.
func paginationLinks(ctx *Ctx, page, perPage, total int) string {
	lastPage := int(math.Max(1, math.Ceil(float64(total)/float64(perPage))))
	var links []string
	urlFor := func(p int) string {
		var sb strings.Builder
		sb.WriteString(ctx.Origin)
		sb.WriteString(ctx.Path)
		sb.WriteString("?")
		first := true
		for _, pair := range strings.Split(ctx.RawQuery, "&") {
			if pair == "" {
				continue
			}
			key, value, _ := strings.Cut(pair, "=")
			decodedKey, err := url.QueryUnescape(key)
			if err != nil {
				decodedKey = key
			}
			if decodedKey == "page" || decodedKey == "per_page" {
				continue
			}
			if !first {
				sb.WriteString("&")
			}
			first = false
			sb.WriteString(url.QueryEscape(decodedKey))
			sb.WriteString("=")
			decodedValue, err := url.QueryUnescape(value)
			if err != nil {
				decodedValue = value
			}
			sb.WriteString(url.QueryEscape(decodedValue))
		}
		if !first {
			sb.WriteString("&")
		}
		sb.WriteString("page=")
		sb.WriteString(strconv.Itoa(p))
		sb.WriteString("&per_page=")
		sb.WriteString(strconv.Itoa(perPage))
		return sb.String()
	}
	if page < lastPage {
		links = append(links, "<"+urlFor(page+1)+">; rel=\"next\"")
	}
	if page > 1 {
		links = append(links, "<"+urlFor(page-1)+">; rel=\"prev\"")
	}
	if lastPage > 1 {
		links = append(links, "<"+urlFor(1)+">; rel=\"first\"")
		links = append(links, "<"+urlFor(lastPage)+">; rel=\"last\"")
	}
	if len(links) == 0 {
		return ""
	}
	return strings.Join(links, ", ")
}

// parsePageParam mirrors Number(raw) || fallback: empty, non-numeric, NaN
// and zero all fall back; the caller clamps to its range.
func parsePageParam(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || v == 0 {
		return fallback
	}
	return int(v)
}

// paginate slices items for the requested page. per_page is clamped to
// [1,100] and defaults to 30; page defaults to 1 and is clamped to >= 1.
// The returned page is never nil: an out-of-range page is an empty array,
// matching Array.prototype.slice.
func paginate[T any](items []T, ctx *Ctx) (page []T, total, pageNumber, perPage int) {
	perPage = parsePageParam(ctx.Query.Get("per_page"), 30)
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	pageNumber = parsePageParam(ctx.Query.Get("page"), 1)
	if pageNumber < 1 {
		pageNumber = 1
	}
	total = len(items)
	start := (pageNumber - 1) * perPage
	if start >= total {
		return []T{}, total, pageNumber, perPage
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return items[start:end], total, pageNumber, perPage
}

// splitPath splits a pathname into segments, decoding %XX per segment.
// Segments without a '%' skip decodeURIComponent (the common case).
func splitPath(path string) []string {
	parts := strings.Split(path, "/")
	var out []string
	for _, seg := range parts {
		if seg == "" {
			continue
		}
		if strings.Contains(seg, "%") {
			if decoded, err := url.PathUnescape(seg); err == nil {
				seg = decoded
			}
		}
		out = append(out, seg)
	}
	return out
}
