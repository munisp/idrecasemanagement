// Author: Patrick Munis at newwave
package main

// API rate limits (2026-11-25). Every caller gets a fixed one-minute budget, counted in Redis so all replicas share it:
//
//   - a partner API key (a Keycloak service account, api_keys.go)   API_RATE_LIMIT_KEY_PER_MIN    default 120
//   - a signed-in person (portal staff, TPA and plan users)          API_RATE_LIMIT_USER_PER_MIN   default 600
//   - an anonymous caller of /api/public/*, per client address       API_RATE_LIMIT_PUBLIC_PER_MIN default 120
//
// 0 turns that limit off. The platform's own workers (SERVICE_WORKER) are never limited, and API_RATE_LIMIT_EXEMPT
// lists usernames or key client ids that are not (a load-test key, for one). The endpoints that already have their
// own tighter limits (filing, registration, the e-mail check, ShareBox) keep them; this is the ceiling over all of it.
//
// Answers carry X-RateLimit-Limit / -Remaining / -Reset; a refused request gets 429 with Retry-After. If Redis is
// unreachable the request goes through: an outage of the counter must not take the API down with it.

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type apiLimits struct {
	key, user, public int64
	exempt            map[string]bool
}

func loadAPILimits() apiLimits {
	n := func(name string, def int64) int64 {
		if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64); err == nil && v >= 0 {
			return v
		}
		return def
	}
	l := apiLimits{key: n("API_RATE_LIMIT_KEY_PER_MIN", 120), user: n("API_RATE_LIMIT_USER_PER_MIN", 600),
		public: n("API_RATE_LIMIT_PUBLIC_PER_MIN", 120), exempt: map[string]bool{}}
	for _, x := range strings.Split(os.Getenv("API_RATE_LIMIT_EXEMPT"), ",") {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
			l.exempt[x] = true
		}
	}
	return l
}

// rateWindow counts one request against key in the current one-minute window: the count so far and when the window
// ends. ok is false when Redis could not be asked.
func (s *server) rateWindow(key string) (n int64, reset time.Time, ok bool) {
	now := time.Now()
	win := now.Unix() / 60
	reset = time.Unix((win+1)*60, 0)
	k := fmt.Sprintf("%s:rl:api:%s:%d", s.cfg.RedisPrefix, key, win)
	res, err := s.rds.cmd("INCR", k)
	if err != nil {
		return 0, reset, false
	}
	if _, err := fmt.Sscanf(res, "%d", &n); err != nil {
		return 0, reset, false
	}
	if n == 1 {
		_, _ = s.rds.cmd("EXPIRE", k, "120")
	}
	return n, reset, true
}

// limitResponse sets the headers and, past the limit, answers 429. It reports whether the request may go on.
func limitResponse(w http.ResponseWriter, limit, n int64, reset time.Time, who string) bool {
	left := limit - n
	if left < 0 {
		left = 0
	}
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.FormatInt(limit, 10))
	h.Set("X-RateLimit-Remaining", strconv.FormatInt(left, 10))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	if n <= limit {
		return true
	}
	wait := int64(time.Until(reset).Seconds()) + 1
	h.Set("Retry-After", strconv.FormatInt(wait, 10))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":       fmt.Sprintf("too many requests: %s may make %d a minute; try again in %d seconds", who, limit, wait),
		"retry_after": wait,
	})
	return false
}

// apiRateLimit: the per-caller limit on the signed-in API (after authentication, so it knows who is calling).
func (s *server) apiRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := r.Context().Value(ctxPrincipal{}).(principal)
		limit, who := s.limits.user, "a signed-in user"
		if isAPIKeyPrincipal(p) {
			limit, who = s.limits.key, "an API key"
		}
		name := strings.ToLower(p.Username)
		if limit == 0 || p.Subject == "" || contains(p.Roles, serviceRole) || s.limits.exempt[name] ||
			s.limits.exempt[strings.TrimPrefix(name, "service-account-")] {
			next.ServeHTTP(w, r)
			return
		}
		n, reset, ok := s.rateWindow("sub:" + p.Subject)
		if !ok {
			next.ServeHTTP(w, r) // fail open
			return
		}
		if limitResponse(w, limit, n, reset, who) {
			next.ServeHTTP(w, r)
			return
		}
		rateLimited.WithLabelValues(map[bool]string{true: "api_key", false: "user"}[isAPIKeyPrincipal(p)]).Inc()
	})
}

// publicRateLimit: the per-address limit on the anonymous /api/public/* endpoints.
func (s *server) publicRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.limits.public == 0 || !strings.HasPrefix(r.URL.Path, "/api/public/") {
			next.ServeHTTP(w, r)
			return
		}
		n, reset, ok := s.rateWindow("ip:" + clientIP(r))
		if !ok || limitResponse(w, s.limits.public, n, reset, "one address") {
			next.ServeHTTP(w, r)
			return
		}
		rateLimited.WithLabelValues("public").Inc()
	})
}
