// REST v3 under /api/v3. Every reply is application/json with a non-empty
// X-GitHub-Request-Id; errors carry {message}; unknown routes are 404 JSON;
// malformed JSON bodies are 400. Repositories, issues and pull requests are
// stored in the shared store, so MCP and GraphQL see the same objects.

package main

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func repoJSON(repo *Repo, origin string) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"node_id":        repoGlobalID(repo),
		"name":           repo.Name,
		"full_name":      repo.FullName(),
		"owner":          map[string]any{"login": repo.Owner, "id": 1000, "type": "User"},
		"private":        repo.IsPrivate(),
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"html_url":       origin + "/" + repo.Owner + "/" + repo.Name,
		"url":            origin + "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
		"created_at":     "2025-01-01T00:00:00Z",
		"updated_at":     "2025-01-01T00:00:00Z",
		"has_issues":     true,
		"has_projects":   false,
		"has_wiki":       false,
	}
}

// issueJSONOut is the single-issue body as a struct, not a map: encoding/json
// emits struct fields in declaration order (matching the TypeScript's
// insertion order, so the bytes are identical) without the key-sorting and
// interface dispatch a map marshal pays. Labels is always non-nil so an
// issue with no labels renders [] and never null.
type issueLabelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type issueUserJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type issueJSONOut struct {
	ID          int              `json:"id"`
	NodeID      string           `json:"node_id"`
	Number      int              `json:"number"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	State       string           `json:"state"`
	Labels      []issueLabelJSON `json:"labels"`
	User        issueUserJSON    `json:"user"`
	CreatedAt   string           `json:"created_at"`
	UpdatedAt   string           `json:"updated_at"`
	HTMLURL     string           `json:"html_url"`
	URL         string           `json:"url"`
	CommentsURL string           `json:"comments_url"`
}

func issueJSON(repo *Repo, issue *Issue, origin string) issueJSONOut {
	labels := make([]issueLabelJSON, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, issueLabelJSON{Name: l.Name, Color: l.Color})
	}
	return issueJSONOut{
		ID:          issue.ID,
		NodeID:      issueGlobalID(repo, issue),
		Number:      issue.Number,
		Title:       issue.Title,
		Body:        issue.Body,
		State:       issue.State,
		Labels:      labels,
		User:        issueUserJSON{Login: VIEWER.Login, ID: 1000, Type: "User"},
		CreatedAt:   issue.CreatedAt,
		UpdatedAt:   issue.UpdatedAt,
		HTMLURL:     origin + "/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		URL:         origin + "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		CommentsURL: origin + "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number) + "/comments",
	}
}

// issueJSONString returns the serialized single-issue body, cached per origin.
// The read workload GETs the same issue 2,000 times; rebuilding and
// re-stringifying it every time is pure waste. The cache is cleared whenever
// the issue mutates.
func issueJSONString(repo *Repo, issue *Issue, origin string) string {
	if issue.JSONCache == nil {
		issue.JSONCache = new(sync.Map)
	}
	if v, ok := issue.JSONCache.Load(origin); ok {
		return v.(string)
	}
	body := string(marshalJSON(issueJSON(repo, issue, origin)))
	issue.JSONCache.Store(origin, body)
	return body
}

// jsonStringResp sends a pre-serialized JSON body (the cached issue body)
// with the standard headers.
func jsonStringResp(status int, body string) Resp {
	return Resp{
		Status: status,
		Headers: map[string]string{
			"content-type":        "application/json; charset=utf-8",
			"x-github-request-id": newRequestID(),
		},
		Body: []byte(body),
	}
}

func pullJSON(repo *Repo, pull *PullRequest, origin string) map[string]any {
	mergedAt := any(nil)
	if pull.MergedAt != "" {
		mergedAt = pull.MergedAt
	}
	mergeSHA := any(nil)
	if pull.MergeCommitSHA != "" {
		mergeSHA = pull.MergeCommitSHA
	}
	return map[string]any{
		"id":      pull.ID,
		"node_id": pullGlobalID(repo, pull),
		"number":  pull.Number,
		"title":   pull.Title,
		"body":    pull.Body,
		"state":   pull.State,
		"head": map[string]any{
			"ref":   pull.HeadRef,
			"sha":   pull.HeadSHA,
			"label": repo.Owner + ":" + pull.HeadRef,
		},
		"base": map[string]any{
			"ref":   pull.BaseRef,
			"sha":   pull.BaseSHA,
			"label": repo.Owner + ":" + pull.BaseRef,
		},
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"user":             map[string]any{"login": VIEWER.Login, "id": 1000, "type": "User"},
		"created_at":       pull.CreatedAt,
		"html_url":         origin + "/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
		"url":              origin + "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + strconv.Itoa(pull.Number),
	}
}

func repoOr404(store *Store, owner, name string) (*Repo, *Resp) {
	repo := store.GetRepo(owner, name)
	if repo == nil {
		r := jsonErrorMsg(404, "Not Found")
		return nil, &r
	}
	if repo.Owner != VIEWER.Login {
		r := jsonErrorMsg(404, "Not Found")
		return nil, &r
	}
	return repo, nil
}

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var refHeadsRe = regexp.MustCompile(`^refs/heads/`)

func handleRest(ctx *Ctx, store *Store, token string) *Resp {
	segments := splitPath(ctx.Path)
	if len(segments) < 2 || segments[0] != "api" || segments[1] != "v3" {
		return nil
	}
	if token != "" && bearerToken(ctx) != token {
		r := jsonErrorMsg(401, "Bad credentials")
		return &r
	}
	rest := segments[2:]
	method := ctx.Method

	// GET /api/v3/ — API root.
	if len(rest) == 0 {
		if method != "GET" {
			r := jsonErrorMsg(404, "Not Found")
			return &r
		}
		r := jsonResp(200, map[string]any{
			"current_user_url":      ctx.Origin + "/api/v3/user",
			"repository_url":        ctx.Origin + "/api/v3/repos/{owner}/{repo}",
			"user_repositories_url": ctx.Origin + "/api/v3/user/repos",
			"rate_limit_url":        ctx.Origin + "/api/v3/rate_limit",
		}, map[string]string{"x-oauth-scopes": "repo, user"})
		return &r
	}

	// GET /api/v3/meta
	if len(rest) == 1 && rest[0] == "meta" {
		if method != "GET" {
			r := jsonErrorMsg(404, "Not Found")
			return &r
		}
		r := jsonResp(200, map[string]any{"installed_version": "1.0.0", "verifiable_password_authentication": true, "git": []string{"github.com"}}, nil)
		return &r
	}

	// GET /api/v3/user
	if len(rest) == 1 && rest[0] == "user" {
		if method != "GET" {
			r := jsonErrorMsg(404, "Not Found")
			return &r
		}
		r := jsonResp(200, map[string]any{
			"login":    VIEWER.Login,
			"id":       1000,
			"node_id":  "user:1000",
			"name":     VIEWER.Name,
			"email":    VIEWER.Email,
			"type":     "User",
			"html_url": ctx.Origin + "/" + VIEWER.Login,
			"url":      ctx.Origin + "/api/v3/user",
		}, nil)
		return &r
	}

	// /api/v3/user/repos
	if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
		if method == "GET" {
			repos := store.ListViewerRepos()
			page, total, pageNumber, perPage := paginate(repos, ctx)
			headers := map[string]string{}
			if links := paginationLinks(ctx, pageNumber, perPage, total); links != "" {
				headers["link"] = links
			}
			arr := make([]map[string]any, 0, len(page))
			for _, repo := range page {
				arr = append(arr, repoJSON(repo, ctx.Origin))
			}
			r := jsonResp(200, arr, headers)
			return &r
		}
		if method == "POST" {
			parsed, ok := parseJSONBody(ctx)
			if !ok {
				r := jsonErrorMsg(400, "Problems parsing JSON")
				return &r
			}
			body, _ := parsed.(map[string]any)
			name := stringField(body, "name")
			if name == "" {
				r := jsonErrorMsg(422, "name is required")
				return &r
			}
			if !repoNameRe.MatchString(name) {
				r := jsonErrorMsg(422, "name is invalid")
				return &r
			}
			description := stringField(body, "description")
			isPrivate := false
			if b, ok := body["private"].(bool); ok {
				isPrivate = b
			}
			repo := store.CreateRepo(name, isPrivate, description)
			if repo == nil {
				r := jsonErrorMsg(422, "Repository creation failed. Name already exists on this account.")
				return &r
			}
			r := jsonResp(201, repoJSON(repo, ctx.Origin), nil)
			return &r
		}
		r := jsonErrorMsg(404, "Not Found")
		return &r
	}

	// /api/v3/repos/{owner}/{repo}/...
	if len(rest) >= 3 && rest[0] == "repos" {
		owner := rest[1]
		repoName := rest[2]
		tail := rest[3:]

		// Repo itself: GET / PATCH / DELETE /repos/{owner}/{repo}
		if len(tail) == 0 {
			repo, errResp := repoOr404(store, owner, repoName)
			if errResp != nil {
				return errResp
			}
			if method == "GET" {
				r := jsonResp(200, repoJSON(repo, ctx.Origin), nil)
				return &r
			}
			if method == "PATCH" {
				parsed, ok := parseJSONBody(ctx)
				if !ok {
					r := jsonErrorMsg(400, "Problems parsing JSON")
					return &r
				}
				body, _ := parsed.(map[string]any)
				if desc, ok := hasStringField(body, "description"); ok {
					repo.Description = desc
				}
				if b, ok := body["private"].(bool); ok {
					repo.PrivateField = b
				}
				r := jsonResp(200, repoJSON(repo, ctx.Origin), nil)
				return &r
			}
			if method == "DELETE" {
				store.DeleteRepo(owner, repoName)
				r := emptyResp(204, nil)
				return &r
			}
			r := jsonErrorMsg(404, "Not Found")
			return &r
		}

		repo, errResp := repoOr404(store, owner, repoName)
		if errResp != nil {
			return errResp
		}

		// git refs: POST /git/refs, GET /git/refs/heads/{branch}
		if tail[0] == "git" && len(tail) >= 2 && tail[1] == "refs" {
			if method == "POST" && len(tail) == 2 {
				parsed, ok := parseJSONBody(ctx)
				if !ok {
					r := jsonErrorMsg(400, "Problems parsing JSON")
					return &r
				}
				body, _ := parsed.(map[string]any)
				ref := stringField(body, "ref")
				sha := stringField(body, "sha")
				if ref == "" || sha == "" {
					r := jsonErrorMsg(422, "ref and sha are required")
					return &r
				}
				if !refHeadsRe.MatchString(ref) {
					r := jsonErrorMsg(422, "only refs/heads/* refs are supported")
					return &r
				}
				if repo.Refs.Has(ref) {
					r := jsonErrorMsg(422, "Reference already exists")
					return &r
				}
				object := repo.Lookup(sha)
				if object == nil || object.Type != ObjCommit {
					r := jsonErrorMsg(422, "sha is not a commit")
					return &r
				}
				repo.Refs.Set(ref, sha)
				r := jsonResp(201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha}}, nil)
				return &r
			}
			if method == "GET" && len(tail) == 4 && tail[2] == "heads" {
				branch := tail[3]
				sha, ok := repo.Refs.Get("refs/heads/" + branch)
				if !ok {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				r := jsonResp(200, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": sha}}, nil)
				return &r
			}
			r := jsonErrorMsg(404, "Not Found")
			return &r
		}

		// branches: GET /branches, GET /branches/{branch}
		if tail[0] == "branches" {
			if method == "GET" && len(tail) == 1 {
				var branches []map[string]any
				branches = make([]map[string]any, 0)
				for _, ref := range repo.Refs.Keys() {
					if strings.HasPrefix(ref, "refs/heads/") {
						sha, _ := repo.Refs.Get(ref)
						branches = append(branches, map[string]any{
							"name":   strings.TrimPrefix(ref, "refs/heads/"),
							"commit": map[string]any{"sha": sha},
						})
					}
				}
				r := jsonResp(200, branches, nil)
				return &r
			}
			if method == "GET" && len(tail) == 2 {
				sha, ok := repo.Refs.Get("refs/heads/" + tail[1])
				if !ok {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				r := jsonResp(200, map[string]any{"name": tail[1], "commit": map[string]any{"sha": sha}}, nil)
				return &r
			}
			r := jsonErrorMsg(404, "Not Found")
			return &r
		}

		// commits: GET /commits[?sha=<ref>]
		if tail[0] == "commits" && len(tail) == 1 {
			if method != "GET" {
				r := jsonErrorMsg(404, "Not Found")
				return &r
			}
			ref := ctx.Query.Get("sha")
			var tip string
			var ok bool
			if ref == "" {
				tip, ok = repo.BranchTip("")
			} else {
				tip, ok = repo.Refs.Get(ref)
				if !ok {
					tip, ok = repo.Refs.Get("refs/heads/" + ref)
				}
			}
			if !ok || tip == ZeroOID {
				r := jsonResp(200, make([]any, 0), nil)
				return &r
			}
			commits := store.CommitHistoryInRepo(tip, repo)
			arr := make([]map[string]any, 0, len(commits))
			for _, sha := range commits {
				object := repo.Lookup(sha)
				var view *CommitView
				if object != nil && object.Type == ObjCommit {
					v := parseCommit(object.Data)
					view = &v
				}
				message := ""
				var parents []map[string]any
				parents = make([]map[string]any, 0)
				if view != nil {
					message = view.Message
					for _, p := range view.Parents {
						parents = append(parents, map[string]any{"sha": p})
					}
				}
				arr = append(arr, map[string]any{
					"sha": sha,
					"commit": map[string]any{
						"message":   message,
						"author":    map[string]any{"name": VIEWER.Name, "email": VIEWER.Email, "date": "2025-01-01T00:00:00Z"},
						"committer": map[string]any{"name": VIEWER.Name, "email": VIEWER.Email, "date": "2025-01-01T00:00:00Z"},
					},
					"parents": parents,
				})
			}
			page, total, pageNumber, perPage := paginate(arr, ctx)
			headers := map[string]string{}
			if links := paginationLinks(ctx, pageNumber, perPage, total); links != "" {
				headers["link"] = links
			}
			if page == nil {
				page = make([]map[string]any, 0)
			}
			r := jsonResp(200, page, headers)
			return &r
		}

		// contents: GET/PUT/DELETE /contents/{path}
		if tail[0] == "contents" && len(tail) >= 2 {
			filePath := strings.Join(tail[1:], "/")
			ref := ctx.Query.Get("ref")
			if method == "GET" {
				treeOID := repo.TreeAt(ref)
				if treeOID == "" {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				files := flattenTree(treeOID, func(oid string) *GitObject { return repo.Lookup(oid) }, "")
				if direct, ok := files[filePath]; ok {
					object := repo.Lookup(direct)
					if object == nil {
						r := jsonErrorMsg(404, "Not Found")
						return &r
					}
					name := filePath
					if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
						name = filePath[idx+1:]
					}
					r := jsonResp(200, map[string]any{
						"type":     "file",
						"encoding": "base64",
						"content":  base64.StdEncoding.EncodeToString(object.Data),
						"sha":      direct,
						"size":     len(object.Data),
						"name":     name,
						"path":     filePath,
					}, nil)
					return &r
				}
				// Directory listing (or 404 when nothing lives under the path).
				prefix := filePath + "/"
				children := map[string]map[string]any{}
				for path, oid := range files {
					if !strings.HasPrefix(path, prefix) {
						continue
					}
					rest2 := strings.TrimPrefix(path, prefix)
					first := strings.Split(rest2, "/")[0]
					if strings.Contains(rest2, "/") {
						if _, ok := children[first]; !ok {
							dirSHA := ""
							if tree := repo.TreeAt(ref); tree != "" {
								dirSHA = findDirSHA(repo, tree, append(strings.Split(filePath, "/"), first))
							}
							children[first] = map[string]any{"type": "dir", "sha": dirSHA}
						}
					} else {
						object := repo.Lookup(oid)
						size := 0
						if object != nil {
							size = len(object.Data)
						}
						children[first] = map[string]any{"type": "file", "sha": oid, "size": size}
					}
				}
				if len(children) == 0 {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				names := make([]string, 0, len(children))
				for n := range children {
					names = append(names, n)
				}
				sort.Strings(names)
				arr := make([]map[string]any, 0, len(names))
				for _, name := range names {
					info := children[name]
					entry := map[string]any{"type": info["type"], "name": name, "path": filePath + "/" + name, "sha": info["sha"]}
					if size, ok := info["size"]; ok {
						entry["size"] = size
					}
					arr = append(arr, entry)
				}
				r := jsonResp(200, arr, nil)
				return &r
			}
			if method == "PUT" {
				parsed, ok := parseJSONBody(ctx)
				if !ok {
					r := jsonErrorMsg(400, "Problems parsing JSON")
					return &r
				}
				body, _ := parsed.(map[string]any)
				contentB64, contentOK := hasStringField(body, "content")
				if !contentOK {
					r := jsonErrorMsg(422, "content is required")
					return &r
				}
				message := stringField(body, "message")
				if message == "" {
					message = "Update " + filePath
				}
				branch := stringField(body, "branch")
				sha := stringField(body, "sha")
				cleaned := strings.Map(func(r rune) rune {
					if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
						return -1
					}
					return r
				}, contentB64)
				content, _ := base64.StdEncoding.DecodeString(cleaned)
				existed := false
				if treeOID := repo.TreeAt(branch); treeOID != "" {
					_, existed = flattenTree(treeOID, func(oid string) *GitObject { return repo.Lookup(oid) }, "")[filePath]
				}
				result := store.CommitFileChecked(repo, filePath, content, message, branch, sha)
				if result.Error != "" {
					if result.Error == "stale-sha" {
						r := jsonErrorMsg(409, "sha does not match the current file")
						return &r
					}
					r := jsonErrorMsg(422, "branch does not exist")
					return &r
				}
				name := filePath
				if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
					name = filePath[idx+1:]
				}
				status := 201
				if existed {
					status = 200
				}
				r := jsonResp(status, map[string]any{
					"content": map[string]any{"name": name, "path": filePath, "sha": result.ContentSHA, "size": len(content), "type": "file", "encoding": "base64"},
					"commit":  map[string]any{"sha": result.CommitSHA, "message": message},
				}, nil)
				return &r
			}
			if method == "DELETE" {
				parsed, ok := parseJSONBody(ctx)
				if !ok {
					r := jsonErrorMsg(400, "Problems parsing JSON")
					return &r
				}
				body, _ := parsed.(map[string]any)
				message := stringField(body, "message")
				if message == "" {
					message = "Delete " + filePath
				}
				branch := stringField(body, "branch")
				sha := stringField(body, "sha")
				result := store.DeleteFile(repo, filePath, message, branch, sha)
				if result.Error != "" {
					if result.Error == "stale-sha" {
						r := jsonErrorMsg(409, "sha does not match the current file")
						return &r
					}
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				r := jsonResp(200, map[string]any{"commit": map[string]any{"sha": result.CommitSHA, "message": message}}, nil)
				return &r
			}
			r := jsonErrorMsg(404, "Not Found")
			return &r
		}

		// labels: POST /labels
		if tail[0] == "labels" && len(tail) == 1 {
			if method != "POST" {
				r := jsonErrorMsg(404, "Not Found")
				return &r
			}
			parsed, ok := parseJSONBody(ctx)
			if !ok {
				r := jsonErrorMsg(400, "Problems parsing JSON")
				return &r
			}
			body, _ := parsed.(map[string]any)
			name := stringField(body, "name")
			if name == "" {
				r := jsonErrorMsg(422, "name is required")
				return &r
			}
			color := stringField(body, "color")
			if color == "" {
				color = "ededed"
			}
			label := Label{Name: name, Color: color}
			repo.Labels[name] = label
			r := jsonResp(201, map[string]any{"name": name, "color": color}, nil)
			return &r
		}

		// issues
		if tail[0] == "issues" {
			if len(tail) == 1 {
				if method == "POST" {
					parsed, ok := parseJSONBody(ctx)
					if !ok {
						r := jsonErrorMsg(400, "Problems parsing JSON")
						return &r
					}
					body, _ := parsed.(map[string]any)
					title := stringField(body, "title")
					if title == "" {
						r := jsonErrorMsg(422, "title is required")
						return &r
					}
					bodyText := stringField(body, "body")
					var labelNames []string
					if raw, ok := body["labels"].([]any); ok {
						for _, item := range raw {
							switch v := item.(type) {
							case string:
								if v != "" {
									labelNames = append(labelNames, v)
								}
							case map[string]any:
								if n := stringField(v, "name"); n != "" {
									labelNames = append(labelNames, n)
								}
							}
						}
					}
					var labels []Label
					for _, labelName := range labelNames {
						label, ok := repo.Labels[labelName]
						if !ok {
							r := jsonErrorMsg(422, "Label \""+labelName+"\" does not exist")
							return &r
						}
						labels = append(labels, label)
					}
					issue := store.CreateIssue(repo, title, bodyText, labels)
					r := jsonResp(201, issueJSON(repo, issue, ctx.Origin), nil)
					return &r
				}
				if method == "GET" {
					state := ctx.Query.Get("state")
					if state == "" {
						state = "open"
					}
					sortBy := ctx.Query.Get("sort")
					if sortBy == "" {
						sortBy = "created"
					}
					direction := ctx.Query.Get("direction")
					if direction == "" {
						direction = "desc"
					}
					var issues []*Issue
					for _, issue := range repo.Issues {
						if state != "all" && issue.State != state {
							continue
						}
						issues = append(issues, issue)
					}
					// TS sorts by seq (sort === 'created' ? a.seq - b.seq : a.seq - b.seq)
					// — both branches are seq, so it is always a seq sort.
					sort.Slice(issues, func(i, j int) bool {
						if direction == "asc" {
							return issues[i].Seq < issues[j].Seq
						}
						return issues[i].Seq > issues[j].Seq
					})
					_ = sortBy
					page, total, pageNumber, perPage := paginate(issues, ctx)
					headers := map[string]string{}
					if links := paginationLinks(ctx, pageNumber, perPage, total); links != "" {
						headers["link"] = links
					}
					arr := make([]issueJSONOut, 0, len(page))
					for _, issue := range page {
						arr = append(arr, issueJSON(repo, issue, ctx.Origin))
					}
					r := jsonResp(200, arr, headers)
					return &r
				}
				r := jsonErrorMsg(404, "Not Found")
				return &r
			}
			issueNumber, err := strconv.Atoi(tail[1])
			if err != nil {
				r := jsonErrorMsg(404, "Not Found")
				return &r
			}
			issue := repo.Issues[issueNumber]
			if len(tail) == 2 {
				if issue == nil {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				if method == "GET" {
					r := jsonStringResp(200, issueJSONString(repo, issue, ctx.Origin))
					return &r
				}
				if method == "PATCH" {
					parsed, ok := parseJSONBody(ctx)
					if !ok {
						r := jsonErrorMsg(400, "Problems parsing JSON")
						return &r
					}
					body, _ := parsed.(map[string]any)
					if state, ok := hasStringField(body, "state"); ok && (state == "closed" || state == "open") {
						issue.State = state
						issue.UpdatedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
					}
					if title, ok := hasStringField(body, "title"); ok {
						issue.Title = title
					}
					if bodyText, ok := hasStringField(body, "body"); ok {
						issue.Body = bodyText
					}
					// The serialized body changed: drop the cached string.
					issue.JSONCache = new(sync.Map)
					r := jsonResp(200, issueJSON(repo, issue, ctx.Origin), nil)
					return &r
				}
				r := jsonErrorMsg(404, "Not Found")
				return &r
			}
			if len(tail) == 3 && tail[2] == "comments" {
				if issue == nil {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				if method == "GET" {
					list := repo.Comments[issueNumber]
					arr := make([]map[string]any, 0, len(list))
					for _, comment := range list {
						arr = append(arr, map[string]any{
							"id":         comment.ID,
							"body":       comment.Body,
							"created_at": comment.CreatedAt,
							"user":       map[string]any{"login": VIEWER.Login, "id": 1000, "type": "User"},
						})
					}
					r := jsonResp(200, arr, nil)
					return &r
				}
				if method == "POST" {
					parsed, ok := parseJSONBody(ctx)
					if !ok {
						r := jsonErrorMsg(400, "Problems parsing JSON")
						return &r
					}
					body, _ := parsed.(map[string]any)
					commentBody := stringField(body, "body")
					if commentBody == "" {
						r := jsonErrorMsg(422, "body is required")
						return &r
					}
					comment := store.CreateComment(repo, issueNumber, commentBody)
					if comment == nil {
						r := jsonErrorMsg(404, "Not Found")
						return &r
					}
					r := jsonResp(201, map[string]any{"id": comment.ID, "body": comment.Body, "created_at": comment.CreatedAt, "user": map[string]any{"login": VIEWER.Login, "id": 1000, "type": "User"}}, nil)
					return &r
				}
				r := jsonErrorMsg(404, "Not Found")
				return &r
			}
			r := jsonErrorMsg(404, "Not Found")
			return &r
		}

		// pulls
		if tail[0] == "pulls" {
			if len(tail) == 1 {
				if method == "POST" {
					parsed, ok := parseJSONBody(ctx)
					if !ok {
						r := jsonErrorMsg(400, "Problems parsing JSON")
						return &r
					}
					body, _ := parsed.(map[string]any)
					title := stringField(body, "title")
					head := stringField(body, "head")
					base := stringField(body, "base")
					if title == "" || head == "" || base == "" {
						r := jsonErrorMsg(422, "title, head and base are required")
						return &r
					}
					bodyText := stringField(body, "body")
					result := store.CreatePull(repo, title, bodyText, head, base)
					if result.Error != "" {
						r := jsonErrorMsg(422, result.Error)
						return &r
					}
					r := jsonResp(201, pullJSON(repo, result.Pull, ctx.Origin), nil)
					return &r
				}
				if method == "GET" {
					state := ctx.Query.Get("state")
					if state == "" {
						state = "open"
					}
					var pulls []*PullRequest
					for _, pull := range repo.Pulls {
						if state == "open" && pull.State != "open" {
							continue
						}
						if state == "closed" && pull.State != "closed" {
							continue
						}
						pulls = append(pulls, pull)
					}
					sort.Slice(pulls, func(i, j int) bool { return pulls[i].Seq > pulls[j].Seq })
					page, total, pageNumber, perPage := paginate(pulls, ctx)
					headers := map[string]string{}
					if links := paginationLinks(ctx, pageNumber, perPage, total); links != "" {
						headers["link"] = links
					}
					arr := make([]map[string]any, 0, len(page))
					for _, pull := range page {
						arr = append(arr, pullJSON(repo, pull, ctx.Origin))
					}
					r := jsonResp(200, arr, headers)
					return &r
				}
				r := jsonErrorMsg(404, "Not Found")
				return &r
			}
			pullNumber, err := strconv.Atoi(tail[1])
			if err != nil {
				r := jsonErrorMsg(404, "Not Found")
				return &r
			}
			pull := repo.Pulls[pullNumber]
			if len(tail) == 2 {
				if pull == nil {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				if method == "GET" {
					r := jsonResp(200, pullJSON(repo, pull, ctx.Origin), nil)
					return &r
				}
				r := jsonErrorMsg(404, "Not Found")
				return &r
			}
			if len(tail) == 3 && tail[2] == "files" {
				if pull == nil {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				if method != "GET" {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				baseTip, ok1 := repo.Refs.Get("refs/heads/" + pull.BaseRef)
				headTip, ok2 := repo.Refs.Get("refs/heads/" + pull.HeadRef)
				if !ok1 || !ok2 {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				baseOID := mergeBase(headTip, baseTip, func(oid string) *GitObject { return repo.Lookup(oid) })
				if baseOID == "" {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				baseObject := repo.Lookup(baseOID)
				if baseObject == nil || baseObject.Type != ObjCommit {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				baseTree := parseCommit(baseObject.Data).Tree
				headTree := repo.TreeAt("refs/heads/" + pull.HeadRef)
				if headTree == "" {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				files := store.DiffFiles(repo, baseTree, headTree)
				arr := make([]map[string]any, 0, len(files))
				for _, f := range files {
					arr = append(arr, map[string]any{
						"filename":  f.Filename,
						"status":    f.Status,
						"additions": f.Additions,
						"deletions": f.Deletions,
						"changes":   f.Changes,
					})
				}
				r := jsonResp(200, arr, nil)
				return &r
			}
			if len(tail) == 3 && tail[2] == "merge" {
				if pull == nil {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				if method != "PUT" {
					r := jsonErrorMsg(404, "Not Found")
					return &r
				}
				if _, ok := parseJSONBody(ctx); !ok {
					r := jsonErrorMsg(400, "Problems parsing JSON")
					return &r
				}
				result := store.MergePull(repo, pull)
				if result.Error != "" {
					if result.Error == "conflicted" {
						r := jsonErrorMsg(405, "Merge conflict")
						return &r
					}
					r := jsonErrorMsg(409, "Pull request is already merged")
					return &r
				}
				r := jsonResp(200, map[string]any{"merged": true, "sha": result.SHA, "message": "Pull request #" + strconv.Itoa(pull.Number) + " merged"}, nil)
				return &r
			}
			r := jsonErrorMsg(404, "Not Found")
			return &r
		}

		r := jsonErrorMsg(404, "Not Found")
		return &r
	}

	r := jsonErrorMsg(404, "Not Found")
	return &r
}

// findDirSHA walks a tree following segments and returns the oid of the final entry.
func findDirSHA(repo *Repo, treeOID string, segments []string) string {
	if len(segments) == 0 {
		return ""
	}
	object := repo.Lookup(treeOID)
	if object == nil || object.Type != ObjTree {
		return ""
	}
	for _, entry := range parseTree(object.Data) {
		if entry.Name == segments[0] {
			if len(segments) == 1 {
				return entry.OID
			}
			return findDirSHA(repo, entry.OID, segments[1:])
		}
	}
	return ""
}
