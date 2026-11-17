// Git smart HTTP at /{owner}/{repo}.git: upload-pack protocol v2 (and v0) plus
// receive-pack v0. The object store, refs and pack codec live in gitcore.go;
// this file is the wire framing the kit's client expects:
//  - info/refs answers v2 when Git-Protocol: version=2 is sent, v0 otherwise.
//  - v2 ls-refs honours ref-prefix server-side, reports HEAD symref + peeled tags.
//  - v2/v0 fetch sends only objects not reachable from the client's haves, with
//    shallow-info for depth requests and an empty pack for a nothing-new fetch.
//  - receive-pack enforces fast-forward per ref and reports one line per ref.
// Caches (fetch responses, ls-refs, v0 ads) are bounded FIFO maps guarded by
// their own RWMutex; the store lock is released before any zlib work.

package main

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const agent = "makefun-forge/1.0"
const uploadAd = "application/x-git-upload-pack-advertisement"
const uploadResult = "application/x-git-upload-pack-result"
const receiveAd = "application/x-git-receive-pack-advertisement"
const receiveResult = "application/x-git-receive-pack-result"

// fifoCache is a bounded FIFO response cache.
type fifoCache struct {
	mu   sync.RWMutex
	m    map[string][]byte
	keys []string
	max  int
}

func newFifoCache(max int) *fifoCache {
	return &fifoCache{m: map[string][]byte{}, max: max}
}

func (c *fifoCache) get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	b, ok := c.m[key]
	return b, ok
}

func (c *fifoCache) put(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; ok {
		return
	}
	if len(c.keys) >= c.max {
		oldest := c.keys[0]
		c.keys = c.keys[1:]
		delete(c.m, oldest)
	}
	c.m[key] = body
	c.keys = append(c.keys, key)
}

var fetchCache = newFifoCache(200)
var lsrefsCache = newFifoCache(200)
var v0AdCache = newFifoCache(200)

func fetchCacheKey(version string, repo *Repo, wants, shallow []string, deepen *int, deepenSince *int64) string {
	d := ""
	if deepen != nil {
		d = strconv.Itoa(*deepen)
	}
	ds := ""
	if deepenSince != nil {
		ds = strconv.FormatInt(*deepenSince, 10)
	}
	return strings.Join([]string{version, strconv.Itoa(repo.ID), strings.Join(wants, ","), strings.Join(shallow, ","), d, ds}, "|")
}

// sidebandPack splits a pack into sideband band-1 pkt-lines (<= 65520 bytes per frame).
func sidebandPack(pack []byte) []byte {
	var parts bytes.Buffer
	maxChunk := 65520 - 4 - 1
	for offset := 0; offset < len(pack); offset += maxChunk {
		end := offset + maxChunk
		if end > len(pack) {
			end = len(pack)
		}
		chunk := pack[offset:end]
		frame := make([]byte, 1, 1+len(chunk))
		frame[0] = 1
		frame = append(frame, chunk...)
		parts.Write(encodePktLine(frame))
	}
	if len(pack) == 0 {
		parts.Write(encodePktLine([]byte{1}))
	}
	parts.Write(pktFlush)
	return parts.Bytes()
}

func sidebandTextLines(lines []string) []byte {
	var items []pktItem
	payload := encodePktItems(func() []pktItem {
		var it []pktItem
		for _, line := range lines {
			it = append(it, pktData(line+"\n"))
		}
		return it
	}())
	frame := append([]byte{1}, payload...)
	items = append(items, pktBytes(frame), pktFlushIt())
	return encodePktItems(items)
}

type refLine struct {
	oid  string
	name string
}

// listRefs returns all refs of a repo, HEAD first, then sorted; annotated tags
// carry their peeled target.
func listRefs(repo *Repo) (lines []refLine, peeled map[string]string) {
	names := repo.Refs.Keys()
	peeled = map[string]string{}
	if tip, ok := repo.Refs.Get("refs/heads/" + repo.DefaultBranch); ok {
		lines = append(lines, refLine{oid: tip, name: "HEAD"})
	}
	for _, name := range names {
		oid, _ := repo.Refs.Get(name)
		lines = append(lines, refLine{oid: oid, name: name})
		if obj := repo.Lookup(oid); obj != nil && obj.Type == ObjTag {
			if target := peelToCommit(repo, oid); target != "" {
				peeled[name] = target
			}
		}
	}
	return lines, peeled
}

// peelToCommit follows tag objects to the commit they point at ("" when not a commit).
func peelToCommit(repo *Repo, oid string) string {
	current := oid
	for depth := 0; depth < 10; depth++ {
		object := repo.Lookup(current)
		if object == nil {
			return ""
		}
		if object.Type == ObjCommit {
			return current
		}
		if object.Type == ObjTag {
			current = parseTag(object.Data).Object
			continue
		}
		return ""
	}
	return ""
}

func v0Advertisement(repo *Repo, service string) []byte {
	cacheKey := fmt.Sprintf("%d|%d|%s", repo.ID, repo.RefVersion, service)
	if cached, ok := v0AdCache.get(cacheKey); ok {
		return cached
	}
	var capabilities string
	if service == "git-upload-pack" {
		capabilities = fmt.Sprintf("side-band-64k shallow symref=HEAD:refs/heads/%s agent=%s", repo.DefaultBranch, agent)
	} else {
		capabilities = fmt.Sprintf("report-status side-band-64k delete-refs agent=%s", agent)
	}
	var items []pktItem
	items = append(items, pktData("# service="+service+"\n"), pktFlushIt())
	refs, peeled := listRefs(repo)
	if len(refs) == 0 {
		items = append(items, pktData(fmt.Sprintf("%s capabilities^{}\x00%s\n", ZeroOID, capabilities)))
	} else {
		for i, ref := range refs {
			suffix := ""
			if i == 0 {
				suffix = "\x00" + capabilities
			}
			items = append(items, pktData(fmt.Sprintf("%s %s%s\n", ref.oid, ref.name, suffix)))
			if peeledOID, ok := peeled[ref.name]; ok {
				items = append(items, pktData(fmt.Sprintf("%s %s^{}\n", peeledOID, ref.name)))
			}
		}
	}
	items = append(items, pktFlushIt())
	body := encodePktItems(items)
	v0AdCache.put(cacheKey, body)
	return body
}

var v2Advertisement = func() []byte {
	return encodePktItems([]pktItem{
		pktData("# service=git-upload-pack\n"),
		pktFlushIt(),
		pktData("version 2\n"),
		pktData("agent=" + agent + "\n"),
		pktData("fetch=shallow\n"),
		pktData("ls-refs\n"),
		pktData("object-format=sha1\n"),
		pktFlushIt(),
	})
}()

// ---------------------------------------------------------------------------------------------
// fetch planning
// ---------------------------------------------------------------------------------------------

type fetchPlan struct {
	objects     []GitObject
	shallow     []string
	unshallow   []string
	acks        []string
	unknownWant bool
}

var commitTimestampRe = regexp.MustCompile(` (\d+) [+-]\d{4}$`)

func planFetch(repo *Repo, wants, haves, shallowBoundaries []string, deepen *int, deepenSince *int64, deepenNot []string) fetchPlan {
	lookup := func(oid string) *GitObject { return repo.Lookup(oid) }
	var knownHaves []string
	for _, h := range haves {
		if repo.Lookup(h) != nil {
			knownHaves = append(knownHaves, h)
		}
	}
	shallowHeld := map[string]bool{}
	for _, s := range shallowBoundaries {
		shallowHeld[s] = true
	}
	depthLimited := deepen != nil || deepenSince != nil
	excluded := map[string]bool{}
	excludeClosure := func(oid string, shallowOnly bool) {
		object := lookup(oid)
		if object == nil {
			return
		}
		if !shallowOnly {
			reachable, err := collectReachable([]string{oid}, lookup)
			if err == nil {
				for next := range reachable {
					excluded[next] = true
				}
			}
			return
		}
		excluded[oid] = true
		if object.Type == ObjCommit {
			stack := []string{parseCommit(object.Data).Tree}
			for len(stack) > 0 {
				current := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if excluded[current] {
					continue
				}
				entry := lookup(current)
				if entry == nil {
					continue
				}
				excluded[current] = true
				if entry.Type == ObjTree {
					for _, te := range parseTree(entry.Data) {
						if te.Mode != "160000" {
							stack = append(stack, te.OID)
						}
					}
				}
			}
		}
	}
	for _, have := range knownHaves {
		excludeClosure(have, depthLimited && shallowHeld[have])
	}
	for _, boundary := range shallowBoundaries {
		excludeClosure(boundary, depthLimited)
	}
	for _, ref := range deepenNot {
		tip, ok := repo.Refs.Get(ref)
		if !ok {
			tip, ok = repo.Refs.Get("refs/heads/" + ref)
		}
		if ok {
			reachable, err := collectReachable([]string{tip}, lookup)
			if err == nil {
				for oid := range reachable {
					excluded[oid] = true
				}
			}
		}
	}

	// Commits reachable from the wants, with their generation depth from a want.
	commitDepth := map[string]int{}
	type qItem struct {
		oid   string
		depth int
	}
	var queue []qItem
	for _, want := range wants {
		object := repo.Lookup(want)
		if object == nil {
			continue
		}
		commitOID := want
		if object.Type == ObjTag {
			commitOID = peelToCommit(repo, want)
			if commitOID == "" {
				continue
			}
		}
		if _, ok := commitDepth[commitOID]; !ok {
			commitDepth[commitOID] = 1
			queue = append(queue, qItem{oid: commitOID, depth: 1})
		}
	}
	commitTimestamp := func(oid string) int64 {
		object := repo.Lookup(oid)
		if object == nil || object.Type != ObjCommit {
			return 0
		}
		author := parseCommit(object.Data).Author
		m := commitTimestampRe.FindStringSubmatch(author)
		if m == nil {
			return 0
		}
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return n
	}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if deepen != nil && item.depth >= *deepen {
			continue
		}
		object := repo.Lookup(item.oid)
		if object == nil || object.Type != ObjCommit {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			if _, ok := commitDepth[parent]; !ok {
				commitDepth[parent] = item.depth + 1
				queue = append(queue, qItem{oid: parent, depth: item.depth + 1})
			}
		}
	}

	var includedCommits []string
	var shallow, unshallow []string
	if deepen != nil {
		for oid, depth := range commitDepth {
			if depth <= *deepen {
				includedCommits = append(includedCommits, oid)
			}
		}
		for oid, depth := range commitDepth {
			if depth != *deepen {
				continue
			}
			object := repo.Lookup(oid)
			if object == nil || object.Type != ObjCommit {
				continue
			}
			if len(parseCommit(object.Data).Parents) > 0 {
				shallow = append(shallow, oid)
			}
		}
		for _, boundary := range shallowBoundaries {
			found := false
			for _, c := range includedCommits {
				if c == boundary {
					found = true
					break
				}
			}
			if !found {
				continue
			}
			object := repo.Lookup(boundary)
			var parents []string
			if object != nil && object.Type == ObjCommit {
				parents = parseCommit(object.Data).Parents
			}
			allIncluded := true
			for _, p := range parents {
				in := false
				for _, c := range includedCommits {
					if c == p {
						in = true
						break
					}
				}
				if !in {
					allIncluded = false
					break
				}
			}
			if allIncluded {
				unshallow = append(unshallow, boundary)
			}
		}
	} else if deepenSince != nil {
		for oid := range commitDepth {
			if commitTimestamp(oid) >= *deepenSince || (len(wants) > 0 && oid == wants[0]) {
				includedCommits = append(includedCommits, oid)
			}
		}
		for _, oid := range includedCommits {
			object := repo.Lookup(oid)
			if object == nil || object.Type != ObjCommit {
				continue
			}
			parents := parseCommit(object.Data).Parents
			for _, parent := range parents {
				in := false
				for _, c := range includedCommits {
					if c == parent {
						in = true
						break
					}
				}
				if !in {
					shallow = append(shallow, oid)
					break
				}
			}
		}
	} else {
		for oid := range commitDepth {
			includedCommits = append(includedCommits, oid)
		}
	}

	// The pack holds the included commits and their tree/blob closures only.
	reachable := map[string]bool{}
	addTreeClosure := func(treeOID string) {
		stack := []string{treeOID}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if reachable[oid] {
				continue
			}
			object := lookup(oid)
			if object == nil {
				continue
			}
			reachable[oid] = true
			if object.Type == ObjTree {
				for _, entry := range parseTree(object.Data) {
					if entry.Mode != "160000" {
						stack = append(stack, entry.OID)
					}
				}
			}
		}
	}
	for _, commitOID := range includedCommits {
		reachable[commitOID] = true
		if object := lookup(commitOID); object != nil && object.Type == ObjCommit {
			addTreeClosure(parseCommit(object.Data).Tree)
		}
	}
	for _, want := range wants {
		object := lookup(want)
		if object == nil {
			continue
		}
		if object.Type == ObjTag {
			reachable[want] = true
		} else if object.Type != ObjCommit {
			r, err := collectReachable([]string{want}, lookup)
			if err == nil {
				for oid := range r {
					reachable[oid] = true
				}
			}
		}
	}
	for oid := range reachable {
		if excluded[oid] {
			delete(reachable, oid)
		}
	}

	var oids []string
	for oid := range reachable {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	var objects []GitObject
	for _, oid := range oids {
		if object := repo.Lookup(oid); object != nil {
			objects = append(objects, *object)
		}
	}
	unknownWant := false
	for _, want := range wants {
		if repo.Lookup(want) == nil {
			unknownWant = true
			break
		}
	}
	return fetchPlan{objects: objects, shallow: shallow, unshallow: unshallow, acks: knownHaves, unknownWant: unknownWant}
}

// ---------------------------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------------------------

func parseRequestLines(body []byte) (lines []string, delimAt int, err error) {
	packets, err := decodePktLines(body)
	if err != nil {
		return nil, -1, err
	}
	delimAt = -1
	for _, packet := range packets {
		if packet.Kind == PktDelim {
			if delimAt < 0 {
				delimAt = len(lines)
			}
			continue
		}
		if packet.Kind == PktFlush {
			break
		}
		if packet.Kind == PktData {
			lines = append(lines, string(packet.Payload))
		}
	}
	return lines, delimAt, nil
}

func handleLsRefs(repo *Repo, body []byte) Resp {
	lines, _, err := parseRequestLines(body)
	if err != nil {
		return jsonErrorMsg(400, "bad request")
	}
	args := []string{}
	if len(lines) > 1 {
		args = lines[1:]
	}
	peel, symrefs := false, false
	var prefixes []string
	for _, arg := range args {
		trimmed := strings.TrimSuffix(arg, "\n")
		switch {
		case trimmed == "peel":
			peel = true
		case trimmed == "symrefs":
			symrefs = true
		case strings.HasPrefix(trimmed, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(trimmed, "ref-prefix "))
		}
	}
	cacheKey := fmt.Sprintf("%d|%d|%d|%d|%s", repo.ID, repo.RefVersion, b2i(peel), b2i(symrefs), strings.Join(prefixes, ","))
	if cached, ok := lsrefsCache.get(cacheKey); ok {
		return Resp{Status: 200, Headers: map[string]string{"content-type": uploadResult}, Body: cached}
	}
	refs, peeled := listRefs(repo)
	var out []pktItem
	for _, ref := range refs {
		if len(prefixes) > 0 {
			match := false
			for _, prefix := range prefixes {
				if strings.HasPrefix(ref.name, prefix) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		line := fmt.Sprintf("%s %s", ref.oid, ref.name)
		if symrefs && ref.name == "HEAD" {
			line += " symref-target:refs/heads/" + repo.DefaultBranch
		}
		if peel {
			if peeledOID, ok := peeled[ref.name]; ok {
				line += " peeled:" + peeledOID
			}
		}
		out = append(out, pktData(line+"\n"))
	}
	out = append(out, pktFlushIt())
	responseBody := encodePktItems(out)
	lsrefsCache.put(cacheKey, responseBody)
	return Resp{Status: 200, Headers: map[string]string{"content-type": uploadResult}, Body: responseBody}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func handleFetchV2(repo *Repo, body []byte) Resp {
	lines, _, err := parseRequestLines(body)
	if err != nil {
		return jsonErrorMsg(400, "bad request")
	}
	args := []string{}
	if len(lines) > 1 {
		args = lines[1:]
	}
	var wants, haves, shallow []string
	var deepen *int
	var deepenSince *int64
	var deepenNot []string
	for _, arg := range args {
		trimmed := strings.TrimSuffix(arg, "\n")
		switch {
		case strings.HasPrefix(trimmed, "want "):
			wants = append(wants, strings.Split(trimmed[5:], " ")[0])
		case strings.HasPrefix(trimmed, "have "):
			haves = append(haves, strings.TrimSpace(trimmed[5:]))
		case strings.HasPrefix(trimmed, "shallow "):
			shallow = append(shallow, strings.TrimSpace(trimmed[8:]))
		case strings.HasPrefix(trimmed, "deepen "):
			n, _ := strconv.Atoi(trimmed[7:])
			deepen = &n
		case strings.HasPrefix(trimmed, "deepen-since "):
			n, _ := strconv.ParseInt(trimmed[13:], 10, 64)
			deepenSince = &n
		case strings.HasPrefix(trimmed, "deepen-not "):
			deepenNot = append(deepenNot, strings.TrimSpace(trimmed[11:]))
		}
	}
	cacheable := len(haves) == 0 && len(deepenNot) == 0
	cacheKey := ""
	if cacheable {
		cacheKey = fetchCacheKey("v2", repo, wants, shallow, deepen, deepenSince)
		if cached, ok := fetchCache.get(cacheKey); ok {
			return Resp{Status: 200, Headers: map[string]string{"content-type": uploadResult}, Body: cached}
		}
	}
	plan := planFetch(repo, wants, haves, shallow, deepen, deepenSince, deepenNot)
	var parts [][]byte
	if len(haves) > 0 {
		var ackItems []pktItem
		ackItems = append(ackItems, pktData("acknowledgments\n"))
		if len(plan.acks) == 0 {
			ackItems = append(ackItems, pktData("NAK\n"))
		} else {
			for _, ack := range plan.acks {
				ackItems = append(ackItems, pktData("ACK "+ack+"\n"))
			}
			ackItems = append(ackItems, pktData("ready\n"))
		}
		parts = append(parts, encodePktItems(ackItems), pktDelim)
	}
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		var infoItems []pktItem
		infoItems = append(infoItems, pktData("shallow-info\n"))
		for _, oid := range plan.shallow {
			infoItems = append(infoItems, pktData("shallow "+oid+"\n"))
		}
		for _, oid := range plan.unshallow {
			infoItems = append(infoItems, pktData("unshallow "+oid+"\n"))
		}
		parts = append(parts, encodePktItems(infoItems), pktDelim)
	}
	pack := writePack(plan.objects)
	parts = append(parts, encodePktLine([]byte("packfile\n")), sidebandPack(pack))
	responseBody := bytes.Join(parts, nil)
	if cacheable {
		fetchCache.put(cacheKey, responseBody)
	}
	return Resp{Status: 200, Headers: map[string]string{"content-type": uploadResult}, Body: responseBody}
}

func handleFetchV0(repo *Repo, body []byte) Resp {
	packets, err := decodePktLines(body)
	if err != nil {
		return jsonErrorMsg(400, "bad request")
	}
	var wants, haves, shallow []string
	var deepen *int
	var deepenSince *int64
	var deepenNot []string
	section := 0
	for _, packet := range packets {
		if packet.Kind == PktFlush {
			section++
			continue
		}
		if packet.Kind != PktData {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		if section == 0 {
			switch {
			case strings.HasPrefix(line, "want "):
				wants = append(wants, strings.Split(line[5:], " ")[0])
			case strings.HasPrefix(line, "shallow "):
				shallow = append(shallow, strings.TrimSpace(line[8:]))
			case strings.HasPrefix(line, "deepen "):
				n, _ := strconv.Atoi(line[7:])
				deepen = &n
			case strings.HasPrefix(line, "deepen-since "):
				n, _ := strconv.ParseInt(line[13:], 10, 64)
				deepenSince = &n
			case strings.HasPrefix(line, "deepen-not "):
				deepenNot = append(deepenNot, strings.TrimSpace(line[11:]))
			}
		} else {
			if strings.HasPrefix(line, "have ") {
				haves = append(haves, strings.TrimSpace(line[5:]))
			}
		}
	}
	cacheable := len(haves) == 0 && len(deepenNot) == 0
	cacheKey := ""
	if cacheable {
		cacheKey = fetchCacheKey("v0", repo, wants, shallow, deepen, deepenSince)
		if cached, ok := fetchCache.get(cacheKey); ok {
			return Resp{Status: 200, Headers: map[string]string{"content-type": uploadResult}, Body: cached}
		}
	}
	plan := planFetch(repo, wants, haves, shallow, deepen, deepenSince, deepenNot)
	var headLines []string
	if len(haves) > 0 {
		if len(plan.acks) == 0 {
			headLines = append(headLines, "NAK")
		} else {
			for _, ack := range plan.acks {
				headLines = append(headLines, "ACK "+ack)
			}
		}
	} else {
		headLines = append(headLines, "NAK")
	}
	for _, oid := range plan.shallow {
		headLines = append(headLines, "shallow "+oid)
	}
	for _, oid := range plan.unshallow {
		headLines = append(headLines, "unshallow "+oid)
	}
	var headItems []pktItem
	for _, line := range headLines {
		headItems = append(headItems, pktData(line+"\n"))
	}
	pack := writePack(plan.objects)
	responseBody := bytes.Join([][]byte{encodePktItems(headItems), sidebandPack(pack)}, nil)
	if cacheable {
		fetchCache.put(cacheKey, responseBody)
	}
	return Resp{Status: 200, Headers: map[string]string{"content-type": uploadResult}, Body: responseBody}
}

func handleUploadPack(repo *Repo, ctx *Ctx) Resp {
	isV2 := ctx.Headers["git-protocol"] == "version=2"
	if isV2 {
		lines, _, err := parseRequestLines(ctx.Body)
		if err != nil {
			return jsonErrorMsg(400, "bad request")
		}
		command := ""
		if len(lines) > 0 {
			command = strings.TrimSuffix(lines[0], "\n")
		}
		switch command {
		case "command=ls-refs":
			return handleLsRefs(repo, ctx.Body)
		case "command=fetch":
			return handleFetchV2(repo, ctx.Body)
		default:
			return textResp(400, "unknown v2 command", "text/plain", nil)
		}
	}
	return handleFetchV0(repo, ctx.Body)
}

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func handleReceivePack(repo *Repo, ctx *Ctx) Resp {
	var commands []pushCommand
	packStart := -1
	offset := 0
	body := ctx.Body
	for offset < len(body) {
		if offset+4 > len(body) {
			break
		}
		head := string(body[offset : offset+4])
		if head == "0000" {
			packStart = offset + 4
			break
		}
		length, err := strconv.ParseUint(head, 16, 32)
		if err != nil || length < 4 || offset+int(length) > len(body) {
			break
		}
		payload := string(body[offset+4 : offset+int(length)])
		nul := strings.Index(payload, "\x00")
		commandText := payload
		if nul >= 0 {
			commandText = payload[:nul]
		}
		commandText = strings.TrimSuffix(commandText, "\n")
		parts := strings.Split(commandText, " ")
		if len(parts) == 3 && isObjectID(parts[0]) && isObjectID(parts[1]) {
			commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
		}
		offset += int(length)
	}

	var reportLines []string
	unpackOK := true
	unpackDetail := "ok"
	if packStart >= 0 && packStart < len(body) {
		packBytes := body[packStart:]
		read, err := readPack(packBytes, func(oid string) *GitObject { return repo.Lookup(oid) })
		if err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			for oid, object := range read.Objects {
				if repo.Lookup(oid) == nil {
					repo.Objects[oid] = object
				}
			}
		}
	}
	if !unpackOK {
		reportLines = append(reportLines, "unpack error "+unpackDetail)
		for _, cmd := range commands {
			reportLines = append(reportLines, "ng "+cmd.ref+" unpacker error")
		}
	} else {
		reportLines = append(reportLines, "unpack ok")
		for _, cmd := range commands {
			current, hasCurrent := repo.Refs.Get(cmd.ref)
			if cmd.newOID == ZeroOID {
				if !hasCurrent {
					reportLines = append(reportLines, "ng "+cmd.ref+" remote ref does not exist")
				} else if cmd.oldOID != ZeroOID && cmd.oldOID != current {
					reportLines = append(reportLines, "ng "+cmd.ref+" non-fast-forward")
				} else {
					repo.Refs.Delete(cmd.ref)
					reportLines = append(reportLines, "ok "+cmd.ref)
				}
				continue
			}
			newObject := repo.Lookup(cmd.newOID)
			if newObject == nil {
				reportLines = append(reportLines, "ng "+cmd.ref+" unpacker error")
				continue
			}
			newCommit := cmd.newOID
			if newObject.Type == ObjTag {
				newCommit = peelToCommit(repo, cmd.newOID)
			}
			if newObject.Type != ObjCommit && newCommit == "" {
				reportLines = append(reportLines, "ng "+cmd.ref+" object is not a commit")
				continue
			}
			if !hasCurrent {
				if cmd.oldOID != ZeroOID {
					reportLines = append(reportLines, "ng "+cmd.ref+" remote ref does not exist")
					continue
				}
				repo.Refs.Set(cmd.ref, cmd.newOID)
				reportLines = append(reportLines, "ok "+cmd.ref)
				continue
			}
			if cmd.oldOID != current {
				reportLines = append(reportLines, "ng "+cmd.ref+" non-fast-forward")
				continue
			}
			if cmd.newOID == current {
				reportLines = append(reportLines, "ok "+cmd.ref)
				continue
			}
			target := newCommit
			if !isAncestor(current, target, func(oid string) *GitObject { return repo.Lookup(oid) }) {
				reportLines = append(reportLines, "ng "+cmd.ref+" non-fast-forward")
				continue
			}
			repo.Refs.Set(cmd.ref, cmd.newOID)
			reportLines = append(reportLines, "ok "+cmd.ref)
		}
	}
	return Resp{Status: 200, Headers: map[string]string{"content-type": receiveResult}, Body: sidebandTextLines(reportLines)}
}

func handleGit(ctx *Ctx, store *Store, token string) *Resp {
	segments := splitPath(ctx.Path)
	if len(segments) < 2 || !strings.HasSuffix(segments[1], ".git") {
		return nil
	}
	owner := segments[0]
	repoName := strings.TrimSuffix(segments[1], ".git")
	tail := segments[2:]

	if token != "" && bearerToken(ctx) != token {
		r := jsonErrorMsg(401, "Bad credentials")
		return &r
	}

	repo := store.GetRepo(owner, repoName)
	if repo == nil || repo.Owner != VIEWER.Login {
		r := jsonErrorMsg(404, "Not Found")
		return &r
	}

	if ctx.Method == "GET" && len(tail) == 2 && tail[0] == "info" && tail[1] == "refs" {
		service := ctx.Query.Get("service")
		if service == "git-upload-pack" {
			isV2 := ctx.Headers["git-protocol"] == "version=2"
			if isV2 {
				return &Resp{Status: 200, Headers: map[string]string{"content-type": uploadAd}, Body: v2Advertisement}
			}
			return &Resp{Status: 200, Headers: map[string]string{"content-type": uploadAd}, Body: v0Advertisement(repo, "git-upload-pack")}
		}
		if service == "git-receive-pack" {
			return &Resp{Status: 200, Headers: map[string]string{"content-type": receiveAd}, Body: v0Advertisement(repo, "git-receive-pack")}
		}
		r := jsonErrorMsg(400, "service is required")
		return &r
	}

	if ctx.Method == "POST" && len(tail) == 1 && tail[0] == "git-upload-pack" {
		r := handleUploadPack(repo, ctx)
		return &r
	}
	if ctx.Method == "POST" && len(tail) == 1 && tail[0] == "git-receive-pack" {
		r := handleReceivePack(repo, ctx)
		return &r
	}
	r := jsonErrorMsg(404, "Not Found")
	return &r
}
