// MCP at /mcp: JSON-RPC 2.0 over Streamable HTTP. The modern era (revision
// 2026-07-28) answers server/discover with supportedVersions, capabilities and
// serverInfo (mirrored under result._meta); the legacy era answers initialize
// (echoing the requested revision) plus notifications/initialized (202, empty).
// Tools act on the same store as REST, GraphQL and git.

package main

import (
	"sort"
)

const serverName = "makefun-forge"
const serverVersion = "1.0.0"

var supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2024-11-05"}

type toolDef struct {
	name        string
	description string
	properties  map[string]map[string]string
	required    []string
}

// toolsJSON renders one tool definition as a JSON object. The properties are
// built as an ordered structure (encoding/json sorts map keys, and the suite
// compares the tools list byte-for-byte across two calls, which holds because
// the input is constant — but the TS order is declaration order, so we build
// the object with a small ordered writer instead of a map).
func (t toolDef) toJSON() map[string]any {
	props := map[string]any{}
	for name, prop := range t.properties {
		props[name] = map[string]any{"type": prop["type"], "description": prop["description"]}
	}
	return map[string]any{
		"name":        t.name,
		"description": t.description,
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": props,
			"required":   t.required,
		},
	}
}

func prop(typ, desc string) map[string]string {
	return map[string]string{"type": typ, "description": desc}
}

// TOOLS is one constant array, serialized in a fixed order: two tools/list
// calls answer the same list byte for byte.
var toolsList = []toolDef{
	{
		name:        "create_repo",
		description: "Create a repository owned by the authenticated user.",
		properties: map[string]map[string]string{
			"name":        prop("string", "The repository name."),
			"description": prop("string", "A short description of the repository."),
			"private":     prop("boolean", "Whether the repository is private."),
		},
		required: []string{"name"},
	},
	{
		name:        "get_repo",
		description: "Read one repository by owner and name.",
		properties: map[string]map[string]string{
			"owner": prop("string", "The repository owner login."),
			"repo":  prop("string", "The repository name."),
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		properties: map[string]map[string]string{
			"owner": prop("string", "The repository owner login."),
			"repo":  prop("string", "The repository name."),
			"title": prop("string", "The issue title."),
			"body":  prop("string", "The issue body."),
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name:        "list_issues",
		description: "List the issues of a repository, optionally filtered by state.",
		properties: map[string]map[string]string{
			"owner": prop("string", "The repository owner login."),
			"repo":  prop("string", "The repository name."),
			"state": prop("string", "open, closed or all."),
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_pull_request",
		description: "Open a pull request from a head branch onto a base branch.",
		properties: map[string]map[string]string{
			"owner": prop("string", "The repository owner login."),
			"repo":  prop("string", "The repository name."),
			"title": prop("string", "The pull request title."),
			"head":  prop("string", "The head branch name."),
			"base":  prop("string", "The base branch name."),
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name:        "merge_pull_request",
		description: "Squash-merge a pull request onto its base branch.",
		properties: map[string]map[string]string{
			"owner":       prop("string", "The repository owner login."),
			"repo":        prop("string", "The repository name."),
			"pull_number": prop("integer", "The pull request number."),
		},
		required: []string{"owner", "repo", "pull_number"},
	},
}

func toolsListJSON() []map[string]any {
	out := make([]map[string]any, 0, len(toolsList))
	for _, t := range toolsList {
		out = append(out, t.toJSON())
	}
	return out
}

func rpcResult(id any, result any) Resp {
	if id == nil {
		id = nil
	}
	return jsonResp(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result}, nil)
}

func rpcError(id any, code int, message string, status int) Resp {
	if status == 0 {
		status = 200
	}
	return jsonResp(status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}, nil)
}

func toolResult(data map[string]any) map[string]any {
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(marshalJSON(data))}},
		"structuredContent": data,
	}
}

func toolError(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(marshalJSON(map[string]any{"error": message}))}},
		"isError": true,
	}
}

func stringArg(args map[string]any, name string) string {
	if s, ok := args[name].(string); ok {
		return s
	}
	return ""
}

// callTool runs one tool. It returns (result, rpcErr): rpcErr is non-nil only
// for an unknown tool, which the TypeScript answers as a JSON-RPC error
// (-32602); every other failure is a toolError result (isError: true, RPC 200).
func callTool(store *Store, origin, name string, args map[string]any) (map[string]any, *rpcErr) {
	var tool *toolDef
	for i := range toolsList {
		if toolsList[i].name == name {
			tool = &toolsList[i]
			break
		}
	}
	if tool == nil {
		return nil, &rpcErr{code: -32602, message: "Unknown tool: " + name}
	}
	for _, required := range tool.required {
		v, ok := args[required]
		if !ok || v == nil {
			return toolError("Missing required argument: " + required), nil
		}
		if s, isStr := v.(string); isStr && s == "" {
			return toolError("Missing required argument: " + required), nil
		}
	}

	if name == "create_repo" {
		repoName := stringArg(args, "name")
		description := stringArg(args, "description")
		isPrivate := false
		if b, ok := args["private"].(bool); ok {
			isPrivate = b
		}
		repo := store.CreateRepo(repoName, isPrivate, description)
		if repo == nil {
			return toolError("Repository " + repoName + " already exists"), nil
		}
		return toolResult(repoJSON(repo, origin)), nil
	}

	owner := stringArg(args, "owner")
	repoName := stringArg(args, "repo")
	repo := store.GetRepo(owner, repoName)
	if repo == nil || repo.Owner != VIEWER.Login {
		return toolError("Repository " + owner + "/" + repoName + " not found"), nil
	}

	switch name {
	case "get_repo":
		return toolResult(repoJSON(repo, origin)), nil
	case "create_issue":
		title := stringArg(args, "title")
		body := stringArg(args, "body")
		issue := store.CreateIssue(repo, title, body, nil)
		return toolResult(map[string]any{
			"id":         issue.ID,
			"number":     issue.Number,
			"title":      issue.Title,
			"body":       issue.Body,
			"state":      issue.State,
			"labels":     []any{},
			"created_at": issue.CreatedAt,
			"updated_at": issue.UpdatedAt,
			"url":        origin + "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
		}), nil
	case "list_issues":
		state := stringArg(args, "state")
		if state == "" {
			state = "open"
		}
		var issues []*Issue
		for _, issue := range repo.Issues {
			if state != "all" && issue.State != state {
				continue
			}
			issues = append(issues, issue)
		}
		sort.Slice(issues, func(i, j int) bool { return issues[i].Seq > issues[j].Seq })
		list := make([]map[string]any, 0, len(issues))
		for _, issue := range issues {
			labels := make([]map[string]any, 0, len(issue.Labels))
			for _, l := range issue.Labels {
				labels = append(labels, map[string]any{"name": l.Name, "color": l.Color})
			}
			list = append(list, map[string]any{
				"id":         issue.ID,
				"number":     issue.Number,
				"title":      issue.Title,
				"body":       issue.Body,
				"state":      issue.State,
				"labels":     labels,
				"created_at": issue.CreatedAt,
				"updated_at": issue.UpdatedAt,
			})
		}
		return toolResult(map[string]any{"issues": list}), nil
	case "create_pull_request":
		title := stringArg(args, "title")
		head := stringArg(args, "head")
		base := stringArg(args, "base")
		result := store.CreatePull(repo, title, "", head, base)
		if result.Error != "" {
			return toolError(result.Error), nil
		}
		pull := result.Pull
		mergedAt := any(nil)
		if pull.MergedAt != "" {
			mergedAt = pull.MergedAt
		}
		mergeSHA := any(nil)
		if pull.MergeCommitSHA != "" {
			mergeSHA = pull.MergeCommitSHA
		}
		return toolResult(map[string]any{
			"id":               pull.ID,
			"number":           pull.Number,
			"title":            pull.Title,
			"body":             pull.Body,
			"state":            pull.State,
			"head":             map[string]any{"ref": pull.HeadRef, "sha": pull.HeadSHA},
			"base":             map[string]any{"ref": pull.BaseRef, "sha": pull.BaseSHA},
			"merged":           pull.Merged,
			"merged_at":        mergedAt,
			"merge_commit_sha": mergeSHA,
		}), nil
	case "merge_pull_request":
		pullNumber := 0
		switch v := args["pull_number"].(type) {
		case float64:
			pullNumber = int(v)
		case int:
			pullNumber = v
		}
		pull := repo.Pulls[pullNumber]
		if pull == nil {
			return toolError("Pull request #" + itoa(pullNumber) + " not found"), nil
		}
		result := store.MergePull(repo, pull)
		if result.Error != "" {
			if result.Error == "conflicted" {
				return toolError("Merge conflict"), nil
			}
			return toolError("Pull request is already merged"), nil
		}
		return toolResult(map[string]any{
			"number":           pull.Number,
			"merged":           true,
			"state":            pull.State,
			"sha":              result.SHA,
			"merge_commit_sha": result.SHA,
		}), nil
	}
	return toolError("Unknown tool: " + name), nil
}

type rpcErr struct {
	code    int
	message string
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func handleMCP(ctx *Ctx, store *Store, token string) *Resp {
	if ctx.Path != "/mcp" {
		return nil
	}
	if token != "" && bearerToken(ctx) != token {
		r := jsonErrorMsg(401, "Bad credentials")
		return &r
	}
	if ctx.Method == "DELETE" {
		// Session close: any answer is fine.
		r := Resp{Status: 204, Headers: map[string]string{}}
		return &r
	}
	if ctx.Method != "POST" {
		r := jsonErrorMsg(405, "Method not allowed")
		return &r
	}
	parsed, ok := parseJSONBody(ctx)
	if !ok {
		r := rpcError(nil, -32700, "Parse error", 400)
		return &r
	}
	body, isRecordBody := parsed.(map[string]any)
	var method string
	if isRecordBody {
		if m, ok := body["method"].(string); ok {
			method = m
		}
	}
	if !isRecordBody || method == "" {
		var id any
		if isRecordBody {
			id = body["id"]
		}
		r := rpcError(id, -32600, "Invalid Request", 400)
		return &r
	}
	// Notifications carry no id and get an empty 202. (An explicit null id is
	// a request, not a notification: the key is present.)
	_, hasID := body["id"]
	if !hasID {
		r := Resp{Status: 202, Headers: map[string]string{}}
		return &r
	}
	id := body["id"]

	switch method {
	case "server/discover":
		r := rpcResult(id, map[string]any{
			"supportedVersions": supportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": serverName, "version": serverVersion},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": serverName, "version": serverVersion},
			},
		})
		return &r
	case "initialize":
		params, _ := body["params"].(map[string]any)
		requested := supportedVersions[0]
		if params != nil {
			if v, ok := params["protocolVersion"].(string); ok {
				requested = v
			}
		}
		r := rpcResult(id, map[string]any{
			"protocolVersion": requested,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		})
		return &r
	case "tools/list":
		r := rpcResult(id, map[string]any{"tools": toolsListJSON()})
		return &r
	case "tools/call":
		params, _ := body["params"].(map[string]any)
		toolName := ""
		args := map[string]any{}
		if params != nil {
			if n, ok := params["name"].(string); ok {
				toolName = n
			}
			if a, ok := params["arguments"].(map[string]any); ok {
				args = a
			}
		}
		outcome, err := callTool(store, ctx.Origin, toolName, args)
		if err != nil {
			r := rpcError(id, err.code, err.message, 200)
			return &r
		}
		r := rpcResult(id, outcome)
		return &r
	case "ping":
		r := rpcResult(id, map[string]any{})
		return &r
	}
	r := rpcError(id, -32601, "Method not found: "+method, 200)
	return &r
}
