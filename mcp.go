package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// MCP: JSON-RPC 2.0 over Streamable HTTP (plain JSON answers).
// Modern era: server/discover (2026-07-28). Legacy era: initialize +
// notifications/initialized. tools/list is a fixed array so two calls are
// byte-identical. Tools share the REST store exactly.
// ---------------------------------------------------------------------------

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func schema(props map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

var mcpTools = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a repository owned by the viewer.",
		InputSchema: schema(map[string]any{
			"owner":       strProp("Repository owner login"),
			"name":        strProp("Repository name"),
			"description": strProp("Repository description"),
			"private":     boolProp("Whether the repository is private"),
		}, []string{"name"}),
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
		}, []string{"owner", "repo"}),
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
			"title": strProp("Issue title"),
			"body":  strProp("Issue body"),
		}, []string{"owner", "repo", "title"}),
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
			"state": strProp("Issue state filter: open, closed or all"),
		}, []string{"owner", "repo"}),
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request between two branches.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Repository owner login"),
			"repo":  strProp("Repository name"),
			"title": strProp("Pull request title"),
			"body":  strProp("Pull request body"),
			"head":  strProp("Head branch name"),
			"base":  strProp("Base branch name"),
		}, []string{"owner", "repo", "title", "head", "base"}),
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request with a squash merge.",
		InputSchema: schema(map[string]any{
			"owner":       strProp("Repository owner login"),
			"repo":        strProp("Repository name"),
			"pull_number": intProp("Pull request number"),
		}, []string{"owner", "repo", "pull_number"}),
	},
}

func mcpToolsList() []mcpToolDef { return mcpTools }

// mcpError renders a JSON-RPC error response.
func mcpError(id any, code int, message string) []byte {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
	return marshal(resp)
}

func mcpResult(id any, result any) []byte {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	return marshal(resp)
}

// toolResult renders a tools/call result with text content plus structured
// content (the suite reads both).
func toolResult(payload any, isError bool) map[string]any {
	text, _ := json.Marshal(payload)
	out := map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(text)}},
	}
	if !isError {
		out["structuredContent"] = payload
	} else {
		out["isError"] = true
	}
	return out
}

func toolErrorResult(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}
}

// splitFullName parses "owner/repo" into its parts.
func splitFullName(full string) (string, string, bool) {
	parts := strings.SplitN(full, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// mcpOwnerRepo resolves owner/repo arguments, falling back to a full_name
// argument ("owner/repo") when either part is missing.
func mcpOwnerRepo(args map[string]any) (owner, repo string) {
	owner, _ = args["owner"].(string)
	repo, _ = args["repo"].(string)
	if full, ok := args["full_name"].(string); ok && full != "" {
		if owner == "" || repo == "" {
			if o, r, ok := splitFullName(full); ok {
				if owner == "" {
					owner = o
				}
				if repo == "" {
					repo = r
				}
			}
		}
	}
	if owner == "" {
		owner = viewerLogin
	}
	return owner, repo
}

func mcpArgStr(args map[string]any, key string) (string, bool) {
	s, ok := args[key].(string)
	return s, ok && s != ""
}

func mcpArgInt(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case int:
		return v, true
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil
	}
	return 0, false
}

// callMCPTool executes one tool. It returns the result payload and, when
// the call failed at the tool level, an error message (rendered isError).
func (s *server) callMCPTool(name string, args map[string]any) (any, string) {
	owner, repoName := mcpOwnerRepo(args)
	switch name {
	case "create_repo":
		repoNameArg, ok := mcpArgStr(args, "name")
		if !ok {
			return nil, "missing required argument: name"
		}
		desc, _ := args["description"].(string)
		priv, _ := args["private"].(bool)
		if s.store.GetRepo(owner, repoNameArg) != nil {
			return nil, "Repository already exists"
		}
		r := s.store.CreateRepo(owner, repoNameArg, priv, desc)
		r.seedLabels()
		return toRepoJSON(r), ""
	case "get_repo":
		if owner == "" || repoName == "" {
			return nil, "missing required argument: owner or repo"
		}
		r := s.store.GetRepo(owner, repoName)
		if r == nil {
			return nil, "repository not found"
		}
		return toRepoJSON(r), ""
	case "create_issue":
		title, ok := mcpArgStr(args, "title")
		if !ok {
			return nil, "missing required argument: title"
		}
		body, _ := args["body"].(string)
		r := s.store.GetRepo(owner, repoName)
		if r == nil {
			return nil, "repository not found"
		}
		iss := s.store.newIssue(r, title, body, nil)
		return toIssueJSON(r, iss), ""
	case "list_issues":
		r := s.store.GetRepo(owner, repoName)
		if r == nil {
			return nil, "repository not found"
		}
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		out := make([]issueJSON, 0)
		for _, iss := range r.IssueOrder {
			if state != "all" && iss.State != state {
				continue
			}
			out = append(out, toIssueJSON(r, iss))
		}
		return out, ""
	case "create_pull_request":
		title, ok := mcpArgStr(args, "title")
		if !ok {
			return nil, "missing required argument: title"
		}
		head, okH := mcpArgStr(args, "head")
		base, okB := mcpArgStr(args, "base")
		if !okH || !okB {
			return nil, "missing required argument: head or base"
		}
		body, _ := args["body"].(string)
		r := s.store.GetRepo(owner, repoName)
		if r == nil {
			return nil, "repository not found"
		}
		headSHA := r.Refs["refs/heads/"+head]
		baseSHA := r.Refs["refs/heads/"+base]
		if headSHA == "" || baseSHA == "" {
			return nil, "head or base branch does not exist"
		}
		mb := r.mergeBase(headSHA, baseSHA)
		var mbTree string
		if mb != "" {
			mbTree = r.commitTree(mb)
		}
		files := r.diffFiles(mbTree, r.commitTree(headSHA))
		pr := s.store.newPull(r, title, body, head, base, headSHA, baseSHA, files)
		return toPRJSON(pr), ""
	case "merge_pull_request":
		num, ok := mcpArgInt(args, "pull_number")
		if !ok {
			return nil, "missing required argument: pull_number"
		}
		r := s.store.GetRepo(owner, repoName)
		if r == nil {
			return nil, "repository not found"
		}
		pr := r.Pulls[num]
		if pr == nil {
			return nil, "pull request not found"
		}
		if pr.Merged {
			return map[string]any{"merged": true, "sha": pr.MergeSHA, "number": pr.Number}, ""
		}
		conflicted, resultFlat := r.prConflict(pr)
		if conflicted {
			return nil, "merge conflict"
		}
		baseTip := repoBaseTip(r, pr)
		resultTree := r.buildTreeFromFlat(resultFlat)
		msg := "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + pr.HeadRef
		mergeOID := r.storeObject(ObjCommit, buildCommit(resultTree, []string{baseTip}, msg))
		r.Refs["refs/heads/"+pr.BaseRef] = mergeOID
		pr.State = "closed"
		pr.Merged = true
		pr.MergedAt = "2026-11-20T00:00:00Z"
		pr.MergeSHA = mergeOID
		pr.BaseSHA = mergeOID
		return map[string]any{"merged": true, "sha": mergeOID, "number": pr.Number}, ""
	}
	return nil, "unknown tool"
}

// handleMCP serves POST /mcp.
func (s *server) handleMCP(req *request, keepAlive bool) []byte {
	dec := json.NewDecoder(strings.NewReader(string(req.body)))
	dec.UseNumber()
	var envelope any
	if err := dec.Decode(&envelope); err != nil {
		// Not JSON at all: HTTP 400 with JSON-RPC -32700.
		return response(400, "application/json", mcpError(nil, -32700, "Parse error"), keepAlive)
	}
	m, ok := envelope.(map[string]any)
	if !ok {
		return response(400, "application/json", mcpError(nil, -32600, "Invalid Request"), keepAlive)
	}
	method, _ := m["method"].(string)
	id, hasID := m["id"]
	params, _ := m["params"].(map[string]any)
	if params == nil {
		params = make(map[string]any)
	}
	// Notifications carry no id: acknowledge with 202 and no body.
	if !hasID {
		return response(202, "application/json", nil, keepAlive)
	}
	switch method {
	case "server/discover":
		result := map[string]any{
			"supportedVersions": []string{"2026-07-28"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "forge-mcp", "version": "1.0.0"},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": "forge-mcp", "version": "1.0.0"},
			},
		}
		return jsonResponse(200, mcpResult(id, result), keepAlive)
	case "initialize":
		proto, _ := params["protocolVersion"].(string)
		if proto == "" {
			proto = "2025-11-25"
		}
		result := map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge-mcp", "version": "1.0.0"},
		}
		return jsonResponse(200, mcpResult(id, result), keepAlive)
	case "tools/list":
		return jsonResponse(200, mcpResult(id, map[string]any{"tools": mcpToolsList()}), keepAlive)
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = make(map[string]any)
		}
		known := false
		for _, t := range mcpTools {
			if t.Name == name {
				known = true
				break
			}
		}
		if !known {
			return jsonResponse(200, mcpError(id, -32601, "unknown tool: "+name), keepAlive)
		}
		// Required-argument validation (missing -> isError, never HTTP 500).
		var missing string
		for _, t := range mcpTools {
			if t.Name != name {
				continue
			}
			if reqList, ok := t.InputSchema["required"].([]string); ok {
				for _, key := range reqList {
					if _, present := args[key]; !present {
						missing = key
						break
					}
				}
			}
		}
		if missing != "" {
			return jsonResponse(200, mcpResult(id, toolErrorResult("missing required argument: "+missing)), keepAlive)
		}
		payload, errMsg := s.callMCPTool(name, args)
		if errMsg != "" {
			return jsonResponse(200, mcpResult(id, toolErrorResult(errMsg)), keepAlive)
		}
		return jsonResponse(200, mcpResult(id, toolResult(payload, false)), keepAlive)
	default:
		return jsonResponse(200, mcpError(id, -32601, "method not found: "+method), keepAlive)
	}
}
