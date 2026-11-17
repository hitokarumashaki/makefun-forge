// Git object model: SHA-1 ids, loose-object bytes, trees, commits, tags,
// pkt-lines and packfiles (plain objects, plus OFS/REF delta reading).
// No git binary, no outside service: everything is the standard library.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const ZeroOID = "0000000000000000000000000000000000000000"

func isObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

type GitObjectType string

const (
	ObjCommit GitObjectType = "commit"
	ObjTree   GitObjectType = "tree"
	ObjBlob   GitObjectType = "blob"
	ObjTag    GitObjectType = "tag"
)

type GitObject struct {
	Type GitObjectType
	Data []byte
}

func hashObject(typ GitObjectType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typ, len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			break
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			break
		}
		nul += space + 1
		mode := string(data[offset:space])
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		offset = nul + 21
	}
	return entries
}

func compareTreeNames(a, b TreeEntry) int {
	an := a.Name
	if a.Mode == "40000" {
		an += "/"
	}
	bn := b.Name
	if b.Mode == "40000" {
		bn += "/"
	}
	return bytes.Compare([]byte(an), []byte(bn))
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) CommitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headerText := string(data[:headerEnd])
	message := ""
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	v := CommitView{}
	for _, line := range strings.Split(headerText, "\n") {
		if line == "" {
			continue
		}
		space := strings.Index(line, " ")
		key, value := line, ""
		if space >= 0 {
			key, value = line[:space], line[space+1:]
		}
		switch key {
		case "tree":
			v.Tree = value
		case "parent":
			v.Parents = append(v.Parents, value)
		case "author":
			v.Author = value
		case "committer":
			v.Committer = value
		}
	}
	v.Message = message
	return v
}

func buildCommit(tree string, parents []string, message, author string, committer string, timestamp int64) []byte {
	var lines []string
	lines = append(lines, "tree "+tree)
	for _, p := range parents {
		lines = append(lines, "parent "+p)
	}
	who := fmt.Sprintf("%s %d +0000", author, timestamp)
	if committer == "" {
		committer = author
	}
	c := fmt.Sprintf("%s %d +0000", committer, timestamp)
	lines = append(lines, "author "+who, "committer "+c, "")
	return []byte(strings.Join(lines, "\n") + "\n" + message)
}

type TagView struct {
	Object  string
	Type    GitObjectType
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) TagView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headers := map[string]string{}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		space := strings.Index(line, " ")
		if space < 0 {
			continue
		}
		headers[line[:space]] = line[space+1:]
	}
	msg := ""
	if blank >= 0 {
		msg = string(data[blank+2:])
	}
	typ := GitObjectType(headers["type"])
	if typ == "" {
		typ = ObjCommit
	}
	return TagView{
		Object:  headers["object"],
		Type:    typ,
		Tag:     headers["tag"],
		Tagger:  headers["tagger"],
		Message: msg,
	}
}

func buildTag(object string, typ GitObjectType, tag, message, tagger string, timestamp int64) []byte {
	return []byte(fmt.Sprintf("object %s\ntype %s\ntag %s\ntagger %s %d +0000\n\n%s",
		object, typ, tag, tagger, timestamp, message))
}

// ---------------------------------------------------------------------------------------------
// pkt-lines
// ---------------------------------------------------------------------------------------------

type PktKind int

const (
	PktData PktKind = iota
	PktFlush
	PktDelim
	PktResponseEnd
)

type Pkt struct {
	Kind    PktKind
	Payload []byte
}

func encodePktLine(payload []byte) []byte {
	out := make([]byte, 4, 4+len(payload))
	copy(out, []byte(fmt.Sprintf("%04x", len(payload)+4)))
	return append(out, payload...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

func encodePktLines(items [][]byte) []byte {
	var buf bytes.Buffer
	for _, item := range items {
		buf.Write(encodePktLine(item))
	}
	return buf.Bytes()
}

// encodePktItems encodes strings/buffers plus flush/delim markers, mirroring
// the TypeScript encodePktLines(items: (string|Buffer|'flush'|'delim')[]).
type pktItem struct {
	kind byte // 'd' data, 'f' flush, 'e' delim
	data []byte
}

func encodePktItems(items []pktItem) []byte {
	var buf bytes.Buffer
	for _, item := range items {
		switch item.kind {
		case 'f':
			buf.Write(pktFlush)
		case 'e':
			buf.Write(pktDelim)
		default:
			buf.Write(encodePktLine(item.data))
		}
	}
	return buf.Bytes()
}

func pktData(s string) pktItem  { return pktItem{kind: 'd', data: []byte(s)} }
func pktBytes(b []byte) pktItem { return pktItem{kind: 'd', data: b} }
func pktFlushIt() pktItem       { return pktItem{kind: 'f'} }
func pktDelimIt() pktItem       { return pktItem{kind: 'e'} }

// decodePktLines decodes a whole buffer of pkt-lines. Errors on malformed frames.
func decodePktLines(b []byte) ([]Pkt, error) {
	var packets []Pkt
	offset := 0
	for offset < len(b) {
		if offset+4 > len(b) {
			return nil, errors.New("truncated pkt-line header")
		}
		head := string(b[offset : offset+4])
		if head == "0000" {
			packets = append(packets, Pkt{Kind: PktFlush})
			offset += 4
			continue
		}
		if head == "0001" {
			packets = append(packets, Pkt{Kind: PktDelim})
			offset += 4
			continue
		}
		if head == "0002" {
			packets = append(packets, Pkt{Kind: PktResponseEnd})
			offset += 4
			continue
		}
		length64, err := strconv.ParseUint(head, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("bad pkt-line length %q", head)
		}
		length := int(length64)
		if length < 4 || offset+length > len(b) {
			return nil, fmt.Errorf("bad pkt-line length %q", head)
		}
		packets = append(packets, Pkt{Kind: PktData, Payload: b[offset+4 : offset+length]})
		offset += length
	}
	return packets, nil
}

// demuxSideband splits a sideband-framed pack stream into band-1 bytes and band-2 text.
func demuxSideband(packets []Pkt) ([]byte, string, error) {
	var chunks [][]byte
	progress := ""
	for _, p := range packets {
		if p.Kind != PktData || len(p.Payload) == 0 {
			continue
		}
		band := p.Payload[0]
		data := p.Payload[1:]
		switch band {
		case 1:
			chunks = append(chunks, data)
		case 2:
			progress += string(data)
		case 3:
			return nil, "", fmt.Errorf("remote sideband error: %s", strings.TrimSpace(string(data)))
		}
	}
	return bytes.Join(chunks, nil), progress, nil
}

// ---------------------------------------------------------------------------------------------
// packfiles
// ---------------------------------------------------------------------------------------------

var typeCode = map[GitObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]GitObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func writePackObjectHeader(typ GitObjectType, size int) []byte {
	code := typeCode[typ]
	var out []byte
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining = remaining / 16
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining = remaining / 128
	}
	out = append(out, byte(first))
	return out
}

// writePack builds a packfile of plain (undeltified) objects, version 2, with a SHA-1 trailer.
func writePack(objects []GitObject) []byte {
	var parts bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:], 2)
	binary.BigEndian.PutUint32(header[8:], uint32(len(objects)))
	parts.Write(header)
	for _, obj := range objects {
		parts.Write(writePackObjectHeader(obj.Type, len(obj.Data)))
		var zb bytes.Buffer
		w := zlib.NewWriter(&zb)
		w.Write(obj.Data)
		w.Close()
		parts.Write(zb.Bytes())
	}
	body := parts.Bytes()
	sum := sha1.Sum(body)
	out := make([]byte, 0, len(body)+20)
	out = append(out, body...)
	out = append(out, sum[:]...)
	return out
}

func readVarintSize(buffer []byte, start int) (size int, next int, first byte) {
	b := buffer[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		b = buffer[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, offset, first
}

func readOfsDeltaBase(buffer []byte, start int) (baseOffset int, next int) {
	b := buffer[start]
	offset := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		b = buffer[offset]
		offset++
		value = ((value + 1) * 128) + int(b&0x7f)
	}
	return value, offset
}

func inflateAt(buffer []byte, offset int) (data []byte, next int, err error) {
	// bytes.Reader is an io.ByteReader, so flate reads exactly the bytes the
	// stream needs and no further: the reader position after ReadAll is the
	// exact end of the zlib stream.
	br := bytes.NewReader(buffer[offset:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	data, err = io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	if len(data) == 0 {
		return nil, 0, errors.New("zlib stream consumed no bytes")
	}
	consumed := len(buffer[offset:]) - br.Len()
	return data, offset + consumed, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() int {
		value := 0
		shift := uint(0)
		for {
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	sourceSize := readSize()
	targetSize := readSize()
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			copy(out[written:written+literal], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

type PackReadResult struct {
	Version       int
	DeclaredCount int
	Objects       map[string]GitObject
	DeltaCount    int
}

type rawEntry struct {
	offset       int
	typeCode     int
	size         int
	data         []byte
	baseOffset   int
	baseOid      string
	resolvedType GitObjectType
	resolved     []byte
	oid          string
}

// readPack parses a packfile (v2/v3), resolving OFS and REF deltas.
func readPack(pack []byte, resolveBase func(oid string) *GitObject) (*PackReadResult, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(binary.BigEndian.Uint32(pack[4:]))
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(binary.BigEndian.Uint32(pack[8:]))
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	sum := sha1.Sum(pack[:trailerStart])
	if trailer != hex.EncodeToString(sum[:]) {
		return nil, errors.New("pack trailer checksum mismatch")
	}

	raw := make([]*rawEntry, 0, declaredCount)
	byOffset := map[int]*rawEntry{}
	offset := 12
	for i := 0; i < declaredCount; i++ {
		entryOffset := offset
		size, next, first := readVarintSize(pack, offset)
		offset = next
		tc := int((first >> 4) & 0x07)
		entry := &rawEntry{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 {
			base, next2 := readOfsDeltaBase(pack, offset)
			offset = next2
			entry.baseOffset = entryOffset - base
		} else if tc == 7 {
			entry.baseOid = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if _, ok := codeType[tc]; !ok {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", i, tc)
		}
		inflated, nextOff, err := inflateAt(pack[:trailerStart], offset)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		entry.data = inflated
		offset = nextOff
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, errors.New("pack has stray bytes before its trailer")
	}

	objects := map[string]GitObject{}
	resolvedByOid := map[string]*rawEntry{}
	resolving := map[int]bool{}
	var resolve func(entry *rawEntry) (GitObjectType, []byte, string, error)
	resolve = func(entry *rawEntry) (GitObjectType, []byte, string, error) {
		if entry.resolved != nil {
			return entry.resolvedType, entry.resolved, entry.oid, nil
		}
		if resolving[entry.offset] {
			return "", nil, "", fmt.Errorf("delta cycle at pack offset %d", entry.offset)
		}
		resolving[entry.offset] = true
		var typ GitObjectType
		var data []byte
		if entry.typeCode == 6 {
			baseEntry, ok := byOffset[entry.baseOffset]
			if !ok {
				return "", nil, "", errors.New("ofs-delta base is not in the pack")
			}
			bt, bd, _, err := resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			typ = bt
			data, err = applyDelta(bd, entry.data)
			if err != nil {
				return "", nil, "", err
			}
		} else if entry.typeCode == 7 {
			wanted := entry.baseOid
			if baseEntry, ok := resolvedByOid[wanted]; ok {
				bt, bd, _, err := resolve(baseEntry)
				if err != nil {
					return "", nil, "", err
				}
				typ = bt
				data, err = applyDelta(bd, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			} else {
				var external *GitObject
				if resolveBase != nil {
					external = resolveBase(wanted)
				}
				if external == nil {
					return "", nil, "", fmt.Errorf("ref-delta base %s is neither in the pack nor held", wanted)
				}
				typ = external.Type
				var err error
				data, err = applyDelta(external.Data, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			}
		} else {
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		oid := hashObject(typ, data)
		entry.resolved = data
		entry.resolvedType = typ
		entry.oid = oid
		resolvedByOid[oid] = entry
		delete(resolving, entry.offset)
		return typ, data, oid, nil
	}

	deltaCount := 0
	for _, entry := range raw {
		typ, data, oid, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if entry.typeCode == 6 || entry.typeCode == 7 {
			deltaCount++
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = GitObject{Type: typ, Data: data}
		}
	}
	return &PackReadResult{Version: version, DeclaredCount: declaredCount, Objects: objects, DeltaCount: deltaCount}, nil
}

// ---------------------------------------------------------------------------------------------
// reachability
// ---------------------------------------------------------------------------------------------

type objLookup func(oid string) *GitObject

// collectReachable returns every object reachable from roots.
func collectReachable(roots []string, lookup objLookup) (map[string]bool, error) {
	seen := map[string]bool{}
	var queue []string
	for _, root := range roots {
		if root == ZeroOID {
			continue
		}
		if !seen[root] {
			seen[root] = true
			queue = append(queue, root)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		object := lookup(oid)
		if object == nil {
			return nil, fmt.Errorf("missing object %s", oid)
		}
		switch object.Type {
		case ObjCommit:
			commit := parseCommit(object.Data)
			for _, next := range append([]string{commit.Tree}, commit.Parents...) {
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		case ObjTree:
			for _, entry := range parseTree(object.Data) {
				if entry.Mode == "160000" {
					continue
				}
				if !seen[entry.OID] {
					seen[entry.OID] = true
					queue = append(queue, entry.OID)
				}
			}
		case ObjTag:
			tag := parseTag(object.Data)
			if !seen[tag.Object] {
				seen[tag.Object] = true
				queue = append(queue, tag.Object)
			}
		}
	}
	return seen, nil
}

// isAncestor reports whether ancestor is reachable from oid (or equal to it).
func isAncestor(ancestor, oid string, lookup objLookup) bool {
	if ancestor == oid {
		return true
	}
	if ancestor == ZeroOID {
		return true
	}
	seen := map[string]bool{}
	queue := []string{oid}
	for len(queue) > 0 {
		current := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if current == ancestor {
			return true
		}
		if seen[current] {
			continue
		}
		seen[current] = true
		object := lookup(current)
		if object == nil || object.Type != ObjCommit {
			continue
		}
		queue = append(queue, parseCommit(object.Data).Parents...)
	}
	return false
}

// mergeBase returns the first common ancestor found by BFS from both commits.
func mergeBase(a, b string, lookup objLookup) string {
	if a == b {
		return a
	}
	fromA := map[string]bool{}
	queueA := []string{a}
	for len(queueA) > 0 {
		current := queueA[len(queueA)-1]
		queueA = queueA[:len(queueA)-1]
		if fromA[current] {
			continue
		}
		fromA[current] = true
		object := lookup(current)
		if object != nil && object.Type == ObjCommit {
			queueA = append(queueA, parseCommit(object.Data).Parents...)
		}
	}
	queueB := []string{b}
	seenB := map[string]bool{}
	for len(queueB) > 0 {
		current := queueB[len(queueB)-1]
		queueB = queueB[:len(queueB)-1]
		if fromA[current] {
			return current
		}
		if seenB[current] {
			continue
		}
		seenB[current] = true
		object := lookup(current)
		if object != nil && object.Type == ObjCommit {
			queueB = append(queueB, parseCommit(object.Data).Parents...)
		}
	}
	return ""
}

// commitHistory returns commits reachable from tip, newest first (DFS, parents in order).
func commitHistory(tip string, lookup objLookup) []string {
	var out []string
	seen := map[string]bool{}
	var visit func(oid string)
	visit = func(oid string) {
		if seen[oid] || oid == ZeroOID {
			return
		}
		seen[oid] = true
		object := lookup(oid)
		if object == nil || object.Type != ObjCommit {
			return
		}
		out = append(out, oid)
		for _, parent := range parseCommit(object.Data).Parents {
			visit(parent)
		}
	}
	visit(tip)
	return out
}

// flattenTree returns the flat path -> blob-oid map of a tree (recursive).
func flattenTree(treeOid string, lookup objLookup, prefix string) map[string]string {
	out := map[string]string{}
	var walk func(oid, path string)
	walk = func(oid, path string) {
		object := lookup(oid)
		if object == nil || object.Type != ObjTree {
			return
		}
		for _, entry := range parseTree(object.Data) {
			full := entry.Name
			if path != "" {
				full = path + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.OID, full)
			} else if entry.Mode != "160000" {
				out[full] = entry.OID
			}
		}
	}
	walk(treeOid, prefix)
	return out
}

// FileEntry is one file in a flat path -> entry map (for buildTreeFromPaths).
type FileEntry struct {
	Mode string
	OID  string
}

// buildTreeFromPaths builds a tree object from a flat path -> {mode, oid} map.
func buildTreeFromPaths(files map[string]FileEntry, put func(typ GitObjectType, data []byte) string) string {
	type node struct {
		files map[string]FileEntry
		dirs  map[string]*node
	}
	mk := func() *node {
		return &node{
			files: map[string]FileEntry{},
			dirs:  map[string]*node{},
		}
	}
	root := mk()
	for path, file := range files {
		segments := strings.Split(path, "/")
		n := root
		for i := 0; i < len(segments)-1; i++ {
			name := segments[i]
			if _, ok := n.dirs[name]; !ok {
				n.dirs[name] = mk()
			}
			n = n.dirs[name]
		}
		n.files[segments[len(segments)-1]] = file
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []TreeEntry
		for name, file := range n.files {
			entries = append(entries, TreeEntry{Mode: file.Mode, Name: name, OID: file.OID})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return put(ObjTree, buildTree(entries))
	}
	return build(root)
}
