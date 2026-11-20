package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// GraphQL: lexer + parser for the query subset the suite uses
// (operations, variables, aliases, arguments, lists/objects/enums,
// inline + named fragments, introspection field __type).
// ---------------------------------------------------------------------------

type gqlTok struct {
	kind string // "name","int","float","string","punct","eof"
	val  string
}

func gqlLex(src string) ([]gqlTok, error) {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' || c == '!' || c == '=' || c == '@' || c == '$' || c == '&' || c == '|':
			toks = append(toks, gqlTok{"punct", string(c)})
			i++
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlTok{"punct", "..."})
			i += 3
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					default:
						sb.WriteByte(src[j])
					}
					j++
				} else {
					sb.WriteByte(src[j])
					j++
				}
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, gqlTok{"string", sb.String()})
			i = j + 1
		case c >= '0' && c <= '9' || c == '-':
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			toks = append(toks, gqlTok{kind, src[i:j]})
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(src) && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			toks = append(toks, gqlTok{"name", src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	toks = append(toks, gqlTok{"eof", ""})
	return toks, nil
}

// AST nodes.

type gqlValue struct {
	kind string // "var","int","float","string","bool","null","enum","list","object"
	s    string
	num  float64
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlField struct {
	alias string
	name  string
	args  map[string]*gqlValue
	sel   []*gqlSelection
}

type gqlSelection struct {
	field    *gqlField
	fragName string // named fragment spread
	inline   *gqlInlineFrag
}

type gqlInlineFrag struct {
	typeCond string
	sel      []*gqlSelection
}

type gqlOperation struct {
	kind      string // "query" | "mutation"
	name      string
	varDefs   map[string]*gqlValue // var name -> default value (nil if none)
	sel       []*gqlSelection
	fragments map[string]*gqlInlineFrag
}

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func (p *gqlParser) peek() gqlTok  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlTok  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(kind, val string) error {
	t := p.next()
	if t.kind != kind || (val != "" && t.val != val) {
		return fmt.Errorf("expected %s %q, got %s %q", kind, val, t.kind, t.val)
	}
	return nil
}

func (p *gqlParser) parseDocument() ([]*gqlOperation, map[string]*gqlInlineFrag, error) {
	var ops []*gqlOperation
	frags := make(map[string]*gqlInlineFrag)
	for p.peek().kind != "eof" {
		t := p.peek()
		if t.kind == "name" && t.val == "fragment" {
			p.next()
			nameTok := p.next()
			if nameTok.kind != "name" {
				return nil, nil, fmt.Errorf("fragment needs a name")
			}
			if err := p.expect("name", "on"); err != nil {
				return nil, nil, err
			}
			typeTok := p.next()
			if typeTok.kind != "name" {
				return nil, nil, fmt.Errorf("fragment needs a type condition")
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			frags[nameTok.val] = &gqlInlineFrag{typeCond: typeTok.val, sel: sel}
			continue
		}
		op, err := p.parseOperation()
		if err != nil {
			return nil, nil, err
		}
		ops = append(ops, op)
	}
	return ops, frags, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{kind: "query", varDefs: make(map[string]*gqlValue)}
	t := p.peek()
	if t.kind == "name" && (t.val == "query" || t.val == "mutation") {
		op.kind = p.next().val
		if p.peek().kind == "name" {
			op.name = p.next().val
		}
	} else if t.kind == "name" {
		// Anonymous shorthand with an operation name but no keyword is not
		// valid; treat a bare name as the operation name of a query.
		op.name = p.next().val
	}
	if p.peek().val == "(" {
		if err := p.parseVarDefs(op); err != nil {
			return nil, err
		}
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.sel = sel
	return op, nil
}

func (p *gqlParser) parseVarDefs(op *gqlOperation) error {
	if err := p.expect("punct", "("); err != nil {
		return err
	}
	for p.peek().val != ")" {
		if err := p.expect("punct", "$"); err != nil {
			return err
		}
		name := p.next()
		if name.kind != "name" {
			return fmt.Errorf("variable needs a name")
		}
		if err := p.expect("punct", ":"); err != nil {
			return err
		}
		// Type: Name, [Name], Name!, [Name!]! — consume tokens.
		for {
			t := p.next()
			if t.kind == "name" || t.val == "[" || t.val == "]" || t.val == "!" {
				continue
			}
			// Put back anything else (should not happen).
			p.pos--
			break
		}
		if p.peek().val == "=" {
			p.next()
			v, err := p.parseValue()
			if err != nil {
				return err
			}
			op.varDefs[name.val] = v
		} else {
			op.varDefs[name.val] = nil
		}
		if p.peek().val == "," {
			p.next()
		}
	}
	return p.expect("punct", ")")
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expect("punct", "{"); err != nil {
		return nil, err
	}
	var out []*gqlSelection
	for p.peek().val != "}" {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
	if err := p.expect("punct", "}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	if p.peek().val == "..." {
		p.next()
		if p.peek().kind == "name" && p.peek().val != "on" {
			name := p.next().val
			// Directives after a spread are skipped.
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			return &gqlSelection{fragName: name}, nil
		}
		// Inline fragment: ... on Type { ... } or ... { ... }.
		frag := &gqlInlineFrag{}
		if p.peek().val == "on" {
			p.next()
			typeTok := p.next()
			if typeTok.kind != "name" {
				return nil, fmt.Errorf("inline fragment needs a type")
			}
			frag.typeCond = typeTok.val
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		frag.sel = sel
		return &gqlSelection{inline: frag}, nil
	}
	f, err := p.parseField()
	if err != nil {
		return nil, err
	}
	return &gqlSelection{field: f}, nil
}

func (p *gqlParser) skipDirectives() error {
	for p.peek().val == "@" {
		p.next()
		if p.next().kind != "name" {
			return fmt.Errorf("directive needs a name")
		}
		if p.peek().val == "(" {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *gqlParser) parseField() (*gqlField, error) {
	nameTok := p.next()
	if nameTok.kind != "name" {
		return nil, fmt.Errorf("field needs a name, got %s %q", nameTok.kind, nameTok.val)
	}
	f := &gqlField{name: nameTok.val, args: make(map[string]*gqlValue)}
	if p.peek().val == ":" {
		p.next()
		realTok := p.next()
		if realTok.kind != "name" {
			return nil, fmt.Errorf("alias needs a field name")
		}
		f.alias = f.name
		f.name = realTok.val
	}
	if p.peek().val == "(" {
		args, err := p.parseArguments()
		if err != nil {
			return nil, err
		}
		f.args = args
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	if p.peek().val == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.sel = sel
	}
	return f, nil
}

func (p *gqlParser) parseArguments() (map[string]*gqlValue, error) {
	args := make(map[string]*gqlValue)
	if err := p.expect("punct", "("); err != nil {
		return nil, err
	}
	for p.peek().val != ")" {
		name := p.next()
		if name.kind != "name" {
			return nil, fmt.Errorf("argument needs a name")
		}
		if err := p.expect("punct", ":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name.val] = v
		if p.peek().val == "," {
			p.next()
		}
	}
	if err := p.expect("punct", ")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t := p.next()
	switch {
	case t.val == "$":
		name := p.next()
		if name.kind != "name" {
			return nil, fmt.Errorf("variable needs a name")
		}
		return &gqlValue{kind: "var", s: name.val}, nil
	case t.kind == "int":
		n, _ := strconv.ParseFloat(t.val, 64)
		return &gqlValue{kind: "int", s: t.val, num: n}, nil
	case t.kind == "float":
		n, _ := strconv.ParseFloat(t.val, 64)
		return &gqlValue{kind: "float", num: n}, nil
	case t.kind == "string":
		return &gqlValue{kind: "string", s: t.val}, nil
	case t.kind == "name" && t.val == "true":
		return &gqlValue{kind: "bool", s: "true"}, nil
	case t.kind == "name" && t.val == "false":
		return &gqlValue{kind: "bool", s: "false"}, nil
	case t.kind == "name" && t.val == "null":
		return &gqlValue{kind: "null"}, nil
	case t.kind == "name":
		return &gqlValue{kind: "enum", s: t.val}, nil
	case t.val == "[":
		v := &gqlValue{kind: "list"}
		for p.peek().val != "]" {
			item, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.list = append(v.list, item)
			if p.peek().val == "," {
				p.next()
			}
		}
		if err := p.expect("punct", "]"); err != nil {
			return nil, err
		}
		return v, nil
	case t.val == "{":
		v := &gqlValue{kind: "object", obj: make(map[string]*gqlValue)}
		for p.peek().val != "}" {
			name := p.next()
			if name.kind != "name" {
				return nil, fmt.Errorf("object field needs a name")
			}
			if err := p.expect("punct", ":"); err != nil {
				return nil, err
			}
			item, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.obj[name.val] = item
			if p.peek().val == "," {
				p.next()
			}
		}
		if err := p.expect("punct", "}"); err != nil {
			return nil, err
		}
		return v, nil
	}
	return nil, fmt.Errorf("unexpected value token %s %q", t.kind, t.val)
}

// evalValue resolves a parsed value against variables into plain Go data.
func evalValue(v *gqlValue, vars map[string]any) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "var":
		if vars != nil {
			return vars[v.s]
		}
		return nil
	case "int", "float":
		return v.num
	case "string", "enum":
		return v.s
	case "bool":
		return v.s == "true"
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, evalValue(item, vars))
		}
		return out
	case "object":
		out := make(map[string]any, len(v.obj))
		for k, item := range v.obj {
			out[k] = evalValue(item, vars)
		}
		return out
	}
	return nil
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

// gqlObject is a resolved node: kind selects the field table, data carries
// the underlying store object.
type gqlObject struct {
	kind string
	data any
}

type gqlExec struct {
	s         *server
	vars      map[string]any
	fragments map[string]*gqlInlineFrag
	errors    []map[string]any
}

// fieldKey is the response key: alias when present, else the field name.
func fieldKey(f *gqlField) string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

func (e *gqlExec) argString(f *gqlField, name string) (string, bool) {
	v, ok := f.args[name]
	if !ok {
		return "", false
	}
	val := evalValue(v, e.vars)
	s, ok := val.(string)
	return s, ok
}

func (e *gqlExec) argInt(f *gqlField, name string) (int, bool) {
	v, ok := f.args[name]
	if !ok {
		return 0, false
	}
	val := evalValue(v, e.vars)
	switch n := val.(type) {
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case int:
		return n, true
	}
	return 0, false
}

func (e *gqlExec) argAny(f *gqlField, name string) (any, bool) {
	v, ok := f.args[name]
	if !ok {
		return nil, false
	}
	return evalValue(v, e.vars), true
}

// executeSelection resolves a selection set against an object into a JSON
// object (field key -> value). Unknown fields record an error and resolve
// null; fragments expand inline.
func (e *gqlExec) executeSelection(obj *gqlObject, sel []*gqlSelection) map[string]any {
	out := make(map[string]any)
	for _, s := range sel {
		if s.field != nil {
			f := s.field
			val, err := e.resolveField(obj, f)
			if err != nil {
				e.errors = append(e.errors, map[string]any{"message": err.Error()})
				out[fieldKey(f)] = nil
				continue
			}
			out[fieldKey(f)] = e.materialize(val, f.sel)
			continue
		}
		if s.inline != nil {
			if s.inline.typeCond != "" && s.inline.typeCond != obj.kind {
				continue
			}
			sub := e.executeSelection(obj, s.inline.sel)
			for k, v := range sub {
				out[k] = v
			}
			continue
		}
		if s.fragName != "" {
			if frag, ok := e.fragments[s.fragName]; ok {
				if frag.typeCond != "" && frag.typeCond != obj.kind {
					continue
				}
				sub := e.executeSelection(obj, frag.sel)
				for k, v := range sub {
					out[k] = v
				}
			}
		}
	}
	return out
}

// materialize turns a resolved value into JSON-ready data: scalars pass
// through, objects execute their sub-selections, lists map over elements.
func (e *gqlExec) materialize(val any, sel []*gqlSelection) any {
	switch v := val.(type) {
	case *gqlObject:
		if len(sel) == 0 {
			return nil
		}
		return e.executeSelection(v, sel)
	case []*gqlObject:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, e.materialize(item, sel))
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, e.materialize(item, sel))
		}
		return out
	default:
		return val
	}
}

// ---------------------------------------------------------------------------
// Global ids: opaque, stable, never equal to a bare number.
// R:<repoID>  I:<repoID>:<issueNum>  P:<repoID>:<prNum>  C:<repoID>:<issueNum>:<commentID>
// ---------------------------------------------------------------------------

func gid(kind string, parts ...int) string {
	var sb strings.Builder
	sb.WriteString(kind)
	for _, p := range parts {
		sb.WriteByte(':')
		sb.WriteString(strconv.Itoa(p))
	}
	return base64.RawURLEncoding.EncodeToString([]byte(sb.String()))
}

func parseGID(id string) (string, []int, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return "", nil, false
	}
	kind := parts[0]
	if kind != "R" && kind != "I" && kind != "P" && kind != "C" {
		return "", nil, false
	}
	var nums []int
	for _, p := range parts[1:] {
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", nil, false
		}
		nums = append(nums, n)
	}
	return kind, nums, true
}

// repoByGID resolves a repository global id.
func (e *gqlExec) repoByGID(id string) *Repo {
	kind, parts, ok := parseGID(id)
	if !ok || kind != "R" || len(parts) != 1 {
		return nil
	}
	for _, r := range e.s.store.Order {
		if r.ID == parts[0] {
			return r
		}
	}
	return nil
}

func (e *gqlExec) issueByGID(id string) (*Repo, *Issue) {
	kind, parts, ok := parseGID(id)
	if !ok || kind != "I" || len(parts) != 2 {
		return nil, nil
	}
	for _, r := range e.s.store.Order {
		if r.ID == parts[0] {
			return r, r.Issues[parts[1]]
		}
	}
	return nil, nil
}

func (e *gqlExec) pullByGID(id string) (*Repo, *Pull) {
	kind, parts, ok := parseGID(id)
	if !ok || kind != "P" || len(parts) != 2 {
		return nil, nil
	}
	for _, r := range e.s.store.Order {
		if r.ID == parts[0] {
			return r, r.Pulls[parts[1]]
		}
	}
	return nil, nil
}

// cursorFor encodes a list offset opaquely; offsetOf decodes it (0 on junk).
func cursorFor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("off:" + strconv.Itoa(offset)))
}

func offsetOf(cursor string) int {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	s := string(raw)
	if !strings.HasPrefix(s, "off:") {
		return 0
	}
	n, err := strconv.Atoi(s[4:])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ---------------------------------------------------------------------------
// Type tables for validation + introspection
// ---------------------------------------------------------------------------

var gqlTypeFields = map[string][]string{
	"Query":                {"viewer", "repository", "node", "__type", "__typename"},
	"Mutation":             {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"Repository":           {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "__typename"},
	"User":                 {"login", "id", "__typename"},
	"Issue":                {"id", "number", "title", "state", "body", "__typename"},
	"IssueConnection":      {"nodes", "pageInfo", "__typename"},
	"PageInfo":             {"hasNextPage", "endCursor", "__typename"},
	"PullRequest":          {"id", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "__typename"},
	"PullRequestConnection": {"nodes", "__typename"},
	"Ref":                  {"name", "__typename"},
	"FileConnection":       {"nodes", "__typename"},
	"File":                 {"path", "__typename"},
	"Comment":              {"id", "body", "__typename"},
	"CommentEdge":          {"node", "__typename"},
	"CreateIssuePayload":   {"issue", "__typename"},
	"CloseIssuePayload":    {"issue", "__typename"},
	"CreatePullRequestPayload": {"pullRequest", "__typename"},
	"MergePullRequestPayload":  {"pullRequest", "__typename"},
	"AddCommentPayload":    {"commentEdge", "__typename"},
	"Type":                 {"name", "fields", "enumValues", "__typename"},
	"TypeField":            {"name", "__typename"},
	"EnumValue":            {"name", "__typename"},
}

var gqlTypeEnums = map[string][]string{
	"SearchType": {"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER"},
	"IssueState": {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
}

func gqlFieldKnown(kind, name string) bool {
	fields, ok := gqlTypeFields[kind]
	if !ok {
		return false
	}
	for _, f := range fields {
		if f == name {
			return true
		}
	}
	return false
}

// resolveField resolves one field on an object, returning a scalar, a
// *gqlObject, a list, or an error for unknown fields.
func (e *gqlExec) resolveField(obj *gqlObject, f *gqlField) (any, error) {
	if f.name == "__typename" {
		return obj.kind, nil
	}
	if !gqlFieldKnown(obj.kind, f.name) {
		return nil, fmt.Errorf("Cannot query field %q on type %q.", f.name, obj.kind)
	}
	switch obj.kind {
	case "Query":
		return e.resolveQuery(f)
	case "Mutation":
		return e.resolveMutation(f)
	case "Repository":
		return e.resolveRepository(obj.data.(*Repo), f)
	case "User":
		u := obj.data.(userJSON)
		switch f.name {
		case "login":
			return u.Login, nil
		case "id":
			return u.ID, nil
		}
	case "Issue":
		return e.resolveIssue(obj.data.(*Issue), f)
	case "IssueConnection":
		c := obj.data.(*gqlIssueConn)
		switch f.name {
		case "nodes":
			return c.nodes, nil
		case "pageInfo":
			return &gqlObject{kind: "PageInfo", data: c.page}, nil
		}
	case "PageInfo":
		p := obj.data.(gqlPageInfo)
		switch f.name {
		case "hasNextPage":
			return p.hasNextPage, nil
		case "endCursor":
			if p.endCursor == "" {
				return nil, nil
			}
			return p.endCursor, nil
		}
	case "PullRequest":
		return e.resolvePull(obj.data.(*Pull), f)
	case "PullRequestConnection":
		return obj.data.([]*gqlObject), nil
	case "Ref":
		return obj.data.(string), nil
	case "FileConnection":
		return obj.data.([]*gqlObject), nil
	case "File":
		return obj.data.(string), nil
	case "Comment":
		c := obj.data.(*Comment)
		switch f.name {
		case "id":
			return gid("C", c.RepoID, c.IssueNum, c.ID), nil
		case "body":
			return c.Body, nil
		}
	case "CommentEdge":
		return obj.data.(*gqlObject), nil
	case "CreateIssuePayload", "CloseIssuePayload":
		return obj.data.(*gqlObject), nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		return obj.data.(*gqlObject), nil
	case "AddCommentPayload":
		return obj.data.(*gqlObject), nil
	case "Type":
		return e.resolveType(obj.data.(gqlTypeDef), f)
	case "TypeField":
		return obj.data.(string), nil
	case "EnumValue":
		return obj.data.(string), nil
	}
	return nil, fmt.Errorf("Cannot query field %q on type %q.", f.name, obj.kind)
}

type gqlTypeDef struct {
	name       string
	fields     []string
	enumValues []string
}

type gqlPageInfo struct {
	hasNextPage bool
	endCursor   string
}

type gqlIssueConn struct {
	nodes []*gqlObject
	page  gqlPageInfo
}

func (e *gqlExec) resolveType(t gqlTypeDef, f *gqlField) (any, error) {
	switch f.name {
	case "name":
		return t.name, nil
	case "fields":
		out := make([]*gqlObject, 0, len(t.fields))
		for _, name := range t.fields {
			out = append(out, &gqlObject{kind: "TypeField", data: name})
		}
		return out, nil
	case "enumValues":
		out := make([]*gqlObject, 0, len(t.enumValues))
		for _, name := range t.enumValues {
			out = append(out, &gqlObject{kind: "EnumValue", data: name})
		}
		return out, nil
	}
	return nil, fmt.Errorf("Cannot query field %q on type %q.", f.name, "Type")
}

// ---------------------------------------------------------------------------
// Query / Repository / Issue / PullRequest resolvers
// ---------------------------------------------------------------------------

func (e *gqlExec) resolveQuery(f *gqlField) (any, error) {
	switch f.name {
	case "viewer":
		return &gqlObject{kind: "User", data: viewerUser()}, nil
	case "repository":
		owner, _ := e.argString(f, "owner")
		name, _ := e.argString(f, "name")
		if owner != viewerLogin {
			return nil, nil
		}
		repo := e.s.store.GetRepo(owner, name)
		if repo == nil {
			return nil, nil
		}
		return &gqlObject{kind: "Repository", data: repo}, nil
	case "node":
		id, _ := e.argString(f, "id")
		kind, parts, ok := parseGID(id)
		if !ok {
			return nil, nil
		}
		if kind == "R" && len(parts) == 1 {
			for _, r := range e.s.store.Order {
				if r.ID == parts[0] {
					return &gqlObject{kind: "Repository", data: r}, nil
				}
			}
		}
		return nil, nil
	case "__type":
		name, _ := e.argString(f, "name")
		if fields, ok := gqlTypeFields[name]; ok {
			return &gqlObject{kind: "Type", data: gqlTypeDef{name: name, fields: fields}}, nil
		}
		if enums, ok := gqlTypeEnums[name]; ok {
			return &gqlObject{kind: "Type", data: gqlTypeDef{name: name, enumValues: enums}}, nil
		}
		return nil, nil
	}
	return nil, fmt.Errorf("Cannot query field %q on type %q.", f.name, "Query")
}

func (e *gqlExec) resolveRepository(repo *Repo, f *gqlField) (any, error) {
	switch f.name {
	case "id":
		return gid("R", repo.ID), nil
	case "databaseId":
		return repo.ID, nil
	case "name":
		return repo.Name, nil
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name, nil
	case "owner":
		return &gqlObject{kind: "User", data: userJSON{Login: repo.Owner, ID: viewerID, Type: "User"}}, nil
	case "defaultBranchRef":
		if repo.headOID() == "" {
			return nil, nil
		}
		return &gqlObject{kind: "Ref", data: repo.DefaultBranch}, nil
	case "viewerPermission":
		return "ADMIN", nil
	case "hasIssuesEnabled":
		return true, nil
	case "issue":
		num, ok := e.argInt(f, "number")
		if !ok {
			return nil, nil
		}
		iss := repo.Issues[num]
		if iss == nil {
			return nil, nil
		}
		return &gqlObject{kind: "Issue", data: iss}, nil
	case "issues":
		return e.resolveIssuesConn(repo, f), nil
	case "pullRequests":
		first, hasFirst := e.argInt(f, "first")
		out := make([]*gqlObject, 0, len(repo.PullOrder))
		for _, pr := range repo.PullOrder {
			out = append(out, &gqlObject{kind: "PullRequest", data: pr})
		}
		if hasFirst && len(out) > first {
			out = out[:first]
		}
		return &gqlObject{kind: "PullRequestConnection", data: out}, nil
	case "pullRequest":
		num, ok := e.argInt(f, "number")
		if !ok {
			return nil, nil
		}
		pr := repo.Pulls[num]
		if pr == nil {
			return nil, nil
		}
		return &gqlObject{kind: "PullRequest", data: pr}, nil
	}
	return nil, fmt.Errorf("Cannot query field %q on type %q.", f.name, "Repository")
}

func (e *gqlExec) resolveIssuesConn(repo *Repo, f *gqlField) *gqlObject {
	// Base list: creation-ascending.
	list := make([]*Issue, 0, len(repo.IssueOrder))
	list = append(list, repo.IssueOrder...)
	// states filter: [OPEN] / [CLOSED].
	if raw, ok := e.argAny(f, "states"); ok {
		if arr, ok := raw.([]any); ok && len(arr) > 0 {
			want := make(map[string]bool)
			for _, v := range arr {
				if s, ok := v.(string); ok {
					want[strings.ToLower(s)] = true
				}
			}
			filtered := list[:0]
			for _, iss := range list {
				if want[iss.State] {
					filtered = append(filtered, iss)
				}
			}
			list = filtered
		}
	}
	// orderBy {field: CREATED_AT, direction: DESC}.
	if raw, ok := e.argAny(f, "orderBy"); ok {
		if m, ok := raw.(map[string]any); ok {
			dir, _ := m["direction"].(string)
			if strings.ToUpper(dir) == "DESC" {
				for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
					list[i], list[j] = list[j], list[i]
				}
			}
		}
	}
	// Cursor pagination.
	offset := 0
	if cur, ok := e.argString(f, "after"); ok && cur != "" {
		offset = offsetOf(cur)
	}
	first, hasFirst := e.argInt(f, "first")
	if offset > len(list) {
		offset = len(list)
	}
	end := len(list)
	if hasFirst && offset+first < end {
		end = offset + first
	}
	page := list[offset:end]
	nodes := make([]*gqlObject, 0, len(page))
	for _, iss := range page {
		nodes = append(nodes, &gqlObject{kind: "Issue", data: iss})
	}
	info := gqlPageInfo{hasNextPage: end < len(list)}
	if len(page) > 0 {
		info.endCursor = cursorFor(offset + len(page))
	}
	return &gqlObject{kind: "IssueConnection", data: &gqlIssueConn{nodes: nodes, page: info}}
}

func (e *gqlExec) resolveIssue(iss *Issue, f *gqlField) (any, error) {
	switch f.name {
	case "id":
		// Global id needs the owning repo; find it via the store order.
		for _, r := range e.s.store.Order {
			if r.Issues[iss.Number] == iss {
				return gid("I", r.ID, iss.Number), nil
			}
		}
		return gid("I", 0, iss.Number), nil
	case "number":
		return iss.Number, nil
	case "title":
		return iss.Title, nil
	case "state":
		if iss.State == "closed" {
			return "CLOSED", nil
		}
		return "OPEN", nil
	case "body":
		return iss.Body, nil
	}
	return nil, fmt.Errorf("Cannot query field %q on type %q.", f.name, "Issue")
}

func (e *gqlExec) resolvePull(pr *Pull, f *gqlField) (any, error) {
	switch f.name {
	case "id":
		for _, r := range e.s.store.Order {
			if r.Pulls[pr.Number] == pr {
				return gid("P", r.ID, pr.Number), nil
			}
		}
		return gid("P", 0, pr.Number), nil
	case "number":
		return pr.Number, nil
	case "title":
		return pr.Title, nil
	case "body":
		return pr.Body, nil
	case "state":
		if pr.Merged {
			return "MERGED", nil
		}
		if pr.State == "closed" {
			return "CLOSED", nil
		}
		return "OPEN", nil
	case "merged":
		return pr.Merged, nil
	case "headRefName":
		return pr.HeadRef, nil
	case "baseRefName":
		return pr.BaseRef, nil
	case "mergeable":
		// Computed from the owning repo's current base tip.
		for _, r := range e.s.store.Order {
			if r.Pulls[pr.Number] == pr {
				conflicted, _ := r.prConflict(pr)
				if conflicted {
					return "CONFLICTING", nil
				}
				return "MERGEABLE", nil
			}
		}
		return "UNKNOWN", nil
	case "files":
		first, hasFirst := e.argInt(f, "first")
		nodes := make([]*gqlObject, 0, len(pr.Files))
		for _, fl := range pr.Files {
			nodes = append(nodes, &gqlObject{kind: "File", data: fl.Filename})
		}
		if hasFirst && len(nodes) > first {
			nodes = nodes[:first]
		}
		return &gqlObject{kind: "FileConnection", data: nodes}, nil
	}
	return nil, fmt.Errorf("Cannot query field %q on type %q.", f.name, "PullRequest")
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

func (e *gqlExec) resolveMutation(f *gqlField) (any, error) {
	input, _ := e.argAny(f, "input")
	in, _ := input.(map[string]any)
	strArg := func(key string) string {
		if in == nil {
			return ""
		}
		s, _ := in[key].(string)
		return s
	}
	switch f.name {
	case "createIssue":
		repo := e.repoByGID(strArg("repositoryId"))
		if repo == nil {
			return nil, fmt.Errorf("repository not found")
		}
		iss := e.s.store.newIssue(repo, strArg("title"), strArg("body"), nil)
		return &gqlObject{kind: "CreateIssuePayload", data: &gqlObject{kind: "Issue", data: iss}}, nil
	case "closeIssue":
		_, iss := e.issueByGID(strArg("issueId"))
		if iss == nil {
			return nil, fmt.Errorf("issue not found")
		}
		iss.State = "closed"
		return &gqlObject{kind: "CloseIssuePayload", data: &gqlObject{kind: "Issue", data: iss}}, nil
	case "createPullRequest":
		repo := e.repoByGID(strArg("repositoryId"))
		if repo == nil {
			return nil, fmt.Errorf("repository not found")
		}
		head, base := strArg("headRefName"), strArg("baseRefName")
		headSHA := repo.Refs["refs/heads/"+head]
		baseSHA := repo.Refs["refs/heads/"+base]
		if headSHA == "" || baseSHA == "" {
			return nil, fmt.Errorf("head or base branch does not exist")
		}
		mb := repo.mergeBase(headSHA, baseSHA)
		var mbTree string
		if mb != "" {
			mbTree = repo.commitTree(mb)
		}
		files := repo.diffFiles(mbTree, repo.commitTree(headSHA))
		pr := e.s.store.newPull(repo, strArg("title"), strArg("body"), head, base, headSHA, baseSHA, files)
		return &gqlObject{kind: "CreatePullRequestPayload", data: &gqlObject{kind: "PullRequest", data: pr}}, nil
	case "mergePullRequest":
		_, pr := e.pullByGID(strArg("pullRequestId"))
		if pr == nil {
			return nil, fmt.Errorf("pull request not found")
		}
		var repo *Repo
		for _, r := range e.s.store.Order {
			if r.Pulls[pr.Number] == pr {
				repo = r
				break
			}
		}
		if repo == nil {
			return nil, fmt.Errorf("pull request not found")
		}
		if pr.Merged {
			return &gqlObject{kind: "MergePullRequestPayload", data: &gqlObject{kind: "PullRequest", data: pr}}, nil
		}
		conflicted, resultFlat := repo.prConflict(pr)
		if conflicted {
			return nil, fmt.Errorf("merge conflict")
		}
		baseTip := repoBaseTip(repo, pr)
		resultTree := repo.buildTreeFromFlat(resultFlat)
		msg := "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + pr.HeadRef
		mergeOID := repo.storeObject(ObjCommit, buildCommit(resultTree, []string{baseTip}, msg))
		repo.Refs["refs/heads/"+pr.BaseRef] = mergeOID
		pr.State = "closed"
		pr.Merged = true
		pr.MergedAt = "2026-11-20T00:00:00Z"
		pr.MergeSHA = mergeOID
		pr.BaseSHA = mergeOID
		return &gqlObject{kind: "MergePullRequestPayload", data: &gqlObject{kind: "PullRequest", data: pr}}, nil
	case "addComment":
		_, iss := e.issueByGID(strArg("subjectId"))
		if iss == nil {
			return nil, fmt.Errorf("issue not found")
		}
		var repo *Repo
		for _, r := range e.s.store.Order {
			if r.Issues[iss.Number] == iss {
				repo = r
				break
			}
		}
		if repo == nil {
			return nil, fmt.Errorf("issue not found")
		}
		c := &Comment{ID: repo.NextCommentID, Body: strArg("body"), RepoID: repo.ID, IssueNum: iss.Number}
		repo.NextCommentID++
		iss.Comments = append(iss.Comments, c)
		edge := &gqlObject{kind: "CommentEdge", data: &gqlObject{kind: "Comment", data: c}}
		return &gqlObject{kind: "AddCommentPayload", data: edge}, nil
	}
	return nil, fmt.Errorf("Cannot query field %q on type %q.", f.name, "Mutation")
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

func (s *server) handleGraphQL(req *request, keepAlive bool) []byte {
	// Batching: a JSON array body answers a JSON array, in order.
	trimmed := strings.TrimSpace(string(req.body))
	if strings.HasPrefix(trimmed, "[") {
		var batch []json.RawMessage
		if err := json.Unmarshal(req.body, &batch); err != nil {
			return jsonError(400, "malformed JSON body", keepAlive)
		}
		results := make([]any, 0, len(batch))
		for _, item := range batch {
			results = append(results, s.execGraphQL(item))
		}
		return jsonResponse(200, marshal(results), keepAlive)
	}
	return jsonResponse(200, marshal(s.execGraphQL(req.body)), keepAlive)
}

func (s *server) execGraphQL(body []byte) map[string]any {
	var doc struct {
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
		OperationName string         `json:"operationName"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return map[string]any{"errors": []map[string]any{{"message": "malformed JSON body"}}}
	}
	toks, err := gqlLex(doc.Query)
	if err != nil {
		return map[string]any{"errors": []map[string]any{{"message": err.Error()}}}
	}
	p := &gqlParser{toks: toks}
	ops, frags, err := p.parseDocument()
	if err != nil {
		return map[string]any{"errors": []map[string]any{{"message": err.Error()}}}
	}
	if len(ops) == 0 {
		return map[string]any{"errors": []map[string]any{{"message": "no operation"}}}
	}
	op := ops[0]
	if doc.OperationName != "" {
		for _, o := range ops {
			if o.name == doc.OperationName {
				op = o
				break
			}
		}
	}
	vars := doc.Variables
	if vars == nil {
		vars = make(map[string]any)
	}
	// Apply variable defaults.
	for name, def := range op.varDefs {
		if _, ok := vars[name]; !ok && def != nil {
			vars[name] = evalValue(def, nil)
		}
	}
	e := &gqlExec{s: s, vars: vars, fragments: frags}
	rootKind := "Query"
	if op.kind == "mutation" {
		rootKind = "Mutation"
	}
	root := &gqlObject{kind: rootKind}
	data := e.executeSelection(root, op.sel)
	out := map[string]any{"data": data}
	if len(e.errors) > 0 {
		out["errors"] = e.errors
	}
	return out
}
