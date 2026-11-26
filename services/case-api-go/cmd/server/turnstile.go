// Author: Patrick Munis at newwave
package main

// Cloudflare Turnstile on the public forms (2026-11-26): filing a dispute and registering a TPA or a health plan.
// The page shows the widget when GET /api/public/turnstile gives a site key, and sends the token it gets in the
// X-Turnstile-Token header; the server checks it with Cloudflare before anything is filed. With no secret
// (TURNSTILE_SECRET unset) the check is off, so nothing changes until the keys are set.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var turnstileClient = &http.Client{Timeout: 8 * time.Second}

const turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// GET /api/public/turnstile: the site key for the page (empty = the check is off).
func (s *server) publicTurnstile(w http.ResponseWriter, r *http.Request) {
	key := ""
	if os.Getenv("TURNSTILE_SECRET") != "" {
		key = strings.TrimSpace(os.Getenv("TURNSTILE_SITE_KEY"))
	}
	writeJSON(w, http.StatusOK, map[string]string{"site_key": key})
}

// turnstilePassed: true when the check is off, or Cloudflare accepts the request's token. On false it has answered.
func turnstilePassed(w http.ResponseWriter, r *http.Request) bool {
	secret := os.Getenv("TURNSTILE_SECRET")
	if secret == "" {
		return true
	}
	token := strings.TrimSpace(r.Header.Get("X-Turnstile-Token"))
	if token == "" || len(token) > 2048 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "please complete the “I am human” check on the form, then send it again"})
		return false
	}
	resp, err := turnstileClient.PostForm(turnstileVerifyURL, url.Values{"secret": {secret}, "response": {token}, "remoteip": {clientIP(r)}})
	if err != nil {
		// Cloudflare unreachable: let the person through rather than lose a filing; the per-address limits still apply.
		return true
	}
	defer resp.Body.Close()
	var out struct {
		Success bool     `json:"success"`
		Codes   []string `json:"error-codes"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || !out.Success {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the “I am human” check did not pass or has expired — complete it again, then send the form"})
		return false
	}
	return true
}
