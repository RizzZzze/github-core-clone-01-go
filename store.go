package main

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Git object model
// ---------------------------------------------------------------------------

// Object type codes match the pack format: commit=1, tree=2, blob=3, tag=4.
type ObjType byte

const (
	ObjCommit ObjType = 1
	ObjTree   ObjType = 2
	ObjBlob   ObjType = 3
	ObjTag    ObjType = 4
)

// ZeroOID is the all-zero object id: unborn refs and delete commands.
const ZeroOID = "0000000000000000000000000000000000000000"

// EmptyTreeOID is the SHA-1 of an empty tree object.
const EmptyTreeOID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// FixedTime is the author/committer timestamp baked into every object this
// server creates, so SHAs are reproducible across runs and replays.
const FixedTime = int64(1700000000)

// FixedIdent is the author/committer line used for server-created objects.
const FixedIdent = "forge <forge@example.invalid> 1700000000 +0000"

func typeName(t ObjType) string {
	switch t {
	case ObjCommit:
		return "commit"
	case ObjTree:
		return "tree"
	case ObjBlob:
		return "blob"
	case ObjTag:
		return "tag"
	}
	return "unknown"
}

func typeCode(name string) (ObjType, bool) {
	switch name {
	case "commit":
		return ObjCommit, true
	case "tree":
		return ObjTree, true
	case "blob":
		return ObjBlob, true
	case "tag":
		return ObjTag, true
	}
	return 0, false
}

// Obj is one stored git object. Commit and tree payloads are parsed once at
// store time so fetch closure walks and ref checks never re-parse bytes.
type Obj struct {
	typ  ObjType
	data []byte
	// commit fields (parsed when typ == ObjCommit)
	tree       string
	parents    []string
	commitTime int64
	// tree fields (parsed when typ == ObjTree)
	entries []TreeEntry
	// tag fields (parsed when typ == ObjTag)
	tagTarget string
	tagType   ObjType
}

// TreeEntry is one entry of a parsed tree object.
type TreeEntry struct {
	mode  string
	name  string
	oid   string
	isDir bool
}

// hashObject returns the git SHA-1 id of an object of the given type.
func hashObject(typ ObjType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typeName(typ)))
	h.Write([]byte{' '})
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// buildCommit renders a commit object body. Message is used verbatim.
func buildCommit(tree string, parents []string, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + FixedIdent + "\n")
	b.WriteString("committer " + FixedIdent + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// buildTag renders an annotated tag object body.
func buildTag(target string, typ ObjType, name string, message string) []byte {
	var b strings.Builder
	b.WriteString("object " + target + "\n")
	b.WriteString("type " + typeName(typ) + "\n")
	b.WriteString("tag " + name + "\n")
	b.WriteString("tagger " + FixedIdent + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// buildTree renders a tree object body from entries sorted the way git
// sorts them: byte-wise by name, directories compared as name + "/".
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		ni, nj := sorted[i].name, sorted[j].name
		if sorted[i].isDir {
			ni += "/"
		}
		if sorted[j].isDir {
			nj += "/"
		}
		return ni < nj
	})
	var out []byte
	for _, e := range sorted {
		out = append(out, []byte(e.mode+" "+e.name+"\x00")...)
		raw, _ := hex.DecodeString(e.oid)
		out = append(out, raw...)
	}
	return out
}

// parseTree decodes a tree object body into entries.
func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	i := 0
	for i < len(data) {
		sp := indexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := indexByte(data[i:], 0)
		if nul < 0 || i+nul+20 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		i += nul + 1
		oid := hex.EncodeToString(data[i : i+20])
		i += 20
		entries = append(entries, TreeEntry{mode: mode, name: name, oid: oid, isDir: mode == "40000"})
	}
	return entries
}

// parseCommit extracts tree, parents and committer time from a commit body.
func parseCommit(data []byte) (tree string, parents []string, commitTime int64) {
	commitTime = FixedTime
	s := string(data)
	// Headers end at the first blank line.
	head := s
	if idx := strings.Index(s, "\n\n"); idx >= 0 {
		head = s[:idx]
	}
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, "tree ") {
			tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimSpace(line[7:]))
		} else if strings.HasPrefix(line, "committer ") {
			fields := strings.Fields(line[len("committer "):])
			if len(fields) >= 2 {
				if ts, err := strconv.ParseInt(fields[len(fields)-2], 10, 64); err == nil {
					commitTime = ts
				}
			}
		}
	}
	return tree, parents, commitTime
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// storeObject hashes, parses and stores an object in the repo, returning its
// id. Storing an object that already exists is a no-op returning its id.
func (r *Repo) storeObject(typ ObjType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.Objects[oid]; ok {
		return oid
	}
	obj := &Obj{typ: typ, data: data}
	switch typ {
	case ObjCommit:
		obj.tree, obj.parents, obj.commitTime = parseCommit(data)
	case ObjTree:
		obj.entries = parseTree(data)
	case ObjTag:
		s := string(data)
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, "object ") {
				obj.tagTarget = strings.TrimSpace(line[7:])
			} else if strings.HasPrefix(line, "type ") {
				if t, ok := typeCode(strings.TrimSpace(line[5:])); ok {
					obj.tagType = t
				}
			}
			if line == "" {
				break
			}
		}
	}
	r.Objects[oid] = obj
	return oid
}

// ---------------------------------------------------------------------------
// Issues, pull requests, comments, labels
// ---------------------------------------------------------------------------

type Comment struct {
	ID       int
	Body     string
	RepoID   int // owning repo id (for global ids)
	IssueNum int // owning issue number (for global ids)
}

type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []string
	CreatedSeq int
	Comments   []*Comment
}

type PRFile struct {
	Filename  string
	Status    string // "added" | "modified"
	Additions int
	Deletions int
	Changes   int
}

type Pull struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Merged     bool
	MergedAt   string
	MergeSHA   string
	HeadRef    string
	BaseRef    string
	HeadSHA    string
	BaseSHA    string
	BaseAtOpen string // base tip recorded at creation, for conflict checks
	CreatedSeq int
	Files      []PRFile
}

type Label struct {
	Name  string
	Color string
}

// ---------------------------------------------------------------------------
// Repository and store
// ---------------------------------------------------------------------------

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Private       bool
	Description   string
	DefaultBranch string
	Refs          map[string]string // full ref name -> oid
	Objects       map[string]*Obj
	Issues        map[int]*Issue
	IssueOrder    []*Issue
	Pulls         map[int]*Pull
	PullOrder     []*Pull
	Labels        map[string]*Label
	LabelOrder    []*Label
	NextNum       int // next shared issue/PR number
	NextCommentID int
	CreatedSeq    int
}

type Store struct {
	Repos  map[string]*Repo // "owner/name" -> repo
	Order  []*Repo          // creation order, for pagination
	NextID int
	Seq    int
}

func NewStore() *Store {
	return &Store{Repos: make(map[string]*Repo), NextID: 1}
}

func repoKey(owner, name string) string { return owner + "/" + name }

func (s *Store) GetRepo(owner, name string) *Repo {
	return s.Repos[repoKey(owner, name)]
}

func (s *Store) CreateRepo(owner, name string, private bool, description string) *Repo {
	s.Seq++
	s.NextID++
	r := &Repo{
		ID:            s.NextID - 1,
		Owner:         owner,
		Name:          name,
		Private:       private,
		Description:   description,
		DefaultBranch: "main",
		Refs:          make(map[string]string),
		Objects:       make(map[string]*Obj),
		Issues:        make(map[int]*Issue),
		Pulls:         make(map[int]*Pull),
		Labels:        make(map[string]*Label),
		NextNum:       1,
		NextCommentID: 1,
		CreatedSeq:    s.Seq,
	}
	s.Repos[repoKey(owner, name)] = r
	s.Order = append(s.Order, r)
	return r
}

func (s *Store) DeleteRepo(owner, name string) {
	key := repoKey(owner, name)
	r := s.Repos[key]
	if r == nil {
		return
	}
	delete(s.Repos, key)
	for i, x := range s.Order {
		if x == r {
			s.Order = append(s.Order[:i], s.Order[i+1:]...)
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Refs, trees, history
// ---------------------------------------------------------------------------

// headRef returns the full ref name of the default branch.
func (r *Repo) headRef() string { return "refs/heads/" + r.DefaultBranch }

// headOID returns the default branch tip, or "" when the repo is unborn.
func (r *Repo) headOID() string { return r.Refs[r.headRef()] }

// refOID returns the oid a ref points at, or "" when it does not exist.
func (r *Repo) refOID(ref string) string { return r.Refs[ref] }

// branchNames returns branch short names sorted alphabetically.
func (r *Repo) branchNames() []string {
	var out []string
	for ref := range r.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			out = append(out, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(out)
	return out
}

// tagNames returns tag short names sorted alphabetically.
func (r *Repo) tagNames() []string {
	var out []string
	for ref := range r.Refs {
		if strings.HasPrefix(ref, "refs/tags/") {
			out = append(out, strings.TrimPrefix(ref, "refs/tags/"))
		}
	}
	sort.Strings(out)
	return out
}

// isAncestor reports whether anc is reachable from desc by following parent
// links (a commit is its own ancestor). Missing objects stop the walk.
func (r *Repo) isAncestor(anc, desc string) bool {
	if anc == "" || desc == "" {
		return false
	}
	if anc == desc {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{desc}
	steps := 0
	for len(stack) > 0 {
		steps++
		if steps > 100000 {
			return false
		}
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.Objects[cur]
		if obj == nil || obj.typ != ObjCommit {
			continue
		}
		stack = append(stack, obj.parents...)
	}
	return false
}

// commitTree returns the tree oid of a commit, or "" when unknown.
func (r *Repo) commitTree(oid string) string {
	obj := r.Objects[oid]
	if obj == nil || obj.typ != ObjCommit {
		return ""
	}
	return obj.tree
}

// updateTree applies one path change to a tree (recursively) and returns the
// new tree oid. blobOID == "" deletes the path. Intermediate trees are
// created as needed. An empty resulting tree returns EmptyTreeOID.
func (r *Repo) updateTree(baseTreeOID string, parts []string, blobOID string) string {
	if len(parts) == 0 {
		return baseTreeOID
	}
	var entries []TreeEntry
	if baseTreeOID != "" && baseTreeOID != EmptyTreeOID {
		if obj := r.Objects[baseTreeOID]; obj != nil && obj.typ == ObjTree {
			entries = append(entries, obj.entries...)
		}
	}
	name := parts[0]
	last := len(parts) == 1
	found := -1
	for i, e := range entries {
		if e.name == name {
			found = i
			break
		}
	}
	if last {
		if blobOID == "" {
			if found >= 0 {
				entries = append(entries[:found], entries[found+1:]...)
			}
		} else {
			ne := TreeEntry{mode: "100644", name: name, oid: blobOID}
			if found >= 0 {
				entries[found] = ne
			} else {
				entries = append(entries, ne)
			}
		}
	} else {
		var sub string
		if found >= 0 {
			sub = entries[found].oid
		}
		newSub := r.updateTree(sub, parts[1:], blobOID)
		if newSub == EmptyTreeOID {
			if found >= 0 {
				entries = append(entries[:found], entries[found+1:]...)
			}
		} else {
			ne := TreeEntry{mode: "40000", name: name, oid: newSub, isDir: true}
			if found >= 0 {
				entries[found] = ne
			} else {
				entries = append(entries, ne)
			}
		}
	}
	if len(entries) == 0 {
		return EmptyTreeOID
	}
	return r.storeObject(ObjTree, buildTree(entries))
}

// treeGet returns the blob oid at path in a tree, or "" when absent.
func (r *Repo) treeGet(treeOID string, parts []string) string {
	if len(parts) == 0 || treeOID == "" {
		return ""
	}
	obj := r.Objects[treeOID]
	if obj == nil || obj.typ != ObjTree {
		return ""
	}
	for _, e := range obj.entries {
		if e.name != parts[0] {
			continue
		}
		if len(parts) == 1 {
			if e.isDir {
				return ""
			}
			return e.oid
		}
		if !e.isDir {
			return ""
		}
		return r.treeGet(e.oid, parts[1:])
	}
	return ""
}

// treeEntriesFlat lists every file path in a tree (recursive), with blob oids.
func (r *Repo) treeEntriesFlat(treeOID, prefix string, out *[]string, blobs *map[string]string) {
	obj := r.Objects[treeOID]
	if obj == nil || obj.typ != ObjTree {
		return
	}
	for _, e := range obj.entries {
		p := prefix + e.name
		if e.isDir {
			r.treeEntriesFlat(e.oid, p+"/", out, blobs)
		} else {
			*out = append(*out, p)
			(*blobs)[p] = e.oid
		}
	}
}

// commitChain returns commits reachable from tip, newest first, following
// first parents only for listing order (linear histories in the suite).
func (r *Repo) commitChain(tip string, limit int) []string {
	var out []string
	cur := tip
	seen := make(map[string]bool)
	for cur != "" && len(out) < limit {
		if seen[cur] {
			break
		}
		seen[cur] = true
		obj := r.Objects[cur]
		if obj == nil || obj.typ != ObjCommit {
			break
		}
		out = append(out, cur)
		if len(obj.parents) == 0 {
			break
		}
		cur = obj.parents[0]
	}
	return out
}

// ---------------------------------------------------------------------------
// Issue / PR helpers
// ---------------------------------------------------------------------------

// nextNumber assigns the next shared issue/PR number for the repo.
func (r *Repo) nextNumber() int {
	n := r.NextNum
	r.NextNum++
	return n
}

// newIssue creates an issue with the given labels (names must already exist
// as labels on the repo; unknown names are the caller's problem).
func (s *Store) newIssue(r *Repo, title, body string, labels []string) *Issue {
	s.Seq++
	s.NextID++
	iss := &Issue{
		ID:         s.NextID - 1,
		Number:     r.nextNumber(),
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     append([]string(nil), labels...),
		CreatedSeq: s.Seq,
	}
	r.Issues[iss.Number] = iss
	r.IssueOrder = append(r.IssueOrder, iss)
	return iss
}

// newPull creates a pull request. headSHA/baseSHA are the branch tips.
func (s *Store) newPull(r *Repo, title, body, headRef, baseRef, headSHA, baseSHA string, files []PRFile) *Pull {
	s.Seq++
	s.NextID++
	pr := &Pull{
		ID:         s.NextID - 1,
		Number:     r.nextNumber(),
		Title:      title,
		Body:       body,
		State:      "open",
		HeadRef:    headRef,
		BaseRef:    baseRef,
		HeadSHA:    headSHA,
		BaseSHA:    baseSHA,
		BaseAtOpen: baseSHA,
		CreatedSeq: s.Seq,
		Files:      files,
	}
	r.Pulls[pr.Number] = pr
	r.PullOrder = append(r.PullOrder, pr)
	return pr
}

// seedLabels installs the six standard labels on a fresh repo.
func (r *Repo) seedLabels() {
	for _, lb := range []Label{
		{Name: "bug", Color: "d73a4a"},
		{Name: "enhancement", Color: "a2eeef"},
		{Name: "documentation", Color: "0075ca"},
		{Name: "question", Color: "d876e3"},
		{Name: "good first issue", Color: "7057ff"},
		{Name: "help wanted", Color: "008672"},
	} {
		l := lb
		r.Labels[l.Name] = &l
		r.LabelOrder = append(r.LabelOrder, &l)
	}
}
