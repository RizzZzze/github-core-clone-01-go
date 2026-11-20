package main

import (
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Fetch planning
// ---------------------------------------------------------------------------

type fetchPlan struct {
	objs      []packObject
	shallow   []string
	unshallow []string
	ack       string // first have found in the store, or ""
}

// reachableFrom returns every object id reachable from the given commits
// (commits, their trees recursively, and blobs), unlimited depth.
func (r *Repo) reachableFrom(tips []string) map[string]bool {
	return r.reachableFromStop(tips, nil)
}

// reachableFromStop is reachableFrom with one refinement: a commit in stop
// is itself (and its tree) held, but its parents are NOT held — that is the
// meaning of a client-side shallow boundary. The walk never descends past
// such a commit.
func (r *Repo) reachableFromStop(tips []string, stop map[string]bool) map[string]bool {
	out := make(map[string]bool)
	var walkTree func(oid string)
	walkTree = func(oid string) {
		if out[oid] {
			return
		}
		obj := r.Objects[oid]
		if obj == nil {
			return
		}
		out[oid] = true
		if obj.typ != ObjTree {
			return
		}
		for _, e := range obj.entries {
			walkTree(e.oid)
		}
	}
	// Iterative commit walk to avoid deep recursion on long histories.
	stack := append([]string(nil), tips...)
	seenCommit := make(map[string]bool)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seenCommit[cur] {
			continue
		}
		seenCommit[cur] = true
		obj := r.Objects[cur]
		if obj == nil {
			continue
		}
		switch obj.typ {
		case ObjCommit:
			out[cur] = true
			walkTree(obj.tree)
			if stop[cur] {
				continue
			}
			stack = append(stack, obj.parents...)
		case ObjTag:
			out[cur] = true
			stack = append(stack, obj.tagTarget)
		default:
			walkTree(cur)
		}
	}
	return out
}

// planFetch computes the objects a fetch must send: the closure of wants
// minus everything reachable from haves. depth > 0 limits commit generations
// (want = generation 1); commits at the limit become the shallow boundary.
// clientShallow lists the client's current shallow commits: one that is now
// fully included (its parents sent) is reported unshallow.
func (r *Repo) planFetch(wants, haves, clientShallow []string, depth int) fetchPlan {
	shallowSet := make(map[string]bool, len(clientShallow))
	for _, b := range clientShallow {
		shallowSet[b] = true
	}
	haveReach := r.reachableFromStop(haves, shallowSet)
	openedShallow := make(map[string]bool)
	plan := fetchPlan{}
	for _, h := range haves {
		if _, ok := r.Objects[h]; ok {
			plan.ack = h
			break
		}
	}
	included := make(map[string]bool)
	isShallow := make(map[string]bool)
	var includeTree func(oid string)
	includeTree = func(oid string) {
		if included[oid] || haveReach[oid] {
			return
		}
		obj := r.Objects[oid]
		if obj == nil {
			return
		}
		included[oid] = true
		plan.objs = append(plan.objs, packObject{typ: obj.typ, data: obj.data})
		if obj.typ == ObjTree {
			for _, e := range obj.entries {
				includeTree(e.oid)
			}
		}
	}
	// includeCommit adds a commit and its whole tree, honouring the depth
	// limit; it is called for wants and, recursively, for their parents.
	var includeCommit func(oid string, gen int)
	includeCommit = func(oid string, gen int) {
		if included[oid] {
			return
		}
		obj := r.Objects[oid]
		if obj == nil {
			return
		}
		// A client-shallow commit is already held (object + tree) but its
		// parents are not: descend into the parents without re-sending it.
		if shallowSet[oid] {
			if obj.typ == ObjCommit {
				if depth > 0 && gen >= depth {
					return
				}
				openedShallow[oid] = true
				for _, p := range obj.parents {
					includeCommit(p, gen+1)
				}
			}
			return
		}
		if haveReach[oid] {
			return
		}
		switch obj.typ {
		case ObjCommit:
			included[oid] = true
			plan.objs = append(plan.objs, packObject{typ: obj.typ, data: obj.data})
			includeTree(obj.tree)
			if depth > 0 && gen >= depth {
				plan.shallow = append(plan.shallow, oid)
				isShallow[oid] = true
				return
			}
			for _, p := range obj.parents {
				includeCommit(p, gen+1)
			}
		case ObjTag:
			included[oid] = true
			plan.objs = append(plan.objs, packObject{typ: obj.typ, data: obj.data})
			if obj.tagTarget != "" {
				if t := r.Objects[obj.tagTarget]; t != nil {
					if t.typ == ObjCommit {
						includeCommit(obj.tagTarget, gen)
					} else {
						includeTree(obj.tagTarget)
					}
				}
			}
		default:
			includeTree(oid)
		}
	}
	for _, w := range wants {
		includeCommit(w, 1)
	}
	for _, b := range clientShallow {
		if (included[b] || openedShallow[b]) && !isShallow[b] {
			if obj := r.Objects[b]; obj != nil && obj.typ == ObjCommit {
				plan.unshallow = append(plan.unshallow, b)
			}
		}
	}
	return plan
}

// ---------------------------------------------------------------------------
// Advertisements
// ---------------------------------------------------------------------------

const agentCap = "agent=forge-go/1.0"

func uploadCapsV0() string {
	return "multi_ack thin-pack side-band side-band-64k ofs-delta shallow " +
		"no-progress include-tag " + agentCap + " symref=HEAD:refs/heads/main"
}

func receiveCapsV0() string {
	return "report-status side-band-64k delete-refs ofs-delta " + agentCap
}

// sortedRefNames returns all ref names in the repo, sorted.
func (r *Repo) sortedRefNames() []string {
	names := make([]string, 0, len(r.Refs))
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// refLines renders v0 advertisement ref lines (without the service
// preamble). The first line carries capabilities after NUL. Peeled tag
// lines follow their tag line. An empty repo yields the capabilities^{}
// pseudo-ref line.
func (r *Repo) refLines(caps string) []byte {
	var out []byte
	names := r.sortedRefNames()
	if len(names) == 0 {
		out = append(out, pktString(ZeroOID+" capabilities^{}\x00"+caps+"\n")...)
		return out
	}
	// HEAD line first (target ref's oid, symref capability carries the name).
	headOID := r.headOID()
	if headOID != "" {
		out = append(out, pktString(headOID+" HEAD\x00"+caps+"\n")...)
	} else {
		out = append(out, pktString(ZeroOID+" capabilities^{}\x00"+caps+"\n")...)
	}
	for _, name := range names {
		oid := r.Refs[name]
		out = append(out, pktString(oid+" "+name+"\n")...)
		// Peeled line for annotated tags.
		if strings.HasPrefix(name, "refs/tags/") {
			if obj := r.Objects[oid]; obj != nil && obj.typ == ObjTag && obj.tagTarget != "" {
				out = append(out, pktString(obj.tagTarget+" "+name+"^{}\n")...)
			}
		}
	}
	return out
}

// advertisementV0 renders the full v0 info/refs body for a service.
func (r *Repo) advertisementV0(service string) []byte {
	var out []byte
	if service == "git-upload-pack" {
		out = append(out, pktString("# service=git-upload-pack\n")...)
		out = append(out, pktFlush()...)
		out = append(out, r.refLines(uploadCapsV0())...)
	} else {
		out = append(out, pktString("# service=git-receive-pack\n")...)
		out = append(out, pktFlush()...)
		out = append(out, r.refLines(receiveCapsV0())...)
	}
	out = append(out, pktFlush()...)
	return out
}

// advertisementV2 renders the v2 upload-pack advertisement body.
func (r *Repo) advertisementV2() []byte {
	var out []byte
	out = append(out, pktString("# service=git-upload-pack\n")...)
	out = append(out, pktFlush()...)
	out = append(out, pktString("version 2\n")...)
	out = append(out, pktString(agentCap+"\n")...)
	out = append(out, pktString("object-format=sha1\n")...)
	out = append(out, pktString("ls-refs=unborn\n")...)
	out = append(out, pktString("fetch=shallow\n")...)
	out = append(out, pktFlush()...)
	return out
}

// ---------------------------------------------------------------------------
// ls-refs (protocol v2)
// ---------------------------------------------------------------------------

// handleLsRefs answers a v2 ls-refs command. args are the parsed request
// lines after the delim (peel, symrefs, unborn, ref-prefix ...).
func (r *Repo) handleLsRefs(args []string) []byte {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(a, "ref-prefix "))
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out []byte
	headOID := r.headOID()
	if len(r.Refs) == 0 {
		if unborn && keep("HEAD") {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:" + r.headRef()
			}
			out = append(out, pktString(line+"\n")...)
		}
		out = append(out, pktFlush()...)
		return out
	}
	if keep("HEAD") && headOID != "" {
		line := headOID + " HEAD"
		if symrefs {
			line += " symref-target:" + r.headRef()
		}
		out = append(out, pktString(line+"\n")...)
	}
	for _, name := range r.sortedRefNames() {
		if !keep(name) {
			continue
		}
		oid := r.Refs[name]
		line := oid + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if obj := r.Objects[oid]; obj != nil && obj.typ == ObjTag && obj.tagTarget != "" {
				line += " peeled:" + obj.tagTarget
			}
		}
		out = append(out, pktString(line+"\n")...)
	}
	out = append(out, pktFlush()...)
	return out
}

// ---------------------------------------------------------------------------
// Fetch (protocol v2 and v0)
// ---------------------------------------------------------------------------

type fetchArgs struct {
	wants         []string
	haves         []string
	shallow       []string
	depth         int
	thinPack      bool
	seenDone      bool
}

// parseFetchArgs parses v2 fetch args (after the delim) or v0 want/have
// lines. Capabilities after NUL on the first want line are dropped.
func parseFetchArgs(lines []string) fetchArgs {
	var fa fetchArgs
	for _, ln := range lines {
		ln = strings.TrimSuffix(ln, "\n")
		if idx := strings.IndexByte(ln, 0); idx >= 0 {
			ln = ln[:idx]
		}
		switch {
		case strings.HasPrefix(ln, "want "):
			fa.wants = append(fa.wants, strings.Fields(ln[5:])[0])
		case strings.HasPrefix(ln, "have "):
			fa.haves = append(fa.haves, strings.TrimSpace(ln[5:]))
		case strings.HasPrefix(ln, "shallow "):
			fa.shallow = append(fa.shallow, strings.TrimSpace(ln[8:]))
		case strings.HasPrefix(ln, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(ln[7:])); err == nil {
				fa.depth = n
			}
		case ln == "thin-pack":
			fa.thinPack = true
		case ln == "done":
			fa.seenDone = true
		}
	}
	return fa
}

// fetchResponseV2 renders a v2 fetch response: acknowledgments (only when
// the client sent have lines — a real git client that sent no haves reads
// the first section header as the packfile section and dies on anything
// else), optional shallow-info, and the packfile section with at least one
// sideband packet.
func (r *Repo) fetchResponseV2(fa fetchArgs) []byte {
	plan := r.planFetch(fa.wants, fa.haves, fa.shallow, fa.depth)
	var out []byte
	if len(fa.haves) > 0 {
		out = append(out, pktString("acknowledgments\n")...)
		if plan.ack != "" {
			out = append(out, pktString("ACK "+plan.ack+"\n")...)
			out = append(out, pktString("ready\n")...)
		} else {
			out = append(out, pktString("NAK\n")...)
		}
		out = append(out, pktDelim()...)
	}
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		out = append(out, pktString("shallow-info\n")...)
		for _, s := range plan.shallow {
			out = append(out, pktString("shallow "+s+"\n")...)
		}
		for _, s := range plan.unshallow {
			out = append(out, pktString("unshallow "+s+"\n")...)
		}
		out = append(out, pktDelim()...)
	}
	out = append(out, pktString("packfile\n")...)
	pack := writePack(plan.objs)
	out = append(out, sidebandChunks(pack)...)
	return out
}

// fetchResponseV0 renders a v0 fetch response: NAK/ACK, shallow lines, then
// the sideband pack and a final flush.
func (r *Repo) fetchResponseV0(fa fetchArgs) []byte {
	plan := r.planFetch(fa.wants, fa.haves, fa.shallow, fa.depth)
	var out []byte
	if plan.ack != "" {
		out = append(out, pktString("ACK "+plan.ack+"\n")...)
	} else {
		out = append(out, pktString("NAK\n")...)
	}
	for _, s := range plan.shallow {
		out = append(out, pktString("shallow "+s+"\n")...)
	}
	for _, s := range plan.unshallow {
		out = append(out, pktString("unshallow "+s+"\n")...)
	}
	pack := writePack(plan.objs)
	out = append(out, sidebandChunks(pack)...)
	return out
}

// ---------------------------------------------------------------------------
// Receive-pack (push)
// ---------------------------------------------------------------------------

type pushCommand struct {
	old string
	new string
	ref string
}

// handleReceivePack applies a push. Returns the report-status body.
func (r *Repo) handleReceivePack(body []byte) []byte {
	cmds, pack, err := splitPackFromCommands(body)
	// Inner report stream: pkt("unpack ok\n") + per-command ok/ng lines.
	inner := func(lines ...string) []byte {
		var b []byte
		for _, l := range lines {
			b = append(b, pktString(l)...)
		}
		return b
	}
	// Wrap the inner stream into outer sideband pkt-lines. The band-1
	// payload carries the inner pkt-line stream VERBATIM, length prefixes
	// included: the reader's sideband demultiplexer concatenates band-1
	// bytes and parses the result as pkt-lines, so stripping the inner
	// framing would make it read "unpack ok" as a length prefix.
	wrap := func(innerBytes []byte) []byte {
		var out []byte
		const maxChunk = 65515 // outer payload = 1 band byte + chunk
		for len(innerBytes) > 0 {
			n := len(innerBytes)
			if n > maxChunk {
				n = maxChunk
			}
			payload := make([]byte, 1+n)
			payload[0] = 1
			copy(payload[1:], innerBytes[:n])
			out = append(out, pktLine(payload)...)
			innerBytes = innerBytes[n:]
		}
		out = append(out, pktFlush()...)
		return out
	}
	if err != nil || len(cmds) == 0 {
		detail := "malformed command list"
		if err != nil {
			detail = err.Error()
		}
		// The inner report stream ends with a flush: the reader parses
		// the demultiplexed band-1 bytes as pkt-lines and waits for that
		// flush after the last report line (the outer sideband flush is
		// consumed by the demultiplexer itself).
		return wrap(append(inner("unpack "+detail+"\n"), pktFlush()...))
	}
	// Unpack first: every object lands in the store before any ref moves,
	// so a mixed push can report ok/ng per command independently.
	if len(pack) > 0 {
		resolveBase := func(oid string) (ObjType, []byte, bool) {
			if o := r.Objects[oid]; o != nil {
				return o.typ, o.data, true
			}
			return 0, nil, false
		}
		objs, _, err := readPack(pack, resolveBase)
		if err != nil {
			return wrap(append(inner("unpack "+err.Error()+"\n"), pktFlush()...))
		}
		for oid, o := range objs {
			if _, ok := r.Objects[oid]; !ok {
				r.storeObject(o.typ, o.data)
			}
		}
	}
	var report []string
	report = append(report, "unpack ok\n")
	for _, c := range cmds {
		fields := strings.Fields(c)
		// Command lines may carry capabilities after NUL on the first line.
		if len(fields) < 3 {
			report = append(report, "ng "+refOf(c)+" malformed command\n")
			continue
		}
		pc := pushCommand{old: fields[0], new: fields[1], ref: fields[2]}
		if idx := strings.IndexByte(pc.ref, 0); idx >= 0 {
			pc.ref = pc.ref[:idx]
		}
		ok, reason := r.applyPushCommand(pc)
		if ok {
			report = append(report, "ok "+pc.ref+"\n")
		} else {
			report = append(report, "ng "+pc.ref+" "+reason+"\n")
		}
	}
	// Inner flush: the report reader waits for it after the last line.
	return wrap(append(inner(report...), pktFlush()...))
}

func refOf(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) >= 3 {
		return f[2]
	}
	return ""
}

// applyPushCommand applies one ref update. Returns (ok, reason).
func (r *Repo) applyPushCommand(pc pushCommand) (bool, string) {
	cur, exists := r.Refs[pc.ref]
	if pc.new == ZeroOID {
		// Delete.
		if !exists {
			return false, "remote ref does not exist"
		}
		if pc.old != ZeroOID && pc.old != cur {
			return false, "non-fast-forward"
		}
		delete(r.Refs, pc.ref)
		return true, ""
	}
	if !isOID(pc.new) {
		return false, "invalid new object id"
	}
	if r.Objects[pc.new] == nil {
		return false, "unable to resolve new object"
	}
	if pc.old == ZeroOID {
		// Create.
		if exists {
			return false, "already exists"
		}
		r.Refs[pc.ref] = pc.new
		return true, ""
	}
	// Update: old must match, and new must descend from old.
	if !exists || cur != pc.old {
		return false, "non-fast-forward"
	}
	if pc.new != pc.old && !r.isAncestor(pc.old, pc.new) {
		return false, "non-fast-forward"
	}
	r.Refs[pc.ref] = pc.new
	return true, ""
}

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
