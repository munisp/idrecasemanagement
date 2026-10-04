package main

// Minimal RESP (Redis Serialization Protocol) client — stdlib only, no external
// dependency. Used for:
//   1. JWKS cache: Keycloak's signing keys are cached in Redis with a TTL so
//      every replica shares one fetch and key rotation propagates in minutes.
//   2. Idempotency keys: POST /cases/initiate accepts an Idempotency-Key header;
//      the resulting case_id is stored in Redis for 24h so retries (mobile
//      networks, PWA offline queue replays) return the original case instead
//      of creating a duplicate dispute.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

type redisClient struct{ addr, password string }

func newRedis(addr, password string) *redisClient { return &redisClient{addr: addr, password: password} }

func writeCmd(w io.Writer, args ...string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	_, err := w.Write([]byte(b.String()))
	return err
}

func readReply(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) == 0 {
		return "", errors.New("empty reply")
	}
	switch line[0] {
	case '+': // simple string
		return strings.TrimSpace(line[1:]), nil
	case '-': // error
		return "", errors.New(strings.TrimSpace(line[1:]))
	case ':': // integer
		return strings.TrimSpace(line[1:]), nil
	case '$': // bulk string
		n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
		if n < 0 {
			return "", errRedisNil
		}
		buf := make([]byte, n+2)
		if _, err := readFull(r, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	default:
		return "", fmt.Errorf("unsupported reply type %q", line[0])
	}
}

// cmd opens one connection per call and, when a password is configured,
// AUTHs on that same connection before the real command. The shared cluster
// Redis requires AUTH (confirmed live against the same cluster this app
// also deploys to: unauthenticated SET/GET get "NOAUTH Authentication
// required"); this client never sent one before, so every command silently
// failed -- see setnx's fail-open comment below for what that broke.
func (rc *redisClient) cmd(args ...string) (string, error) {
	if rc.addr == "" {
		return "", errors.New("redis disabled")
	}
	conn, err := net.DialTimeout("tcp", rc.addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	r := bufio.NewReader(conn)

	if rc.password != "" {
		if err := writeCmd(conn, "AUTH", rc.password); err != nil {
			return "", err
		}
		if _, err := readReply(r); err != nil {
			return "", fmt.Errorf("redis auth: %w", err)
		}
	}
	if err := writeCmd(conn, args...); err != nil {
		return "", err
	}
	return readReply(r)
}

var errRedisNil = errors.New("redis: nil")

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// get returns "" when the key is missing or Redis is unavailable (fail-open:
// Redis is a cache, never on the critical path of correctness).
func (rc *redisClient) get(key string) string {
	v, err := rc.cmd("GET", key)
	if err != nil {
		return ""
	}
	return v
}

// setex stores a value with a TTL in seconds; failures are ignored (fail-open).
func (rc *redisClient) setex(key string, ttlSeconds int, value string) {
	_, _ = rc.cmd("SET", key, value, "EX", strconv.Itoa(ttlSeconds))
}

// setnx is an atomic set-if-not-exists; returns true when this caller won --
// INCLUDING when Redis itself errored (down, auth failure, timeout). Fail
// open, matching get/setex's documented philosophy: only a genuine NX
// conflict (SET..NX's nil bulk reply) means someone else is really
// mid-create and should return false/409.
func (rc *redisClient) setnx(key string, ttlSeconds int, value string) bool {
	_, err := rc.cmd("SET", key, value, "EX", strconv.Itoa(ttlSeconds), "NX")
	if err == nil {
		return true // "OK": we won
	}
	if errors.Is(err, errRedisNil) {
		return false // genuine conflict: key already exists
	}
	return true // redis unreachable/erroring: fail open
}
