package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Minimal HTTP client for the env lifecycle (register / done polling).
// No net/http here: the whole server speaks raw sockets, and this client is
// a few dozen lines over net.Dial.
// ---------------------------------------------------------------------------

type envClient struct {
	hostport string
}

// httpDo performs one request against the env and returns status + body.
func (e *envClient) httpDo(method, path, body string) (int, []byte, error) {
	conn, err := net.DialTimeout("tcp", e.hostport, 5*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var sb strings.Builder
	sb.WriteString(method + " " + path + " HTTP/1.1\r\n")
	sb.WriteString("Host: " + e.hostport + "\r\n")
	sb.WriteString("Content-Type: application/json\r\n")
	sb.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	sb.WriteString("Connection: close\r\n\r\n")
	sb.WriteString(body)
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		return 0, nil, err
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	if len(parts) < 2 {
		return 0, nil, fmt.Errorf("bad status line %q", statusLine)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, nil, err
	}
	contentLength := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if idx := strings.Index(line, ":"); idx >= 0 {
			if strings.EqualFold(strings.TrimSpace(line[:idx]), "Content-Length") {
				contentLength, _ = strconv.Atoi(strings.TrimSpace(line[idx+1:]))
			}
		}
	}
	var resp []byte
	if contentLength >= 0 {
		resp = make([]byte, contentLength)
		if _, err := readFull(br, resp); err != nil {
			return 0, nil, err
		}
	} else {
		// No length: read until the server closes.
		buf := make([]byte, 4096)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				resp = append(resp, buf[:n]...)
			}
			if err != nil {
				break
			}
		}
	}
	return status, resp, nil
}

func readFull(br *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := br.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// parseEnvURL splits MAKEFUN_ENV_URL into its host:port.
func parseEnvURL(raw string) (string, error) {
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "https://")
	if idx := strings.IndexByte(raw, '/'); idx >= 0 {
		raw = raw[:idx]
	}
	if raw == "" {
		return "", fmt.Errorf("empty MAKEFUN_ENV_URL host")
	}
	return raw, nil
}

// register announces our port to the env, retrying with exponential backoff
// (250 ms -> 2 s cap) for up to 30 s. 200/409 means registered.
func (e *envClient) register(port int) error {
	body := `{"port":` + strconv.Itoa(port) + `}`
	deadline := time.Now().Add(30 * time.Second)
	delay := 250 * time.Millisecond
	for {
		status, _, err := e.httpDo("POST", "/register", body)
		if err == nil {
			switch status {
			case 200, 409:
				return nil
			case 400, 405:
				return fmt.Errorf("register refused with HTTP %d", status)
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("register failed: %w", err)
			}
			return fmt.Errorf("register failed with HTTP %d", status)
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// pollDone long-polls GET /done until the env reports done, then exits 0.
func (e *envClient) pollDone() {
	for {
		status, body, err := e.httpDo("GET", "/done", "")
		if err == nil && status == 200 {
			if strings.Contains(string(body), `"done":true`) || strings.Contains(string(body), `"done": true`) {
				os.Exit(0)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	// One OS thread for the event loop: the request path is strictly
	// single-threaded (only the done-poll goroutine runs beside it, and it
	// blocks on the network almost all the time).
	runtime.GOMAXPROCS(1)

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	hostport, err := parseEnvURL(envURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv := &server{store: NewStore(), token: os.Getenv("MAKEFUN_SHOP_TOKEN")}

	// Ephemeral listener on loopback.
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		panic(err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		panic(err)
	}
	sa := &syscall.SockaddrInet4{Port: 0, Addr: [4]byte{127, 0, 0, 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		panic(err)
	}
	if err := syscall.Listen(fd, 4096); err != nil {
		panic(err)
	}
	lsa, err := syscall.Getsockname(fd)
	if err != nil {
		panic(err)
	}
	port := lsa.(*syscall.SockaddrInet4).Port

	env := &envClient{hostport: hostport}
	if err := env.register(port); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go env.pollDone()
	srv.serve(fd)
}
