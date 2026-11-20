package main

import (
	"strings"
)

// ---------------------------------------------------------------------------
// Routing + auth
// ---------------------------------------------------------------------------

// authed reports whether the request carries acceptable credentials. When
// the server was started with MAKEFUN_SHOP_TOKEN set, the bearer must match
// exactly; otherwise any non-empty bearer is accepted (local runs pick
// their own token), while a missing header is always refused.
func (s *server) authed(req *request) bool {
	auth := req.authorization
	if auth == "" {
		return false
	}
	if s.token == "" {
		return strings.HasPrefix(auth, "Bearer ") && len(auth) > 7
	}
	return auth == "Bearer "+s.token
}

// queryGet returns the first value of a query parameter, or "".
func queryGet(query, key string) string {
	for len(query) > 0 {
		pair := query
		if idx := strings.IndexByte(query, '&'); idx >= 0 {
			pair, query = query[:idx], query[idx+1:]
		} else {
			query = ""
		}
		if idx := strings.IndexByte(pair, '='); idx >= 0 {
			if pair[:idx] == key {
				return unescape(pair[idx+1:])
			}
		} else if pair == key {
			return ""
		}
	}
	return ""
}

// unescape decodes %XX and + in a query value (small, allocation-light).
func unescape(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) {
			hi := hexVal(s[i+1])
			lo := hexVal(s[i+2])
			if hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		if c == '+' {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// pathSegments splits a path into its '/'-separated parts (no empty parts).
func pathSegments(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// dispatch routes one parsed request to its handler and renders the reply.
func (s *server) dispatch(req *request) []byte {
	keepAlive := !req.closeAfter
	// Health check: no auth, always available.
	if req.path == "/healthz" {
		if req.method != "GET" {
			return jsonError(405, "Method Not Allowed", keepAlive)
		}
		return jsonResponse(200, []byte(`{"ok":true}`), keepAlive)
	}
	// MCP endpoint.
	if req.path == "/mcp" {
		if req.method != "POST" {
			return jsonError(405, "Method Not Allowed", keepAlive)
		}
		if !s.authed(req) {
			return jsonError(401, "Unauthorized", keepAlive)
		}
		return s.handleMCP(req, keepAlive)
	}
	// GraphQL endpoint.
	if req.path == "/api/graphql" {
		if req.method != "POST" {
			return jsonError(405, "Method Not Allowed", keepAlive)
		}
		if !s.authed(req) {
			return jsonError(401, "Unauthorized", keepAlive)
		}
		return s.handleGraphQL(req, keepAlive)
	}
	// REST v3.
	if req.path == "/api/v3" || strings.HasPrefix(req.path, "/api/v3/") {
		if !s.authed(req) {
			return jsonError(401, "Unauthorized", keepAlive)
		}
		return s.handleREST(req, keepAlive)
	}
	// Git smart HTTP: /{owner}/{repo}.git/<tail>
	segs := pathSegments(req.path)
	if len(segs) >= 2 && strings.HasSuffix(segs[1], ".git") {
		owner := segs[0]
		repoName := strings.TrimSuffix(segs[1], ".git")
		tail := strings.Join(segs[2:], "/")
		if !validOwnerRepo(owner) || !validOwnerRepo(repoName) {
			return jsonError(404, "Not Found", keepAlive)
		}
		if !s.authed(req) {
			return jsonError(401, "Unauthorized", keepAlive)
		}
		return s.handleGit(req, owner, repoName, tail, keepAlive)
	}
	return jsonError(404, "Not Found", keepAlive)
}

func validOwnerRepo(s string) bool {
	if len(s) == 0 || len(s) > 100 {
		return false
	}
	for i, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
		if i == 0 {
			ok = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		}
		if !ok {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Git smart HTTP dispatch
// ---------------------------------------------------------------------------

func (s *server) handleGit(req *request, owner, repoName, tail string, keepAlive bool) []byte {
	repo := s.store.GetRepo(owner, repoName)
	switch tail {
	case "info/refs":
		if req.method != "GET" {
			return jsonError(405, "Method Not Allowed", keepAlive)
		}
		if repo == nil {
			return jsonError(404, "Not Found", keepAlive)
		}
		service := queryGet(req.query, "service")
		switch service {
		case "git-upload-pack":
			if req.gitProtocol == "version=2" {
				return response(200, "application/x-git-upload-pack-advertisement", repo.advertisementV2(), keepAlive)
			}
			return response(200, "application/x-git-upload-pack-advertisement", repo.advertisementV0("git-upload-pack"), keepAlive)
		case "git-receive-pack":
			return response(200, "application/x-git-receive-pack-advertisement", repo.advertisementV0("git-receive-pack"), keepAlive)
		default:
			return jsonError(400, "unknown service", keepAlive)
		}
	case "git-upload-pack":
		if req.method != "POST" {
			return jsonError(405, "Method Not Allowed", keepAlive)
		}
		if repo == nil {
			return jsonError(404, "Not Found", keepAlive)
		}
		return s.handleUploadPack(req, repo, keepAlive)
	case "git-receive-pack":
		if req.method != "POST" {
			return jsonError(405, "Method Not Allowed", keepAlive)
		}
		if repo == nil {
			return jsonError(404, "Not Found", keepAlive)
		}
		body := repo.handleReceivePack(req.body)
		return response(200, "application/x-git-receive-pack-result", body, keepAlive)
	default:
		return jsonError(404, "Not Found", keepAlive)
	}
}

// handleUploadPack serves both v2 commands (ls-refs, fetch) and the v0 fetch.
func (s *server) handleUploadPack(req *request, repo *Repo, keepAlive bool) []byte {
	if req.gitProtocol == "version=2" {
		pkts, err := parsePktLines(req.body)
		if err != nil || len(pkts) == 0 {
			return jsonError(400, "malformed request", keepAlive)
		}
		// First data line names the command.
		var cmd string
		var args []string
		seenDelim := false
		for _, p := range pkts {
			switch p.kind {
			case kindFlush:
				// end of request
			case kindDelim:
				seenDelim = true
			case kindData:
				line := strings.TrimSuffix(string(p.payload), "\n")
				if cmd == "" {
					cmd = line
				} else if seenDelim {
					args = append(args, line)
				}
			}
		}
		switch cmd {
		case "command=ls-refs":
			return response(200, "application/x-git-upload-pack-result", repo.handleLsRefs(args), keepAlive)
		case "command=fetch":
			fa := parseFetchArgs(args)
			return response(200, "application/x-git-upload-pack-result", repo.fetchResponseV2(fa), keepAlive)
		default:
			return jsonError(400, "unknown command", keepAlive)
		}
	}
	// v0 fetch: want/have lines, then flush.
	pkts, err := parsePktLines(req.body)
	if err != nil {
		return jsonError(400, "malformed request", keepAlive)
	}
	var lines []string
	for _, p := range pkts {
		if p.kind == kindData {
			lines = append(lines, string(p.payload))
		}
	}
	fa := parseFetchArgs(lines)
	return response(200, "application/x-git-upload-pack-result", repo.fetchResponseV0(fa), keepAlive)
}
