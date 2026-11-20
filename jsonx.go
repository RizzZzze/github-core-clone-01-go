package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

// jsonString renders a JSON string literal (with quotes), escaping as needed.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte("0123456789abcdef"[byte(r)>>4])
				b.WriteByte("0123456789abcdef"[byte(r)&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jsonObj is a decoded JSON object with small typed accessors. Bodies in this
// service are small; encoding/json is fast enough and allocation-cheap here.
type jsonObj map[string]any

func decodeJSON(body []byte) (jsonObj, bool) {
	if len(body) == 0 {
		return nil, false
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return jsonObj(m), true
}

func (o jsonObj) str(key string) (string, bool) {
	v, ok := o[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func (o jsonObj) strDef(key, def string) string {
	if s, ok := o.str(key); ok {
		return s
	}
	return def
}

func (o jsonObj) boolDef(key string, def bool) bool {
	v, ok := o[key]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

func (o jsonObj) num(key string) (int64, bool) {
	v, ok := o[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		return int64(n), true
	}
	return 0, false
}

func (o jsonObj) arr(key string) ([]any, bool) {
	v, ok := o[key]
	if !ok {
		return nil, false
	}
	a, ok := v.([]any)
	return a, ok
}

func (o jsonObj) obj(key string) (jsonObj, bool) {
	v, ok := o[key]
	if !ok {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return jsonObj(m), true
}

func (o jsonObj) strArr(key string) []string {
	a, ok := o.arr(key)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// intStr renders an int without allocation churn (strconv wrapper).
func intStr(n int) string { return strconv.Itoa(n) }
