// Author: Patrick Munis at newwave
package main

// audit_group.go — group commit for the audit hash chain (logAudit, casemgmt.go).
//
// The chain is per tenant and strictly ordered: each entry's hash = sha256(prev_hash + payload), and prev_hash is
// the previous entry's hash. That needs one writer at a time per tenant (the advisory lock), and each write used
// to be its own transaction, so every entry paid a full commit (a disk flush) while holding the lock. A TPA filing
// writes six entries; a load test on dev measured ~50 ms per locked write, which capped filing at ~3.4 a second
// for the whole tenant however many servers ran.
//
// Now each logAudit hands its entry to one writer per server process and waits for it to be committed (callers
// see no change). The writer takes every entry waiting at that moment, and per tenant writes them in one
// transaction under the same lock: read the last hash, chain the entries in arrival order, insert, commit once.
// Across replicas the advisory lock still serializes the groups, so the chain stays a single line. If a group
// write fails, its entries are written one at a time (the old way), so one bad entry can't lose the others.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"
)

type auditEntry struct {
	tenant, caseID, action, body, org, actor string
	done                                     chan struct{}
}

const auditGroupMax = 500 // entries per group; bounds one transaction

var auditWriters sync.Map // *server -> chan *auditEntry

func (s *server) auditQueue() chan *auditEntry {
	if q, ok := auditWriters.Load(s); ok {
		return q.(chan *auditEntry)
	}
	q := make(chan *auditEntry, 4096)
	actual, loaded := auditWriters.LoadOrStore(s, q)
	if !loaded {
		go s.runAuditWriter(q)
	}
	return actual.(chan *auditEntry)
}

// appendAudit queues one entry and waits until it is committed (or written one-by-one after a failed group).
func (s *server) appendAudit(e *auditEntry) {
	e.done = make(chan struct{})
	s.auditQueue() <- e
	select {
	case <-e.done:
	case <-time.After(2 * time.Minute):
		slog.Error("audit entry not confirmed in time", "tenant", e.tenant, "action", e.action)
	}
}

func (s *server) runAuditWriter(q chan *auditEntry) {
	for first := range q {
		group := []*auditEntry{first}
	drain:
		for len(group) < auditGroupMax {
			select {
			case e := <-q:
				group = append(group, e)
			default:
				break drain
			}
		}
		byTenant, order := map[string][]*auditEntry{}, []string{}
		for _, e := range group {
			if _, seen := byTenant[e.tenant]; !seen {
				order = append(order, e.tenant)
			}
			byTenant[e.tenant] = append(byTenant[e.tenant], e)
		}
		for _, tenant := range order {
			s.writeAuditGroup(tenant, byTenant[tenant])
		}
		for _, e := range group {
			close(e.done)
		}
	}
}

func (s *server) writeAuditGroup(tenant string, entries []*auditEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.insertAuditChain(ctx, tenant, entries); err == nil {
		return
	} else if len(entries) == 1 {
		slog.Error("audit", "action", entries[0].action, "err", err)
		return
	} else {
		slog.Warn("audit group write failed; writing its entries one at a time", "tenant", tenant, "entries", len(entries), "err", err)
	}
	for _, e := range entries {
		if err := s.insertAuditChain(ctx, tenant, []*auditEntry{e}); err != nil {
			slog.Error("audit", "action", e.action, "err", err)
		}
	}
}

// insertAuditChain appends entries, in order, to the tenant's chain in one transaction under the chain lock.
func (s *server) insertAuditChain(ctx context.Context, tenant string, entries []*auditEntry) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('audit_log:' || $1))`, tenant); err != nil {
		return err
	}
	prev := "GENESIS"
	_ = tx.QueryRow(ctx, `SELECT hash FROM public.audit_log WHERE tenant=$1 ORDER BY id DESC LIMIT 1`, tenant).Scan(&prev)
	bodies := make([]string, len(entries))
	for i, e := range entries {
		bodies[i] = e.body
	}
	for i, h := range chainHashes(prev, bodies) {
		e := entries[i]
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.audit_log (tenant, case_id, action, payload, prev_hash, hash, org, actor)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''))`, tenant, e.caseID, e.action, e.body, prev, h, e.org, e.actor); err != nil {
			return err
		}
		prev = h
	}
	return tx.Commit(ctx)
}

// chainHashes: the hashes of bodies appended in order after prev (hash = sha256(prev_hash + payload)).
func chainHashes(prev string, bodies []string) []string {
	out := make([]string, len(bodies))
	for i, b := range bodies {
		sum := sha256.Sum256([]byte(prev + b))
		out[i] = hex.EncodeToString(sum[:])
		prev = out[i]
	}
	return out
}
