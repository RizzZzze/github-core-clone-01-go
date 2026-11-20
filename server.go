package main

import (
	"bytes"
	"strconv"
	"strings"
	"syscall"
)

// ---------------------------------------------------------------------------
// Request model
// ---------------------------------------------------------------------------

type request struct {
	method  string
	path    string // path without query, still percent-encoded as sent
	query   string
	body    []byte
	// headers we care about (lowercased values where relevant)
	contentLength int64
	chunked       bool
	authorization string
	gitProtocol   string
	connection    string
	contentType   string
	accept        string
	host          string
	http11        bool
	closeAfter    bool
}

// tryParseRequest parses one complete HTTP request from buf. It returns the
// request, the bytes consumed, and ok=false when more data is needed.
func tryParseRequest(buf []byte) (*request, int, bool) {
	hdrEnd := indexOf(buf, []byte("\r\n\r\n"))
	if hdrEnd < 0 {
		if len(buf) > 65536 {
			return nil, 0, false // oversized headers: caller closes
		}
		return nil, 0, false
	}
	head := buf[:hdrEnd]
	bodyStart := hdrEnd + 4
	// Request line.
	lineEnd := indexOf(head, []byte("\r\n"))
	if lineEnd < 0 {
		return nil, 0, false
	}
	line := string(head[:lineEnd])
	sp1 := strings.IndexByte(line, ' ')
	sp2 := strings.LastIndexByte(line, ' ')
	if sp1 < 0 || sp2 <= sp1 {
		return nil, 0, false
	}
	req := &request{
		method:        line[:sp1],
		contentLength: -1,
		http11:        strings.HasSuffix(line, "HTTP/1.1"),
	}
	target := line[sp1+1 : sp2]
	if q := strings.IndexByte(target, '?'); q >= 0 {
		req.path = target[:q]
		req.query = target[q+1:]
	} else {
		req.path = target
	}
	// Headers.
	rest := head[lineEnd+2:]
	for len(rest) > 0 {
		eol := indexOf(rest, []byte("\r\n"))
		var hline []byte
		if eol < 0 {
			hline = rest
			rest = nil
		} else {
			hline = rest[:eol]
			rest = rest[eol+2:]
		}
		colon := indexByte(hline, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(string(hline[:colon]))
		value := strings.TrimSpace(string(hline[colon+1:]))
		switch name {
		case "content-length":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				req.contentLength = n
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(value), "chunked") {
				req.chunked = true
			}
		case "authorization":
			req.authorization = value
		case "git-protocol":
			req.gitProtocol = value
		case "connection":
			req.connection = strings.ToLower(value)
		case "content-type":
			req.contentType = value
		case "accept":
			req.accept = value
		case "host":
			req.host = value
		}
	}
	// Keep-alive: HTTP/1.1 defaults to keep-alive; HTTP/1.0 defaults close.
	if req.http11 {
		req.closeAfter = req.connection == "close"
	} else {
		req.closeAfter = req.connection != "keep-alive"
	}
	if req.chunked {
		body, consumed, done := decodeChunked(buf[bodyStart:])
		if !done {
			return nil, 0, false
		}
		req.body = body
		return req, bodyStart + consumed, true
	}
	if req.contentLength < 0 {
		req.body = nil
		return req, bodyStart, true
	}
	if req.contentLength > maxBodyBytes {
		return nil, 0, false // caller turns this into 413 via header peek
	}
	if int64(len(buf)-bodyStart) < req.contentLength {
		return nil, 0, false
	}
	n := int(req.contentLength)
	req.body = buf[bodyStart : bodyStart+n]
	return req, bodyStart + n, true
}

// maxBodyBytes caps a single request body (packs in this suite are < 1 MB;
// 64 MB leaves ample headroom inside the 1024 MB pod).
const maxBodyBytes = 64 << 20

// decodeChunked decodes a chunked body present in buf. Returns the decoded
// body, bytes consumed from buf, and whether the terminal chunk was seen.
func decodeChunked(buf []byte) ([]byte, int, bool) {
	var body []byte
	pos := 0
	for {
		eol := indexOf(buf[pos:], []byte("\r\n"))
		if eol < 0 {
			return nil, 0, false
		}
		sizeLine := string(buf[pos : pos+eol])
		if semi := strings.IndexByte(sizeLine, ';'); semi >= 0 {
			sizeLine = sizeLine[:semi]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
		if err != nil || size < 0 {
			return nil, 0, false
		}
		pos += eol + 2
		if size == 0 {
			// Trailers end at an empty line.
			end := indexOf(buf[pos:], []byte("\r\n"))
			if end < 0 {
				return nil, 0, false
			}
			pos += end + 2
			return body, pos, true
		}
		if int64(len(buf)-pos) < size+2 {
			return nil, 0, false
		}
		body = append(body, buf[pos:pos+int(size)]...)
		pos += int(size) + 2 // skip chunk data + trailing CRLF
	}
}

func indexOf(b, sub []byte) int {
	return bytes.Index(b, sub)
}

// ---------------------------------------------------------------------------
// Response building
// ---------------------------------------------------------------------------

var statusTexts = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	413: "Content Too Large",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

var reqIDCounter uint64

func nextReqID() string {
	reqIDCounter++
	// Fixed-width hex counter: unique per response, cheap to render.
	var b [16]byte
	v := reqIDCounter
	for i := 15; i >= 0; i-- {
		b[i] = "0123456789abcdef"[v&0xf]
		v >>= 4
	}
	return string(b[:])
}

// response renders a full HTTP/1.1 response. body may be nil.
func response(status int, contentType string, body []byte, keepAlive bool) []byte {
	text, ok := statusTexts[status]
	if !ok {
		text = "Status"
	}
	var out []byte
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(status), 10)
	out = append(out, ' ')
	out = append(out, text...)
	out = append(out, "\r\nContent-Type: "...)
	out = append(out, contentType...)
	out = append(out, "\r\nContent-Length: "...)
	out = strconv.AppendInt(out, int64(len(body)), 10)
	out = append(out, "\r\nX-GitHub-Request-Id: "...)
	out = append(out, nextReqID()...)
	if !keepAlive {
		out = append(out, "\r\nConnection: close"...)
	}
	out = append(out, "\r\n\r\n"...)
	out = append(out, body...)
	return out
}

func jsonResponse(status int, body []byte, keepAlive bool) []byte {
	return response(status, "application/json", body, keepAlive)
}

// responseEx renders a response with extra raw header lines (each already
// in "Name: value" form, without CRLF).
func responseEx(status int, contentType string, body []byte, keepAlive bool, extra []string) []byte {
	text, ok := statusTexts[status]
	if !ok {
		text = "Status"
	}
	var out []byte
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(status), 10)
	out = append(out, ' ')
	out = append(out, text...)
	out = append(out, "\r\nContent-Type: "...)
	out = append(out, contentType...)
	out = append(out, "\r\nContent-Length: "...)
	out = strconv.AppendInt(out, int64(len(body)), 10)
	out = append(out, "\r\nX-GitHub-Request-Id: "...)
	out = append(out, nextReqID()...)
	for _, h := range extra {
		out = append(out, "\r\n"...)
		out = append(out, h...)
	}
	if !keepAlive {
		out = append(out, "\r\nConnection: close"...)
	}
	out = append(out, "\r\n\r\n"...)
	out = append(out, body...)
	return out
}

func jsonError(status int, msg string, keepAlive bool) []byte {
	return jsonResponse(status, []byte(`{"message":`+jsonString(msg)+`}`), keepAlive)
}

// ---------------------------------------------------------------------------
// Connection state machine + epoll loop
// ---------------------------------------------------------------------------

type conn struct {
	fd         int
	rbuf       []byte // bytes received, not yet parsed
	wbuf       []byte // bytes to send
	woff       int    // bytes of wbuf already written
	closeAfter bool   // close once wbuf drains
	wantWrite  bool   // EPOLLOUT armed
	dead       bool
}

func (c *conn) close(epfd, fd int) {
	if c.dead {
		return
	}
	c.dead = true
	syscall.EpollCtl(epfd, syscall.EPOLL_CTL_DEL, fd, nil)
	syscall.Close(fd)
}

// bodyTooLarge peeks at a complete header block and reports whether its
// Content-Length exceeds the cap.
func bodyTooLarge(buf []byte) bool {
	hdrEnd := indexOf(buf, []byte("\r\n\r\n"))
	if hdrEnd < 0 {
		return false
	}
	head := strings.ToLower(string(buf[:hdrEnd]))
	idx := strings.Index(head, "content-length:")
	if idx < 0 {
		return false
	}
	rest := head[idx+len("content-length:"):]
	eol := strings.Index(rest, "\r\n")
	if eol >= 0 {
		rest = rest[:eol]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
	return err == nil && n > maxBodyBytes
}

type server struct {
	store *Store
	token string // MAKEFUN_SHOP_TOKEN; empty means accept any non-empty bearer
}

// serve runs the single-threaded epoll event loop on an already-listening
// socket. It never returns (the done-poll goroutine exits the process).
func (s *server) serve(listener int) {
	epfd, err := syscall.EpollCreate1(0)
	if err != nil {
		panic(err)
	}
	var ev syscall.EpollEvent
	ev.Events = syscall.EPOLLIN
	ev.Fd = int32(listener)
	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, listener, &ev); err != nil {
		panic(err)
	}
	conns := make(map[int]*conn)
	events := make([]syscall.EpollEvent, 256)
	readBuf := make([]byte, 65536)
	for {
		n, err := syscall.EpollWait(epfd, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			panic(err)
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			evs := events[i].Events
			if fd == listener {
				// Accept until EAGAIN.
				for {
					nfd, _, err := syscall.Accept4(listener, syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC)
					if err != nil {
						break
					}
					syscall.SetsockoptInt(nfd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
					c := &conn{fd: nfd}
					conns[nfd] = c
					var cev syscall.EpollEvent
					cev.Events = syscall.EPOLLIN
					cev.Fd = int32(nfd)
					if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, nfd, &cev); err != nil {
						delete(conns, nfd)
						syscall.Close(nfd)
					}
				}
				continue
			}
			c := conns[fd]
			if c == nil || c.dead {
				continue
			}
			if evs&(syscall.EPOLLHUP|syscall.EPOLLERR) != 0 {
				c.close(epfd, fd)
				delete(conns, fd)
				continue
			}
			if evs&syscall.EPOLLIN != 0 {
				s.handleRead(epfd, c, readBuf)
				if c.dead {
					delete(conns, fd)
					continue
				}
			}
			if evs&syscall.EPOLLOUT != 0 && !c.dead {
				s.handleWrite(epfd, c)
				if c.dead {
					delete(conns, fd)
					continue
				}
			}
			if c.dead {
				delete(conns, fd)
			}
		}
	}
}

func (s *server) armWrite(epfd int, c *conn, want bool) {
	if c.wantWrite == want || c.dead {
		return
	}
	c.wantWrite = want
	var ev syscall.EpollEvent
	ev.Events = syscall.EPOLLIN
	if want {
		ev.Events |= syscall.EPOLLOUT
	}
	ev.Fd = int32(c.fd)
	syscall.EpollCtl(epfd, syscall.EPOLL_CTL_MOD, c.fd, &ev)
}

// handleRead drains the socket until EAGAIN, parses complete requests and
// dispatches them synchronously, then tries to flush the output.
func (s *server) handleRead(epfd int, c *conn, readBuf []byte) {
	for {
		n, err := syscall.Read(c.fd, readBuf)
		if n > 0 {
			c.rbuf = append(c.rbuf, readBuf[:n]...)
			// Parse and dispatch every complete request in the buffer.
			for {
				if len(c.rbuf) == 0 {
					break
				}
				if bodyTooLarge(c.rbuf) {
					c.wbuf = append(c.wbuf, jsonError(413, "request body too large", false)...)
					c.closeAfter = true
					break
				}
				req, consumed, ok := tryParseRequest(c.rbuf)
				if !ok {
					break
				}
				c.rbuf = c.rbuf[consumed:]
				resp := s.dispatch(req)
				c.wbuf = append(c.wbuf, resp...)
				if req.closeAfter {
					c.closeAfter = true
				}
			}
			// Compact the read buffer when fully consumed.
			if len(c.rbuf) == 0 {
				c.rbuf = c.rbuf[:0]
			}
			s.handleWrite(epfd, c)
			if c.dead {
				return
			}
			continue
		}
		if n == 0 {
			// Peer closed.
			c.close(epfd, c.fd)
			return
		}
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			return
		}
		if err == syscall.EINTR {
			continue
		}
		c.close(epfd, c.fd)
		return
	}
}

// handleWrite flushes pending output until EAGAIN or drained.
func (s *server) handleWrite(epfd int, c *conn) {
	for c.woff < len(c.wbuf) {
		n, err := syscall.Write(c.fd, c.wbuf[c.woff:])
		if n > 0 {
			c.woff += n
			continue
		}
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			s.armWrite(epfd, c, true)
			return
		}
		if err == syscall.EINTR {
			continue
		}
		c.close(epfd, c.fd)
		return
	}
	c.wbuf = c.wbuf[:0]
	c.woff = 0
	s.armWrite(epfd, c, false)
	if c.closeAfter {
		c.close(epfd, c.fd)
	}
}
