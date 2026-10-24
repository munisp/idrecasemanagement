package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Outbound mail at volume. The provider allows so many messages a second and so many a day; past either it turns
// mail away ("550 Too many requests", "550 ... daily email sending quota"), and a filing's payment link or a
// party's letter was simply lost. Now:
//   - every send is paced to MAIL_MAX_PER_SECOND (default 8) across all case-api replicas (a Redis counter per
//     second), so a burst never trips the per-second limit;
//   - a "too many requests" answer is retried in place, briefly;
//   - mail the provider still turns away for now -- quota, rate, the relay unreachable -- is kept in
//     public.mail_queue and sent again by the sweeper (a minute later for a rate limit, an hour for the quota),
//     for up to three days. The caller is told it was queued (errMailQueued), so the case says "queued", not "lost".
// A permanent refusal (a bad address) is returned as before.

var errMailQueued = errors.New("queued for retry")

type mailQueuedError struct{ reason string }

func (e *mailQueuedError) Error() string {
	return "queued for retry — the mail provider turned it away for now (" + e.reason + "); it is sent again automatically"
}
func (e *mailQueuedError) Unwrap() error { return errMailQueued }

const mailGiveUpAfter = 72 * time.Hour

func mailMaxPerSecond() int64 {
	if n, err := strconv.ParseInt(os.Getenv("MAIL_MAX_PER_SECOND"), 10, 64); err == nil && n > 0 {
		return n
	}
	return 8
}

// mailRetryAfter says whether an SMTP error is a "not now" (and when to try again) or a permanent refusal.
func mailRetryAfter(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "quota"):
		return time.Hour, true
	case strings.Contains(m, "too many requests"), strings.Contains(m, "rate limit"), strings.Contains(m, "try again later"):
		return time.Minute, true
	case strings.HasPrefix(m, "421"), strings.HasPrefix(m, "450"), strings.HasPrefix(m, "451"), strings.HasPrefix(m, "452"):
		return 2 * time.Minute, true
	case strings.Contains(m, "dial tcp"), strings.Contains(m, "connection refused"), strings.Contains(m, "i/o timeout"),
		strings.Contains(m, "connection reset"), strings.Contains(m, "eof"):
		return 2 * time.Minute, true
	}
	return 0, false
}

// mailPace waits for a free slot in the current second (shared by every replica). Without Redis it does not wait.
func (s *server) mailPace() {
	limit := mailMaxPerSecond()
	for i := 0; i < 50; i++ {
		sec := time.Now().Unix()
		n, err := s.rds.incrExpire(fmt.Sprintf("%s:mail:pace:%d", s.cfg.RedisPrefix, sec), 3)
		if err != nil || n <= limit {
			return
		}
		time.Sleep(time.Until(time.Unix(sec+1, 0)) + 5*time.Millisecond)
	}
}

// deliver: paced, retried once on a rate answer, queued if still refused for now.
func (s *server) deliver(from string, to, cc []string, msg []byte) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		s.mailPace()
		if err = s.smtpDeliver(from, to, cc, msg); err == nil {
			return nil
		}
		wait, transient := mailRetryAfter(err)
		if !transient || wait > time.Minute { // quota / unreachable: no point retrying in place
			break
		}
		time.Sleep(time.Duration(attempt+1) * 1500 * time.Millisecond)
	}
	wait, transient := mailRetryAfter(err)
	if !transient {
		return err
	}
	if qerr := s.enqueueMail(from, to, cc, msg, err.Error(), wait); qerr != nil {
		slog.Error("mail queue insert failed", "err", qerr)
		return err
	}
	return &mailQueuedError{reason: strings.TrimSpace(err.Error())}
}

func (s *server) enqueueMail(from string, to, cc []string, msg []byte, lastErr string, wait time.Duration) error {
	toJ, _ := json.Marshal(to)
	if cc == nil {
		cc = []string{}
	}
	ccJ, _ := json.Marshal(cc)
	subject := ""
	if i := strings.Index(string(msg), "Subject: "); i >= 0 {
		subject = strings.SplitN(string(msg[i+9:]), "\r\n", 2)[0]
	}
	_, err := s.db.Exec(context.Background(), `
		INSERT INTO public.mail_queue (from_addr, to_addrs, cc_addrs, subject, message, last_error, next_attempt_at)
		VALUES ($1,$2,$3,$4,$5,$6, now() + make_interval(secs => $7))`,
		from, string(toJ), string(ccJ), subject, msg, truncate(lastErr, 500), wait.Seconds())
	return err
}

// sweepMailQueue sends what is due, a batch at a time. Rows are claimed with SKIP LOCKED, so two replicas never
// send the same message. Returns how many went out.
func (s *server) sweepMailQueue() int {
	ctx := context.Background()
	sent := 0
	for batch := 0; batch < 20; batch++ {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return sent
		}
		rows, err := tx.Query(ctx, `
			SELECT id, from_addr, to_addrs, cc_addrs, message, attempts, created_at FROM public.mail_queue
			WHERE sent_at IS NULL AND given_up_at IS NULL AND next_attempt_at <= now()
			ORDER BY next_attempt_at LIMIT 25 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			_ = tx.Rollback(ctx)
			return sent
		}
		type item struct {
			id       int64
			from     string
			to, cc   []string
			msg      []byte
			attempts int
			created  time.Time
		}
		var items []item
		for rows.Next() {
			var it item
			var toJ, ccJ []byte
			if rows.Scan(&it.id, &it.from, &toJ, &ccJ, &it.msg, &it.attempts, &it.created) == nil {
				_ = json.Unmarshal(toJ, &it.to)
				_ = json.Unmarshal(ccJ, &it.cc)
				items = append(items, it)
			}
		}
		rows.Close()
		if len(items) == 0 {
			_ = tx.Rollback(ctx)
			return sent
		}
		quota := false
		for _, it := range items {
			if quota { // the provider's daily quota is spent: leave the rest for later
				_, _ = tx.Exec(ctx, `UPDATE public.mail_queue SET next_attempt_at = now() + interval '1 hour' WHERE id=$1`, it.id)
				continue
			}
			s.mailPace()
			err := s.smtpDeliver(it.from, it.to, it.cc, it.msg)
			switch wait, transient := mailRetryAfter(err); {
			case err == nil:
				_, _ = tx.Exec(ctx, `UPDATE public.mail_queue SET sent_at=now(), attempts=attempts+1, last_error='' WHERE id=$1`, it.id)
				sent++
			case transient && time.Since(it.created) < mailGiveUpAfter:
				quota = strings.Contains(strings.ToLower(err.Error()), "quota")
				backoff := wait * time.Duration(1<<min(it.attempts, 4))
				if backoff > 6*time.Hour {
					backoff = 6 * time.Hour
				}
				_, _ = tx.Exec(ctx, `UPDATE public.mail_queue SET attempts=attempts+1, last_error=$2,
					next_attempt_at = now() + make_interval(secs => $3) WHERE id=$1`, it.id, truncate(err.Error(), 500), backoff.Seconds())
			default:
				_, _ = tx.Exec(ctx, `UPDATE public.mail_queue SET attempts=attempts+1, last_error=$2, given_up_at=now() WHERE id=$1`,
					it.id, truncate(err.Error(), 500))
				slog.Warn("queued mail given up", "id", it.id, "err", err)
			}
		}
		if err := tx.Commit(ctx); err != nil || quota {
			return sent
		}
	}
	return sent
}
