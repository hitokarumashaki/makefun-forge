// The forge's store: one in-memory store shared by REST, GraphQL, MCP and git
// smart HTTP, so an object created on one surface reads back identically on the
// others. Repositories own their git object stores and refs; issues, pull
// requests, labels and comments live beside them.
// Concurrency: the whole store is guarded by one RWMutex (held by the router
// for the duration of a request: RLock for reads, Lock for writes). Git
// objects are immutable once stored, so their bytes stay valid after the lock
// is released.

package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Label struct {
	Name  string
	Color string
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []Label
	Seq       int
	CreatedAt string
	UpdatedAt string
	// Serialized JSON per origin (the body carries origin-derived URLs).
	// Cleared whenever the issue is mutated, so a cached string is always
	// current. sync.Map so concurrent reads can populate it safely.
	JSONCache *sync.Map // map[string]string
}

type Comment struct {
	ID        int
	Body      string
	CreatedAt string
}

type PullRequest struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	HeadRef        string
	BaseRef        string
	HeadSHA        string
	BaseSHA        string
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
	Seq            int
	CreatedAt      string
}

// RefMap is a ref map that bumps the owning repo's refVersion on any mutation,
// so caches keyed on ref state (ls-refs, advertisements) can invalidate cheaply.
type RefMap struct {
	m    map[string]string
	repo *Repo
}

func newRefMap(repo *Repo) *RefMap {
	return &RefMap{m: map[string]string{}, repo: repo}
}

func (r *RefMap) Get(key string) (string, bool) {
	v, ok := r.m[key]
	return v, ok
}

func (r *RefMap) Set(key, value string) {
	r.repo.RefVersion++
	r.m[key] = value
}

func (r *RefMap) Delete(key string) bool {
	if _, ok := r.m[key]; ok {
		delete(r.m, key)
		r.repo.RefVersion++
		return true
	}
	return false
}

func (r *RefMap) Has(key string) bool {
	_, ok := r.m[key]
	return ok
}

func (r *RefMap) Keys() []string {
	keys := make([]string, 0, len(r.m))
	for k := range r.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (r *RefMap) Len() int { return len(r.m) }

type Repo struct {
	ID            int
	Owner         string
	Name          string
	PrivateField  bool
	Description   string
	DefaultBranch string
	Objects       map[string]GitObject
	Refs          *RefMap
	Labels        map[string]Label
	Issues        map[int]*Issue
	Pulls         map[int]*PullRequest
	Comments      map[int][]*Comment
	NextIssueNum  int
	NextPullNum   int
	Seq           int
	// Bumped on every refs mutation; caches keyed on ref state read this.
	RefVersion int
}

func newRepo(id int, owner, name string, isPrivate bool, description string) *Repo {
	repo := &Repo{
		ID:            id,
		Owner:         owner,
		Name:          name,
		PrivateField:  isPrivate,
		Description:   description,
		DefaultBranch: "main",
		Objects:       map[string]GitObject{},
		Labels:        map[string]Label{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*PullRequest{},
		Comments:      map[int][]*Comment{},
		NextIssueNum:  1,
		NextPullNum:   1,
	}
	repo.Refs = newRefMap(repo)
	return repo
}

func (r *Repo) FullName() string { return r.Owner + "/" + r.Name }
func (r *Repo) IsPrivate() bool  { return r.PrivateField }

func (r *Repo) PutObject(typ GitObjectType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = GitObject{Type: typ, Data: data}
	}
	return oid
}

func (r *Repo) Lookup(oid string) *GitObject {
	if obj, ok := r.Objects[oid]; ok {
		o := obj
		return &o
	}
	return nil
}

func (r *Repo) RefTip(ref string) (string, bool) { return r.Refs.Get(ref) }

// BranchTip returns the tip commit of a branch name, or of the default branch.
func (r *Repo) BranchTip(branch string) (string, bool) {
	if branch == "" {
		branch = r.DefaultBranch
	}
	return r.Refs.Get("refs/heads/" + branch)
}

func (r *Repo) HeadCommit() (string, bool) { return r.BranchTip("") }

// TreeAt returns the tree oid at a ref (branch name or full ref), or "" when unborn.
func (r *Repo) TreeAt(ref string) string {
	var tip string
	var ok bool
	if ref == "" {
		tip, ok = r.BranchTip("")
	} else {
		tip, ok = r.Refs.Get(ref)
		if !ok {
			tip, ok = r.Refs.Get("refs/heads/" + ref)
		}
	}
	if !ok || tip == ZeroOID {
		return ""
	}
	object := r.Lookup(tip)
	if object == nil || object.Type != ObjCommit {
		return ""
	}
	return parseCommit(object.Data).Tree
}

func isoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func b64url(text string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(text))
}

func unb64url(text string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Opaque global ids (GraphQL): never the plain database id or number.
func repoGlobalID(repo *Repo) string { return b64url("repo:" + strconv.Itoa(repo.ID)) }
func issueGlobalID(repo *Repo, issue *Issue) string {
	return b64url(fmt.Sprintf("issue:%d:%d", repo.ID, issue.Number))
}
func pullGlobalID(repo *Repo, pull *PullRequest) string {
	return b64url(fmt.Sprintf("pull:%d:%d", repo.ID, pull.Number))
}
func commentGlobalID(repo *Repo, issueNumber int, comment *Comment) string {
	return b64url(fmt.Sprintf("comment:%d:%d:%d", repo.ID, issueNumber, comment.ID))
}

type DecodedGlobalID struct {
	Kind      string // "repo" | "issue" | "pull" | "comment"
	RepoID    int
	Number    int
	CommentID int
}

func decodeGlobalID(id string) *DecodedGlobalID {
	text, err := unb64url(id)
	if err != nil {
		return nil
	}
	parts := strings.Split(text, ":")
	switch {
	case len(parts) == 2 && parts[0] == "repo":
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil
		}
		return &DecodedGlobalID{Kind: "repo", RepoID: n}
	case len(parts) == 3 && parts[0] == "issue":
		rid, err1 := strconv.Atoi(parts[1])
		num, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil {
			return nil
		}
		return &DecodedGlobalID{Kind: "issue", RepoID: rid, Number: num}
	case len(parts) == 3 && parts[0] == "pull":
		rid, err1 := strconv.Atoi(parts[1])
		num, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil {
			return nil
		}
		return &DecodedGlobalID{Kind: "pull", RepoID: rid, Number: num}
	case len(parts) == 4 && parts[0] == "comment":
		rid, err1 := strconv.Atoi(parts[1])
		num, err2 := strconv.Atoi(parts[2])
		cid, err3 := strconv.Atoi(parts[3])
		if err1 != nil || err2 != nil || err3 != nil {
			return nil
		}
		return &DecodedGlobalID{Kind: "comment", RepoID: rid, Number: num, CommentID: cid}
	}
	return nil
}

type Store struct {
	mu            sync.RWMutex
	Repos         map[string]*Repo
	ReposByID     map[int]*Repo
	nextRepoID    int
	nextIssueID   int
	nextCommentID int
	nextPullID    int
}

func NewStore() *Store {
	return &Store{
		Repos:         map[string]*Repo{},
		ReposByID:     map[int]*Repo{},
		nextRepoID:    1,
		nextIssueID:   1,
		nextCommentID: 1,
		nextPullID:    1,
	}
}

func (s *Store) ViewerLogin() string { return VIEWER.Login }

func (s *Store) RepoKey(owner, name string) string { return owner + "/" + name }

func (s *Store) GetRepo(owner, name string) *Repo { return s.Repos[s.RepoKey(owner, name)] }

func (s *Store) GetRepoByID(id int) *Repo { return s.ReposByID[id] }

func (s *Store) ListViewerRepos() []*Repo {
	var out []*Repo
	for _, repo := range s.Repos {
		if repo.Owner == VIEWER.Login {
			out = append(out, repo)
		}
	}
	// The TypeScript iterates a Map (insertion order = creation order). Go
	// map iteration is random, which breaks pagination (pages repeat repos),
	// so sort by id: ids are assigned sequentially at creation.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) CreateRepo(name string, isPrivate bool, description string) *Repo {
	if _, ok := s.Repos[s.RepoKey(VIEWER.Login, name)]; ok {
		return nil
	}
	repo := newRepo(s.nextRepoID, VIEWER.Login, name, isPrivate, description)
	s.nextRepoID++
	s.Repos[s.RepoKey(VIEWER.Login, name)] = repo
	s.ReposByID[repo.ID] = repo
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	repo := s.GetRepo(owner, name)
	if repo == nil {
		return false
	}
	delete(s.Repos, s.RepoKey(owner, name))
	delete(s.ReposByID, repo.ID)
	return true
}

// -------------------------------------------------------------------------------------------
// contents (file writes through the REST contents API)
// -------------------------------------------------------------------------------------------

type CommitFileResult struct {
	CommitSHA  string
	ContentSHA string
	Error      string // "" | "stale-sha" | "bad-branch"
}

func (s *Store) CommitFile(repo *Repo, path string, content []byte, message string, branch string) CommitFileResult {
	return s.CommitFileChecked(repo, path, content, message, branch, "")
}

func (s *Store) CommitFileChecked(repo *Repo, path string, content []byte, message string, branch string, expectedSHA string) CommitFileResult {
	branchName := branch
	if branchName == "" {
		branchName = repo.DefaultBranch
	}
	refName := "refs/heads/" + branchName
	tip, hasTip := repo.Refs.Get(refName)
	blobOID := repo.PutObject(ObjBlob, content)
	var parent string
	var baseTree map[string]string
	if hasTip && tip != ZeroOID {
		parent = tip
		treeOID := repo.TreeAt(refName)
		if treeOID == "" {
			baseTree = map[string]string{}
		} else {
			baseTree = flattenTree(treeOID, func(oid string) *GitObject { return repo.Lookup(oid) }, "")
		}
	} else if branchName == repo.DefaultBranch {
		parent = ""
		baseTree = map[string]string{}
	} else {
		defaultTip, ok := repo.BranchTip("")
		if !ok || defaultTip == ZeroOID {
			return CommitFileResult{Error: "bad-branch"}
		}
		parent = defaultTip
		treeOID := repo.TreeAt("")
		if treeOID == "" {
			baseTree = map[string]string{}
		} else {
			baseTree = flattenTree(treeOID, func(oid string) *GitObject { return repo.Lookup(oid) }, "")
		}
	}
	current, hasCurrent := baseTree[path]
	if expectedSHA != "" {
		if !hasCurrent || !strings.EqualFold(current, expectedSHA) {
			return CommitFileResult{Error: "stale-sha"}
		}
	}
	files := map[string]FileEntry{}
	for p, oid := range baseTree {
		files[p] = FileEntry{Mode: "100644", OID: oid}
	}
	files[path] = FileEntry{Mode: "100644", OID: blobOID}
	treeOID := buildTreeFromPaths(files, func(typ GitObjectType, data []byte) string {
		return repo.PutObject(typ, data)
	})
	timestamp := time.Now().Unix()
	author := fmt.Sprintf("%s <%s>", VIEWER.Name, VIEWER.Email)
	parents := []string{}
	if parent != "" {
		parents = []string{parent}
	}
	commitData := buildCommit(treeOID, parents, message, author, "", timestamp)
	commitSHA := repo.PutObject(ObjCommit, commitData)
	repo.Refs.Set(refName, commitSHA)
	return CommitFileResult{CommitSHA: commitSHA, ContentSHA: blobOID}
}

type DeleteFileResult struct {
	CommitSHA string
	Error     string // "" | "not-found" | "stale-sha"
}

func (s *Store) DeleteFile(repo *Repo, path, message, branch, expectedSHA string) DeleteFileResult {
	branchName := branch
	if branchName == "" {
		branchName = repo.DefaultBranch
	}
	refName := "refs/heads/" + branchName
	tip, ok := repo.Refs.Get(refName)
	if !ok || tip == ZeroOID {
		return DeleteFileResult{Error: "not-found"}
	}
	treeOID := repo.TreeAt(refName)
	if treeOID == "" {
		return DeleteFileResult{Error: "not-found"}
	}
	baseTree := flattenTree(treeOID, func(oid string) *GitObject { return repo.Lookup(oid) }, "")
	current, hasCurrent := baseTree[path]
	if !hasCurrent {
		return DeleteFileResult{Error: "not-found"}
	}
	if expectedSHA != "" && !strings.EqualFold(current, expectedSHA) {
		return DeleteFileResult{Error: "stale-sha"}
	}
	delete(baseTree, path)
	files := map[string]FileEntry{}
	for p, oid := range baseTree {
		files[p] = FileEntry{Mode: "100644", OID: oid}
	}
	newTreeOID := buildTreeFromPaths(files, func(typ GitObjectType, data []byte) string {
		return repo.PutObject(typ, data)
	})
	timestamp := time.Now().Unix()
	author := fmt.Sprintf("%s <%s>", VIEWER.Name, VIEWER.Email)
	commitSHA := repo.PutObject(ObjCommit, buildCommit(newTreeOID, []string{tip}, message, author, "", timestamp))
	repo.Refs.Set(refName, commitSHA)
	return DeleteFileResult{CommitSHA: commitSHA}
}

// -------------------------------------------------------------------------------------------
// issues and comments
// -------------------------------------------------------------------------------------------

func (s *Store) CreateIssue(repo *Repo, title, body string, labels []Label) *Issue {
	number := repo.NextIssueNum
	repo.NextIssueNum++
	now := isoNow()
	issue := &Issue{
		ID:        s.nextIssueID,
		Number:    number,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    append([]Label{}, labels...),
		Seq:       repo.Seq + 1,
		CreatedAt: now,
		UpdatedAt: now,
		JSONCache: &sync.Map{},
	}
	s.nextIssueID++
	repo.Seq++
	repo.Issues[number] = issue
	return issue
}

func (s *Store) CreateComment(repo *Repo, issueNumber int, body string) *Comment {
	if _, ok := repo.Issues[issueNumber]; !ok {
		return nil
	}
	comment := &Comment{ID: s.nextCommentID, Body: body, CreatedAt: isoNow()}
	s.nextCommentID++
	repo.Comments[issueNumber] = append(repo.Comments[issueNumber], comment)
	return comment
}

// -------------------------------------------------------------------------------------------
// pull requests
// -------------------------------------------------------------------------------------------

type CreatePullResult struct {
	Pull  *PullRequest
	Error string
}

func (s *Store) CreatePull(repo *Repo, title, body, head, base string) CreatePullResult {
	headTip, ok1 := repo.Refs.Get("refs/heads/" + head)
	baseTip, ok2 := repo.Refs.Get("refs/heads/" + base)
	if !ok1 || !ok2 {
		return CreatePullResult{Error: "unknown branch"}
	}
	for _, existing := range repo.Pulls {
		if existing.HeadRef == head && existing.State == "open" {
			return CreatePullResult{Error: "a pull request is already open from this branch"}
		}
	}
	now := isoNow()
	pull := &PullRequest{
		ID:        s.nextPullID,
		Number:    repo.NextPullNum,
		Title:     title,
		Body:      body,
		State:     "open",
		HeadRef:   head,
		BaseRef:   base,
		HeadSHA:   headTip,
		BaseSHA:   baseTip,
		Merged:    false,
		MergedAt:  "",
		Seq:       repo.Seq + 1,
		CreatedAt: now,
	}
	s.nextPullID++
	repo.NextPullNum++
	repo.Seq++
	repo.Pulls[pull.Number] = pull
	return CreatePullResult{Pull: pull}
}

type DiffFile struct {
	Filename  string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
	Changes   int
}

// DiffFiles returns the paths changed between two trees, with line counts.
func (s *Store) DiffFiles(repo *Repo, baseTreeOID, headTreeOID string) []DiffFile {
	lookup := func(oid string) *GitObject { return repo.Lookup(oid) }
	base := flattenTree(baseTreeOID, lookup, "")
	head := flattenTree(headTreeOID, lookup, "")
	var out []DiffFile
	names := map[string]bool{}
	for k := range base {
		names[k] = true
	}
	for k := range head {
		names[k] = true
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	countLines := func(oid string) []string {
		if oid == "" {
			return nil
		}
		object := repo.Lookup(oid)
		if object == nil {
			return nil
		}
		text := string(object.Data)
		if text == "" {
			return nil
		}
		return strings.Split(text, "\n")
	}
	for _, name := range sorted {
		baseOID, hasBase := base[name]
		headOID, hasHead := head[name]
		if hasBase && hasHead && baseOID == headOID {
			continue
		}
		if !hasBase {
			lines := countLines(headOID)
			out = append(out, DiffFile{Filename: name, Status: "added", Additions: len(lines), Deletions: 0, Changes: len(lines)})
		} else if !hasHead {
			lines := countLines(baseOID)
			out = append(out, DiffFile{Filename: name, Status: "removed", Additions: 0, Deletions: len(lines), Changes: len(lines)})
		} else {
			oldLines := countLines(baseOID)
			newLines := countLines(headOID)
			oldCount := map[string]int{}
			for _, line := range oldLines {
				oldCount[line]++
			}
			additions := 0
			for _, line := range newLines {
				if oldCount[line] > 0 {
					oldCount[line]--
				} else {
					additions++
				}
			}
			deletions := 0
			for _, remaining := range oldCount {
				deletions += remaining
			}
			out = append(out, DiffFile{Filename: name, Status: "modified", Additions: additions, Deletions: deletions, Changes: additions + deletions})
		}
	}
	return out
}

// PullMergeable reports whether merging pull would conflict.
func (s *Store) PullMergeable(repo *Repo, pull *PullRequest) string {
	lookup := func(oid string) *GitObject { return repo.Lookup(oid) }
	headTip, ok1 := repo.Refs.Get("refs/heads/" + pull.HeadRef)
	baseTip, ok2 := repo.Refs.Get("refs/heads/" + pull.BaseRef)
	if !ok1 || !ok2 {
		return "UNKNOWN"
	}
	base := mergeBase(headTip, baseTip, lookup)
	if base == "" {
		return "UNKNOWN"
	}
	baseCommit := repo.Lookup(base)
	if baseCommit == nil || baseCommit.Type != ObjCommit {
		return "UNKNOWN"
	}
	baseTree := parseCommit(baseCommit.Data).Tree
	headTree := repo.TreeAt("refs/heads/" + pull.HeadRef)
	ontoTree := repo.TreeAt("refs/heads/" + pull.BaseRef)
	if headTree == "" || ontoTree == "" {
		return "UNKNOWN"
	}
	baseFiles := flattenTree(baseTree, lookup, "")
	headFiles := flattenTree(headTree, lookup, "")
	ontoFiles := flattenTree(ontoTree, lookup, "")
	names := map[string]bool{}
	for k := range baseFiles {
		names[k] = true
	}
	for k := range headFiles {
		names[k] = true
	}
	for k := range ontoFiles {
		names[k] = true
	}
	for name := range names {
		// Missing entries read as "" — exactly like TS Map.get returning
		// undefined: "" !== oid is true, "" !== "" is false.
		b := baseFiles[name]
		h := headFiles[name]
		o := ontoFiles[name]
		headChanged := h != b
		ontoChanged := o != b
		if headChanged && ontoChanged && h != o {
			return "CONFLICTING"
		}
	}
	return "MERGEABLE"
}

type MergePullResult struct {
	SHA   string
	Error string // "" | "conflicted" | "already-merged"
}

// MergePull squash-merges pull onto its base branch.
func (s *Store) MergePull(repo *Repo, pull *PullRequest) MergePullResult {
	lookup := func(oid string) *GitObject { return repo.Lookup(oid) }
	if pull.Merged || pull.State != "open" {
		return MergePullResult{Error: "already-merged"}
	}
	if s.PullMergeable(repo, pull) == "CONFLICTING" {
		return MergePullResult{Error: "conflicted"}
	}
	headTip, ok1 := repo.Refs.Get("refs/heads/" + pull.HeadRef)
	baseTip, ok2 := repo.Refs.Get("refs/heads/" + pull.BaseRef)
	if !ok1 || !ok2 {
		return MergePullResult{Error: "conflicted"}
	}
	base := mergeBase(headTip, baseTip, lookup)
	var baseTree string
	if base != "" {
		if bc := repo.Lookup(base); bc != nil && bc.Type == ObjCommit {
			baseTree = parseCommit(bc.Data).Tree
		}
	}
	headTree := repo.TreeAt("refs/heads/" + pull.HeadRef)
	ontoTree := repo.TreeAt("refs/heads/" + pull.BaseRef)
	if headTree == "" || ontoTree == "" || baseTree == "" {
		return MergePullResult{Error: "conflicted"}
	}
	baseFiles := flattenTree(baseTree, lookup, "")
	headFiles := flattenTree(headTree, lookup, "")
	ontoFiles := flattenTree(ontoTree, lookup, "")
	merged := map[string]FileEntry{}
	for path, oid := range ontoFiles {
		merged[path] = FileEntry{Mode: "100644", OID: oid}
	}
	for path, oid := range headFiles {
		if baseFiles[path] != oid {
			merged[path] = FileEntry{Mode: "100644", OID: oid}
		}
	}
	for path := range baseFiles {
		_, hasHead := headFiles[path]
		_, hasOnto := ontoFiles[path]
		if !hasHead && hasOnto && baseFiles[path] != ontoFiles[path] {
			if baseFiles[path] == ontoFiles[path] {
				delete(merged, path)
			}
		}
		if !hasHead && baseFiles[path] == ontoFiles[path] {
			delete(merged, path)
		}
	}
	treeOID := buildTreeFromPaths(merged, func(typ GitObjectType, data []byte) string {
		return repo.PutObject(typ, data)
	})
	timestamp := time.Now().Unix()
	author := fmt.Sprintf("%s <%s>", VIEWER.Name, VIEWER.Email)
	message := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", pull.Number, pull.HeadRef, pull.Title)
	sha := repo.PutObject(ObjCommit, buildCommit(treeOID, []string{baseTip}, message, author, "", timestamp))
	repo.Refs.Set("refs/heads/"+pull.BaseRef, sha)
	pull.Merged = true
	pull.State = "closed"
	pull.MergedAt = isoNow()
	pull.MergeCommitSHA = sha
	return MergePullResult{SHA: sha}
}

// -------------------------------------------------------------------------------------------
// git helpers shared by the smart-HTTP surface
// -------------------------------------------------------------------------------------------

func (s *Store) ReachableFrom(tip string, repo *Repo) (map[string]bool, error) {
	return collectReachable([]string{tip}, func(oid string) *GitObject { return repo.Lookup(oid) })
}

func (s *Store) IsAncestorInRepo(ancestor, oid string, repo *Repo) bool {
	return isAncestor(ancestor, oid, func(id string) *GitObject { return repo.Lookup(id) })
}

func (s *Store) CommitHistoryInRepo(tip string, repo *Repo) []string {
	return commitHistory(tip, func(id string) *GitObject { return repo.Lookup(id) })
}

func isValidOID(value string) bool { return isObjectID(value) }

// sha1Hex is a small helper kept for parity checks (unused by the server path).
func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}
