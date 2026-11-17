// GraphQL at /api/graphql: a small hand-rolled executor for the documents gh
// and the suite send (queries, mutations, __type introspection, node(id:),
// cursor pagination, and two-operation batches). Unauthenticated is exactly 401;
// an unknown field is HTTP 200 with an errors array naming it; an unknown
// repository is data.repository = null with no errors.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// -------------------------------------------------------------------------------------------
// Parser (documents only: operations, variables, arguments, inline fragments, aliases)
// -------------------------------------------------------------------------------------------

type gqlValue struct {
	kind    string // "variable" | "string" | "int" | "float" | "boolean" | "null" | "enum" | "list" | "object"
	str     string
	num     float64
	boolean bool
	values  []*gqlValue
	fields  []gqlObjField
}

type gqlObjField struct {
	name  string
	value *gqlValue
}

type gqlArg struct {
	name  string
	value *gqlValue
}

type gqlSelection struct {
	kind             string // "field" | "fragment"
	name             string // response key (alias, or the field name)
	field            string // the field name (for a field selection)
	args             []gqlArg
	selections       []*gqlSelection
	typeCondition    string
	hasTypeCondition bool
}

type gqlOperation struct {
	typ           string // "query" | "mutation"
	name          string
	hasName       bool
	variableNames []string
	selections    []*gqlSelection
}

// jsonQuote renders s the way JSON.stringify does: escapes ", \, and control
// characters; leaves <, >, &, and non-ASCII as-is.
func jsonQuote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString("\\\"")
		case '\\':
			sb.WriteString("\\\\")
		case '\n':
			sb.WriteString("\\n")
		case '\t':
			sb.WriteString("\\t")
		case '\r':
			sb.WriteString("\\r")
		case '\b':
			sb.WriteString("\\b")
		case '\f':
			sb.WriteString("\\f")
		default:
			if r < 0x20 {
				fmt.Fprintf(&sb, "\\u%04x", r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}
func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func tokenize(source string) ([]string, error) {
	var tokens []string
	i := 0
	for i < len(source) {
		char := source[i]
		if char == ' ' || char == '\t' || char == '\n' || char == '\r' || char == ',' {
			i++
			continue
		}
		if char == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if char == '.' && i+2 < len(source) && source[i+1] == '.' && source[i+2] == '.' {
			tokens = append(tokens, "...")
			i += 3
			continue
		}
		if strings.ContainsRune("{}()[]:!=$@&|", rune(char)) {
			tokens = append(tokens, string(char))
			i++
			continue
		}
		if char == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(source) && source[j] != '"' {
				if source[j] == '\\' && j+1 < len(source) {
					escaped := source[j+1]
					switch escaped {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(escaped)
					}
					j += 2
				} else {
					out.WriteByte(source[j])
					j++
				}
			}
			tokens = append(tokens, jsonQuote(out.String()))
			i = j + 1
			continue
		}
		if isNameStart(char) {
			j := i + 1
			for j < len(source) && isNameChar(source[j]) {
				j++
			}
			tokens = append(tokens, source[i:j])
			i = j
			continue
		}
		if isDigit(char) || (char == '-' && i+1 < len(source) && isDigit(source[i+1])) {
			j := i
			if source[j] == '-' {
				j++
			}
			for j < len(source) && isDigit(source[j]) {
				j++
			}
			if j+1 < len(source) && source[j] == '.' && isDigit(source[j+1]) {
				j++
				for j < len(source) && isDigit(source[j]) {
					j++
				}
			}
			tokens = append(tokens, source[i:j])
			i = j
			continue
		}
		return nil, fmt.Errorf("unexpected character %s at %d", jsonQuote(string(char)), i)
	}
	return tokens, nil
}

type gqlParser struct {
	pos    int
	tokens []string
}

func (p *gqlParser) peek() (string, bool) {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos], true
	}
	return "", false
}

func (p *gqlParser) next() (string, error) {
	if p.pos >= len(p.tokens) {
		return "", fmt.Errorf("unexpected end of document")
	}
	tok := p.tokens[p.pos]
	p.pos++
	return tok, nil
}

func (p *gqlParser) expect(token string) error {
	got, err := p.next()
	if err != nil {
		return err
	}
	if got != token {
		return fmt.Errorf("expected %s, got %s", jsonQuote(token), jsonQuote(got))
	}
	return nil
}

func (p *gqlParser) parseDocument() ([]*gqlOperation, error) {
	var operations []*gqlOperation
	for p.pos < len(p.tokens) {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		operations = append(operations, op)
	}
	return operations, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{typ: "query"}
	if tok, ok := p.peek(); ok && (tok == "query" || tok == "mutation") {
		op.typ, _ = p.next()
		if next, ok := p.peek(); ok && next != "(" && next != "{" {
			name, err := p.next()
			if err != nil {
				return nil, err
			}
			op.name = name
			op.hasName = true
		}
	}
	var variableNames []string
	if tok, ok := p.peek(); ok && tok == "(" {
		p.next()
		for {
			peek, ok := p.peek()
			if !ok {
				return nil, fmt.Errorf("unexpected end of document")
			}
			if peek == ")" {
				break
			}
			if err := p.expect("$"); err != nil {
				return nil, err
			}
			name, err := p.next()
			if err != nil {
				return nil, err
			}
			variableNames = append(variableNames, name)
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			if err := p.parseType(); err != nil {
				return nil, err
			}
			if peek, ok := p.peek(); ok && peek == "=" {
				p.next()
				if _, err := p.parseValue(); err != nil {
					return nil, err
				}
			}
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	op.variableNames = variableNames
	selections, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selections = selections
	return op, nil
}

func (p *gqlParser) parseType() error {
	if tok, ok := p.peek(); ok && tok == "[" {
		p.next()
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expect("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.next(); err != nil {
			return err
		}
	}
	if tok, ok := p.peek(); ok && tok == "!" {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var selections []*gqlSelection
	for {
		peek, ok := p.peek()
		if !ok {
			return nil, fmt.Errorf("unexpected end of document")
		}
		if peek == "}" {
			break
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, sel)
	}
	if err := p.expect("}"); err != nil {
		return nil, err
	}
	return selections, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	if tok, ok := p.peek(); ok && tok == "..." {
		p.next()
		sel := &gqlSelection{kind: "fragment"}
		if tok, ok := p.peek(); ok && tok == "on" {
			p.next()
			tc, err := p.next()
			if err != nil {
				return nil, err
			}
			sel.typeCondition = tc
			sel.hasTypeCondition = true
		}
		selections, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selections = selections
		sel.name = sel.typeCondition
		return sel, nil
	}
	first, err := p.next()
	if err != nil {
		return nil, err
	}
	sel := &gqlSelection{kind: "field", name: first, field: first}
	if tok, ok := p.peek(); ok && tok == ":" {
		p.next()
		field, err := p.next()
		if err != nil {
			return nil, err
		}
		sel.field = field
	}
	if tok, ok := p.peek(); ok && tok == "(" {
		p.next()
		for {
			peek, ok := p.peek()
			if !ok {
				return nil, fmt.Errorf("unexpected end of document")
			}
			if peek == ")" {
				break
			}
			argName, err := p.next()
			if err != nil {
				return nil, err
			}
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			sel.args = append(sel.args, gqlArg{name: argName, value: value})
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	if tok, ok := p.peek(); ok && tok == "{" {
		selections, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selections = selections
	}
	return sel, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	token, err := p.next()
	if err != nil {
		return nil, err
	}
	switch token {
	case "$":
		name, err := p.next()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "variable", str: name}, nil
	case "[":
		v := &gqlValue{kind: "list"}
		for {
			peek, ok := p.peek()
			if !ok {
				return nil, fmt.Errorf("unexpected end of document")
			}
			if peek == "]" {
				break
			}
			item, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.values = append(v.values, item)
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		return v, nil
	case "{":
		v := &gqlValue{kind: "object"}
		for {
			peek, ok := p.peek()
			if !ok {
				return nil, fmt.Errorf("unexpected end of document")
			}
			if peek == "}" {
				break
			}
			name, err := p.next()
			if err != nil {
				return nil, err
			}
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.fields = append(v.fields, gqlObjField{name: name, value: value})
		}
		if err := p.expect("}"); err != nil {
			return nil, err
		}
		return v, nil
	case "true":
		return &gqlValue{kind: "boolean", boolean: true}, nil
	case "false":
		return &gqlValue{kind: "boolean", boolean: false}, nil
	case "null":
		return &gqlValue{kind: "null"}, nil
	}
	if strings.HasPrefix(token, "\"") {
		var s string
		if err := json.Unmarshal([]byte(token), &s); err != nil {
			return nil, err
		}
		return &gqlValue{kind: "string", str: s}, nil
	}
	if isIntToken(token) {
		n, _ := strconv.ParseFloat(token, 64)
		return &gqlValue{kind: "int", num: n}, nil
	}
	if isFloatToken(token) {
		n, _ := strconv.ParseFloat(token, 64)
		return &gqlValue{kind: "float", num: n}, nil
	}
	return &gqlValue{kind: "enum", str: token}, nil
}

func isIntToken(token string) bool {
	if token == "" {
		return false
	}
	i := 0
	if token[0] == '-' {
		i = 1
		if len(token) == 1 {
			return false
		}
	}
	for ; i < len(token); i++ {
		if !isDigit(token[i]) {
			return false
		}
	}
	return true
}

func isFloatToken(token string) bool {
	// -? digits . digits
	i := 0
	if len(token) > 0 && token[0] == '-' {
		i = 1
	}
	digitsBefore := 0
	for i < len(token) && isDigit(token[i]) {
		i++
		digitsBefore++
	}
	if digitsBefore == 0 || i >= len(token) || token[i] != '.' {
		return false
	}
	i++
	digitsAfter := 0
	for i < len(token) && isDigit(token[i]) {
		i++
		digitsAfter++
	}
	return digitsAfter > 0 && i == len(token)
}

func resolveGQLValue(node *gqlValue, variables map[string]any) any {
	switch node.kind {
	case "variable":
		return variables[node.str]
	case "string":
		return node.str
	case "int", "float":
		return node.num
	case "boolean":
		return node.boolean
	case "null":
		return nil
	case "enum":
		return node.str
	case "list":
		out := make([]any, 0, len(node.values))
		for _, v := range node.values {
			out = append(out, resolveGQLValue(v, variables))
		}
		return out
	case "object":
		out := map[string]any{}
		for _, f := range node.fields {
			out[f.name] = resolveGQLValue(f.value, variables)
		}
		return out
	}
	return nil
}

// -------------------------------------------------------------------------------------------
// Schema (the fields each type answers; an unknown field is an error, not a crash)
// -------------------------------------------------------------------------------------------

var gqlSchemaFields = map[string][]string{
	"Query":    {"viewer", "repository", "node", "__type", "__schema"},
	"Mutation": {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":     {"login", "id", "name", "email"},
	"Repository": {
		"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission",
		"hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private",
	},
	"Issue":                    {"id", "number", "title", "state", "body"},
	"IssueConnection":          {"nodes", "pageInfo", "totalCount"},
	"PageInfo":                 {"hasNextPage", "endCursor", "hasPreviousPage", "startCursor"},
	"PullRequest":              {"id", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files"},
	"PullRequestConnection":    {"nodes", "pageInfo", "totalCount"},
	"PullRequestChangedFile":   {"path"},
	"Comment":                  {"id", "body"},
	"CommentEdge":              {"node", "cursor"},
	"CreateIssuePayload":       {"issue"},
	"CloseIssuePayload":        {"issue"},
	"CreatePullRequestPayload": {"pullRequest"},
	"MergePullRequestPayload":  {"pullRequest"},
	"AddCommentPayload":        {"commentEdge"},
	"__Type":                   {"name", "kind", "fields", "enumValues"},
	"__Field":                  {"name"},
	"__EnumValue":              {"name"},
	"Ref":                      {"name"},
}

type gqlTypeInfo struct {
	fields     []string
	enumValues []string
}

var gqlIntrospection = map[string]gqlTypeInfo{
	"Issue":       {fields: []string{"id", "number", "title", "state", "body", "createdAt", "updatedAt", "url"}},
	"PullRequest": {fields: []string{"id", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files"}},
	"Repository": {fields: []string{
		"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission",
		"hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private",
	}},
	"SearchType": {enumValues: []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE"}},
	"User":       {fields: []string{"login", "id", "name", "email"}},
}

// -------------------------------------------------------------------------------------------
// Executor
// -------------------------------------------------------------------------------------------

type gqlNode struct {
	typename string
	value    any
}

type gqlIssueVal struct {
	repo  *Repo
	issue *Issue
}

type gqlPullVal struct {
	repo *Repo
	pull *PullRequest
}

type gqlCommentVal struct {
	id   string
	body string
}

type gqlEdgeVal struct {
	node   gqlCommentVal
	cursor string
}

type gqlPageInfo struct {
	hasNextPage     bool
	endCursor       any
	hasPreviousPage bool
	startCursor     any
}

type gqlConnection struct {
	nodes      []any
	pageInfo   gqlPageInfo
	totalCount int
}

type gqlTypeVal struct {
	name       string
	fields     []string
	enumValues []string
}

type gqlRefVal struct {
	name string
}

type gqlExecutor struct {
	store     *Store
	variables map[string]any
	errors    []map[string]any
}

func gqlArgValue(sel *gqlSelection, name string, ex *gqlExecutor) any {
	for _, arg := range sel.args {
		if arg.name == name {
			return resolveGQLValue(arg.value, ex.variables)
		}
	}
	return nil
}

func issueCursor(issue *Issue) string {
	return base64.RawURLEncoding.EncodeToString([]byte("issue:" + strconv.Itoa(issue.Seq)))
}

func pullCursor(pull *PullRequest) string {
	return base64.RawURLEncoding.EncodeToString([]byte("pull:" + strconv.Itoa(pull.Seq)))
}

func cursorSeq(cursor any) (int, bool) {
	s, ok := cursor.(string)
	if !ok || s == "" {
		return 0, false
	}
	text, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	rest := strings.TrimPrefix(string(text), "issue:")
	if rest == string(text) {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

func issueState(issue *Issue) string {
	if issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func pullState(pull *PullRequest) string {
	if pull.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func gqlExecuteSelections(ex *gqlExecutor, parent gqlNode, selections []*gqlSelection) map[string]any {
	out := map[string]any{}
	for _, sel := range selections {
		if sel.kind == "fragment" {
			if sel.hasTypeCondition && sel.typeCondition != parent.typename {
				continue
			}
			for k, v := range gqlExecuteSelections(ex, parent, sel.selections) {
				out[k] = v
			}
			continue
		}
		field := sel.field
		known := gqlSchemaFields[parent.typename]
		found := false
		for _, k := range known {
			if k == field {
				found = true
				break
			}
		}
		if !found {
			ex.errors = append(ex.errors, map[string]any{"message": "Cannot query field \"" + field + "\" on type \"" + parent.typename + "\"."})
			out[sel.name] = nil
			continue
		}
		out[sel.name] = gqlResolveField(ex, parent, sel)
	}
	return out
}

func gqlResolveField(ex *gqlExecutor, parent gqlNode, sel *gqlSelection) any {
	store := ex.store
	field := sel.field
	sub := sel.selections
	value := parent.value

	switch parent.typename {
	case "Query":
		if field == "viewer" {
			return gqlExecuteSelections(ex, gqlNode{typename: "User", value: nil}, sub)
		}
		if field == "repository" {
			owner, _ := gqlArgValue(sel, "owner", ex).(string)
			name, _ := gqlArgValue(sel, "name", ex).(string)
			ownerOK := gqlArgValue(sel, "owner", ex) != nil
			nameOK := gqlArgValue(sel, "name", ex) != nil
			var repo *Repo
			if ownerOK && nameOK {
				repo = store.GetRepo(owner, name)
			}
			if repo == nil || repo.Owner != VIEWER.Login {
				return nil
			}
			return gqlExecuteSelections(ex, gqlNode{typename: "Repository", value: repo}, sub)
		}
		if field == "node" {
			idVal := gqlArgValue(sel, "id", ex)
			id, isStr := idVal.(string)
			if !isStr {
				return nil
			}
			decoded := decodeGlobalID(id)
			if decoded == nil {
				return nil
			}
			repo := store.GetRepoByID(decoded.RepoID)
			if repo == nil {
				return nil
			}
			switch decoded.Kind {
			case "repo":
				return gqlExecuteSelections(ex, gqlNode{typename: "Repository", value: repo}, sub)
			case "issue":
				issue := repo.Issues[decoded.Number]
				if issue == nil {
					return nil
				}
				return gqlExecuteSelections(ex, gqlNode{typename: "Issue", value: gqlIssueVal{repo, issue}}, sub)
			case "pull":
				pull := repo.Pulls[decoded.Number]
				if pull == nil {
					return nil
				}
				return gqlExecuteSelections(ex, gqlNode{typename: "PullRequest", value: gqlPullVal{repo, pull}}, sub)
			}
			return nil
		}
		if field == "__type" {
			name, _ := gqlArgValue(sel, "name", ex).(string)
			nameOK := gqlArgValue(sel, "name", ex) != nil
			if !nameOK {
				return nil
			}
			info, ok := gqlIntrospection[name]
			if !ok {
				return nil
			}
			return gqlExecuteSelections(ex, gqlNode{typename: "__Type", value: gqlTypeVal{name: name, fields: info.fields, enumValues: info.enumValues}}, sub)
		}
		return nil
	case "User":
		if field == "login" {
			return VIEWER.Login
		}
		if field == "id" {
			return "user:1000"
		}
		if field == "name" {
			return VIEWER.Name
		}
		if field == "email" {
			return VIEWER.Email
		}
		return nil
	case "Repository":
		repo := value.(*Repo)
		switch field {
		case "id":
			return repoGlobalID(repo)
		case "databaseId":
			return repo.ID
		case "name":
			return repo.Name
		case "nameWithOwner":
			return repo.FullName()
		case "owner":
			return gqlExecuteSelections(ex, gqlNode{typename: "User", value: nil}, sub)
		case "defaultBranchRef":
			tip, ok := repo.BranchTip("")
			if !ok {
				return nil
			}
			_ = tip
			return gqlExecuteSelections(ex, gqlNode{typename: "Ref", value: gqlRefVal{name: repo.DefaultBranch}}, sub)
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "description":
			return repo.Description
		case "private":
			return repo.IsPrivate()
		case "issue":
			numVal := gqlArgValue(sel, "number", ex)
			num, ok := toInt(numVal)
			if !ok {
				return nil
			}
			issue := repo.Issues[num]
			if issue == nil {
				return nil
			}
			return gqlExecuteSelections(ex, gqlNode{typename: "Issue", value: gqlIssueVal{repo, issue}}, sub)
		case "issues":
			return gqlIssuesConnection(ex, repo, sel)
		case "pullRequests":
			return gqlPullsConnection(ex, repo, sel)
		case "pullRequest":
			numVal := gqlArgValue(sel, "number", ex)
			num, ok := toInt(numVal)
			if !ok {
				return nil
			}
			pull := repo.Pulls[num]
			if pull == nil {
				return nil
			}
			return gqlExecuteSelections(ex, gqlNode{typename: "PullRequest", value: gqlPullVal{repo, pull}}, sub)
		}
		return nil
	case "Issue":
		iv := value.(gqlIssueVal)
		switch field {
		case "id":
			return issueGlobalID(iv.repo, iv.issue)
		case "number":
			return iv.issue.Number
		case "title":
			return iv.issue.Title
		case "state":
			return issueState(iv.issue)
		case "body":
			return iv.issue.Body
		}
		return nil
	case "PullRequest":
		pv := value.(gqlPullVal)
		switch field {
		case "id":
			return pullGlobalID(pv.repo, pv.pull)
		case "number":
			return pv.pull.Number
		case "title":
			return pv.pull.Title
		case "body":
			return pv.pull.Body
		case "state":
			return pullState(pv.pull)
		case "merged":
			return pv.pull.Merged
		case "headRefName":
			return pv.pull.HeadRef
		case "baseRefName":
			return pv.pull.BaseRef
		case "mergeable":
			return store.PullMergeable(pv.repo, pv.pull)
		case "files":
			return gqlPullFiles(ex, pv.repo, pv.pull, sel)
		}
		return nil
	case "PullRequestChangedFile":
		if field == "path" {
			return value.(gqlRefVal).name
		}
		return nil
	case "Comment":
		c := value.(gqlCommentVal)
		if field == "id" {
			return c.id
		}
		if field == "body" {
			return c.body
		}
		return nil
	case "CommentEdge":
		e := value.(gqlEdgeVal)
		if field == "node" {
			return gqlExecuteSelections(ex, gqlNode{typename: "Comment", value: e.node}, sub)
		}
		if field == "cursor" {
			return e.cursor
		}
		return nil
	case "IssueConnection", "PullRequestConnection":
		conn := value.(gqlConnection)
		if field == "nodes" {
			return conn.nodes
		}
		if field == "pageInfo" {
			return gqlExecuteSelections(ex, gqlNode{typename: "PageInfo", value: conn.pageInfo}, sub)
		}
		if field == "totalCount" {
			return conn.totalCount
		}
		return nil
	case "PageInfo":
		info := value.(gqlPageInfo)
		switch field {
		case "hasNextPage":
			return info.hasNextPage
		case "endCursor":
			return info.endCursor
		case "hasPreviousPage":
			return info.hasPreviousPage
		case "startCursor":
			return info.startCursor
		}
		return nil
	case "__Type":
		info := value.(gqlTypeVal)
		switch field {
		case "name":
			return info.name
		case "kind":
			if len(info.enumValues) > 0 {
				return "ENUM"
			}
			return "OBJECT"
		case "fields":
			out := make([]any, 0, len(info.fields))
			for _, name := range info.fields {
				out = append(out, gqlExecuteSelections(ex, gqlNode{typename: "__Field", value: gqlRefVal{name: name}}, sub))
			}
			return out
		case "enumValues":
			out := make([]any, 0, len(info.enumValues))
			for _, name := range info.enumValues {
				out = append(out, gqlExecuteSelections(ex, gqlNode{typename: "__EnumValue", value: gqlRefVal{name: name}}, sub))
			}
			return out
		}
		return nil
	case "__Field", "__EnumValue":
		if field == "name" {
			return value.(gqlRefVal).name
		}
		return nil
	case "Ref":
		if field == "name" {
			return value.(gqlRefVal).name
		}
		return nil
	case "Mutation":
		return gqlResolveMutation(ex, sel)
	default:
		// Mutation payload types resolve their single field by delegating.
		switch parent.typename {
		case "CreateIssuePayload", "CloseIssuePayload":
			iv := value.(gqlIssueVal)
			if field == "issue" {
				return gqlExecuteSelections(ex, gqlNode{typename: "Issue", value: iv}, sub)
			}
		case "CreatePullRequestPayload", "MergePullRequestPayload":
			pv := value.(gqlPullVal)
			if field == "pullRequest" {
				return gqlExecuteSelections(ex, gqlNode{typename: "PullRequest", value: pv}, sub)
			}
		case "AddCommentPayload":
			ev := value.(gqlEdgeVal)
			if field == "commentEdge" {
				return gqlExecuteSelections(ex, gqlNode{typename: "CommentEdge", value: ev}, sub)
			}
		}
		return nil
	}
}

// toInt converts a GraphQL number argument (float64 from JSON or a literal)
// to an int. Non-numbers fail.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

func gqlIssuesConnection(ex *gqlExecutor, repo *Repo, sel *gqlSelection) map[string]any {
	firstVal := gqlArgValue(sel, "first", ex)
	first, firstOK := toInt(firstVal)
	after := gqlArgValue(sel, "after", ex)
	statesVal := gqlArgValue(sel, "states", ex)
	orderByVal := gqlArgValue(sel, "orderBy", ex)
	direction := "DESC"
	if orderBy, ok := orderByVal.(map[string]any); ok {
		if d, ok := orderBy["direction"].(string); ok && d != "" {
			direction = d
		}
	}
	var issues []*Issue
	for _, issue := range repo.Issues {
		issues = append(issues, issue)
	}
	if statesVal != nil {
		if list, ok := statesVal.([]any); ok {
			wanted := map[string]bool{}
			for _, s := range list {
				if str, ok := s.(string); ok {
					wanted[strings.ToLower(str)] = true
				}
			}
			var filtered []*Issue
			for _, issue := range issues {
				if wanted[issue.State] {
					filtered = append(filtered, issue)
				}
			}
			issues = filtered
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		if direction == "ASC" {
			return issues[i].Seq < issues[j].Seq
		}
		return issues[i].Seq > issues[j].Seq
	})
	if afterSeq, ok := cursorSeq(after); ok {
		var filtered []*Issue
		for _, issue := range issues {
			if direction == "ASC" {
				if issue.Seq > afterSeq {
					filtered = append(filtered, issue)
				}
			} else if issue.Seq < afterSeq {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}
	totalCount := len(issues)
	page := issues
	if firstOK {
		if first < len(issues) {
			page = issues[:first]
		}
	}
	hasNextPage := firstOK && len(issues) > first
	var endCursor any
	if len(page) == 0 {
		endCursor = nil
	} else {
		endCursor = issueCursor(page[len(page)-1])
	}
	sub := sel.selections
	out := map[string]any{}
	for _, fieldSel := range sub {
		if fieldSel.kind == "fragment" {
			continue
		}
		switch fieldSel.field {
		case "nodes":
			nodeSels := fieldSel.selections
			nodes := make([]any, 0, len(page))
			for _, issue := range page {
				nodes = append(nodes, gqlExecuteSelections(ex, gqlNode{typename: "Issue", value: gqlIssueVal{repo, issue}}, nodeSels))
			}
			out[fieldSel.name] = nodes
		case "pageInfo":
			out[fieldSel.name] = gqlExecuteSelections(ex, gqlNode{typename: "PageInfo", value: gqlPageInfo{hasNextPage: hasNextPage, endCursor: endCursor}}, fieldSel.selections)
		case "totalCount":
			out[fieldSel.name] = totalCount
		default:
			ex.errors = append(ex.errors, map[string]any{"message": "Cannot query field \"" + fieldSel.field + "\" on type \"IssueConnection\"."})
			out[fieldSel.name] = nil
		}
	}
	return out
}

func gqlPullsConnection(ex *gqlExecutor, repo *Repo, sel *gqlSelection) map[string]any {
	firstVal := gqlArgValue(sel, "first", ex)
	first, firstOK := toInt(firstVal)
	after := gqlArgValue(sel, "after", ex)
	var pulls []*PullRequest
	for _, pull := range repo.Pulls {
		pulls = append(pulls, pull)
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Seq > pulls[j].Seq })
	if afterStr, ok := after.(string); ok && afterStr != "" {
		if afterSeq, ok := cursorSeq(afterStr); ok {
			var filtered []*PullRequest
			for _, pull := range pulls {
				if pull.Seq < afterSeq {
					filtered = append(filtered, pull)
				}
			}
			pulls = filtered
		}
	}
	totalCount := len(pulls)
	page := pulls
	if firstOK {
		if first < len(pulls) {
			page = pulls[:first]
		}
	}
	hasNextPage := firstOK && len(pulls) > first
	var endCursor any
	if len(page) == 0 {
		endCursor = nil
	} else {
		endCursor = pullCursor(page[len(page)-1])
	}
	sub := sel.selections
	out := map[string]any{}
	for _, fieldSel := range sub {
		if fieldSel.kind == "fragment" {
			continue
		}
		switch fieldSel.field {
		case "nodes":
			nodeSels := fieldSel.selections
			nodes := make([]any, 0, len(page))
			for _, pull := range page {
				nodes = append(nodes, gqlExecuteSelections(ex, gqlNode{typename: "PullRequest", value: gqlPullVal{repo, pull}}, nodeSels))
			}
			out[fieldSel.name] = nodes
		case "pageInfo":
			out[fieldSel.name] = gqlExecuteSelections(ex, gqlNode{typename: "PageInfo", value: gqlPageInfo{hasNextPage: hasNextPage, endCursor: endCursor}}, fieldSel.selections)
		case "totalCount":
			out[fieldSel.name] = totalCount
		default:
			ex.errors = append(ex.errors, map[string]any{"message": "Cannot query field \"" + fieldSel.field + "\" on type \"PullRequestConnection\"."})
			out[fieldSel.name] = nil
		}
	}
	return out
}

func gqlPullFiles(ex *gqlExecutor, repo *Repo, pull *PullRequest, sel *gqlSelection) map[string]any {
	firstVal := gqlArgValue(sel, "first", ex)
	first, firstOK := toInt(firstVal)
	baseTip, baseOK := repo.Refs.Get("refs/heads/" + pull.BaseRef)
	headTip, headOK := repo.Refs.Get("refs/heads/" + pull.HeadRef)
	var paths []string
	if baseOK && headOK {
		baseOID := mergeBase(headTip, baseTip, func(oid string) *GitObject { return repo.Lookup(oid) })
		if baseOID != "" {
			baseObject := repo.Lookup(baseOID)
			headTree := repo.TreeAt("refs/heads/" + pull.HeadRef)
			if baseObject != nil && baseObject.Type == ObjCommit && headTree != "" {
				for _, f := range ex.store.DiffFiles(repo, parseCommit(baseObject.Data).Tree, headTree) {
					paths = append(paths, f.Filename)
				}
			}
		}
	}
	limited := paths
	if firstOK {
		if first < len(paths) {
			limited = paths[:first]
		}
	}
	sub := sel.selections
	out := map[string]any{}
	for _, fieldSel := range sub {
		if fieldSel.kind == "fragment" {
			continue
		}
		switch fieldSel.field {
		case "nodes":
			nodeSels := fieldSel.selections
			nodes := make([]any, 0, len(limited))
			for _, path := range limited {
				nodes = append(nodes, gqlExecuteSelections(ex, gqlNode{typename: "PullRequestChangedFile", value: gqlRefVal{name: path}}, nodeSels))
			}
			out[fieldSel.name] = nodes
		case "pageInfo":
			out[fieldSel.name] = gqlExecuteSelections(ex, gqlNode{typename: "PageInfo", value: gqlPageInfo{hasNextPage: false, endCursor: nil}}, fieldSel.selections)
		case "totalCount":
			out[fieldSel.name] = len(paths)
		default:
			ex.errors = append(ex.errors, map[string]any{"message": "Cannot query field \"" + fieldSel.field + "\" on type \"PullRequestChangedFileConnection\"."})
			out[fieldSel.name] = nil
		}
	}
	return out
}

func gqlResolveMutation(ex *gqlExecutor, sel *gqlSelection) any {
	store := ex.store
	field := sel.field
	inputVal := gqlArgValue(sel, "input", ex)
	input, _ := inputVal.(map[string]any)
	sub := sel.selections

	if field == "createIssue" {
		repositoryID, _ := input["repositoryId"].(string)
		title, _ := input["title"].(string)
		var decoded *DecodedGlobalID
		if repositoryID != "" {
			decoded = decodeGlobalID(repositoryID)
		}
		var repo *Repo
		if decoded != nil {
			repo = store.GetRepoByID(decoded.RepoID)
		}
		if repo == nil || title == "" {
			ex.errors = append(ex.errors, map[string]any{"message": "createIssue: the repository does not exist."})
			return nil
		}
		body, _ := input["body"].(string)
		issue := store.CreateIssue(repo, title, body, nil)
		return gqlExecuteSelections(ex, gqlNode{typename: "CreateIssuePayload", value: gqlIssueVal{repo, issue}}, sub)
	}
	if field == "closeIssue" {
		issueID, _ := input["issueId"].(string)
		var decoded *DecodedGlobalID
		if issueID != "" {
			decoded = decodeGlobalID(issueID)
		}
		var repo *Repo
		var issue *Issue
		if decoded != nil {
			repo = store.GetRepoByID(decoded.RepoID)
			if repo != nil {
				issue = repo.Issues[decoded.Number]
			}
		}
		if issue == nil || repo == nil {
			ex.errors = append(ex.errors, map[string]any{"message": "closeIssue: the issue does not exist."})
			return nil
		}
		issue.State = "closed"
		issue.UpdatedAt = isoNow()
		return gqlExecuteSelections(ex, gqlNode{typename: "CloseIssuePayload", value: gqlIssueVal{repo, issue}}, sub)
	}
	if field == "createPullRequest" {
		repositoryID, _ := input["repositoryId"].(string)
		var decoded *DecodedGlobalID
		if repositoryID != "" {
			decoded = decodeGlobalID(repositoryID)
		}
		var repo *Repo
		if decoded != nil {
			repo = store.GetRepoByID(decoded.RepoID)
		}
		title, _ := input["title"].(string)
		headRefName, _ := input["headRefName"].(string)
		baseRefName, _ := input["baseRefName"].(string)
		if repo == nil || title == "" || headRefName == "" || baseRefName == "" {
			ex.errors = append(ex.errors, map[string]any{"message": "createPullRequest: the repository or branches do not exist."})
			return nil
		}
		body, _ := input["body"].(string)
		result := store.CreatePull(repo, title, body, headRefName, baseRefName)
		if result.Error != "" {
			ex.errors = append(ex.errors, map[string]any{"message": "createPullRequest: " + result.Error})
			return nil
		}
		return gqlExecuteSelections(ex, gqlNode{typename: "CreatePullRequestPayload", value: gqlPullVal{repo, result.Pull}}, sub)
	}
	if field == "mergePullRequest" {
		pullRequestID, _ := input["pullRequestId"].(string)
		var decoded *DecodedGlobalID
		if pullRequestID != "" {
			decoded = decodeGlobalID(pullRequestID)
		}
		var repo *Repo
		var pull *PullRequest
		if decoded != nil {
			repo = store.GetRepoByID(decoded.RepoID)
			if repo != nil {
				pull = repo.Pulls[decoded.Number]
			}
		}
		if pull == nil || repo == nil {
			ex.errors = append(ex.errors, map[string]any{"message": "mergePullRequest: the pull request does not exist."})
			return nil
		}
		result := store.MergePull(repo, pull)
		if result.Error != "" {
			ex.errors = append(ex.errors, map[string]any{"message": "mergePullRequest: " + result.Error})
			return nil
		}
		return gqlExecuteSelections(ex, gqlNode{typename: "MergePullRequestPayload", value: gqlPullVal{repo, pull}}, sub)
	}
	if field == "addComment" {
		subjectID, _ := input["subjectId"].(string)
		body, bodyOK := input["body"].(string)
		var decoded *DecodedGlobalID
		if subjectID != "" {
			decoded = decodeGlobalID(subjectID)
		}
		var repo *Repo
		var issue *Issue
		if decoded != nil {
			repo = store.GetRepoByID(decoded.RepoID)
			if repo != nil {
				issue = repo.Issues[decoded.Number]
			}
		}
		if issue == nil || repo == nil || !bodyOK {
			ex.errors = append(ex.errors, map[string]any{"message": "addComment: the subject does not exist."})
			return nil
		}
		comment := store.CreateComment(repo, issue.Number, body)
		if comment == nil {
			ex.errors = append(ex.errors, map[string]any{"message": "addComment: the subject does not exist."})
			return nil
		}
		node := gqlCommentVal{id: commentGlobalID(repo, issue.Number, comment), body: comment.Body}
		return gqlExecuteSelections(ex, gqlNode{typename: "AddCommentPayload", value: gqlEdgeVal{node: node, cursor: issueCursor(issue)}}, sub)
	}
	return nil
}

func gqlExecuteOperation(store *Store, query string, variables map[string]any) map[string]any {
	ex := &gqlExecutor{store: store, variables: variables}
	tokens, err := tokenize(query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": err.Error()}}}
	}
	parser := &gqlParser{tokens: tokens}
	operations, err := parser.parseDocument()
	if err != nil {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": err.Error()}}}
	}
	if len(operations) == 0 {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": "empty document"}}}
	}
	operation := operations[0]
	rootType := "Query"
	if operation.typ == "mutation" {
		rootType = "Mutation"
	}
	data := gqlExecuteSelections(ex, gqlNode{typename: rootType, value: nil}, operation.selections)
	result := map[string]any{"data": data}
	if len(ex.errors) > 0 {
		result["errors"] = ex.errors
	}
	return result
}

func handleGraphQL(ctx *Ctx, store *Store, token string) *Resp {
	if ctx.Path != "/api/graphql" {
		return nil
	}
	if token != "" && bearerToken(ctx) != token {
		r := jsonErrorMsg(401, "Bad credentials")
		return &r
	}
	if ctx.Method != "POST" {
		r := jsonErrorMsg(405, "Method not allowed")
		return &r
	}
	parsed, ok := parseJSONBody(ctx)
	if !ok {
		r := jsonErrorMsg(400, "Problems parsing JSON")
		return &r
	}
	if arr, isArr := parsed.([]any); isArr {
		results := make([]map[string]any, 0, len(arr))
		for _, entry := range arr {
			entryMap, isRecord := entry.(map[string]any)
			if !isRecord {
				results = append(results, map[string]any{"data": nil, "errors": []map[string]any{{"message": "each operation needs a query string"}}})
				continue
			}
			query, ok := entryMap["query"].(string)
			if !ok {
				results = append(results, map[string]any{"data": nil, "errors": []map[string]any{{"message": "each operation needs a query string"}}})
				continue
			}
			variables, _ := entryMap["variables"].(map[string]any)
			if variables == nil {
				variables = map[string]any{}
			}
			results = append(results, gqlExecuteOperation(store, query, variables))
		}
		r := jsonResp(200, results, nil)
		return &r
	}
	body, isRecord := parsed.(map[string]any)
	if !isRecord {
		r := jsonErrorMsg(400, "query is required")
		return &r
	}
	query, ok := body["query"].(string)
	if !ok {
		r := jsonErrorMsg(400, "query is required")
		return &r
	}
	variables, _ := body["variables"].(map[string]any)
	if variables == nil {
		variables = map[string]any{}
	}
	r := jsonResp(200, gqlExecuteOperation(store, query, variables), nil)
	return &r
}
