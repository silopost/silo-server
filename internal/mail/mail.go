// Package mail is the domain layer of the mail cache: message ingest from
// IMAP FETCH data, identity matching, threading, system/folder/user labels,
// keyword mapping, thread aggregates and the events written for SSE.
//
// It talks to Postgres only and never to IMAP. The sync engine calls it with a
// pgx transaction, typically one transaction per batch of ~200 messages:
//
//	EnsureSystemLabels   once per account (idempotent)
//	IngestBatch          new/known UIDs of one folder (steps 1-5 and 8 of plan 3.5.7)
//	ApplyFlags           flag changes made by other clients (CONDSTORE/QRESYNC)
//	LocationVanished     UIDs that disappeared from a folder (plan 3.5.6)
//	PurgeOrphans         called by the orphan_gc job
//	IngestBody           phase C / on-demand body download
//	Reconcile            consistency check behind `silo-server reconcile`
//
// This package is the only writer of message_labels rows of type system and
// folder, of threads and of thread_labels (plan 4.4). Events are inserted into
// the events table in the same transaction as the change; the SSE hub that
// publishes them after commit is a separate component.
//
// Step 6 of 3.5.7 (waking a snoozed thread) and step 7 (filters) are not
// implemented yet; IngestBatch marks the place for them.
package mail

import (
	"github.com/silopost/silo-server/internal/store/sqlcgen"
)

// DB is what the package needs from a pgx connection, transaction or pool.
// Mutating functions must be given a transaction (several of them rely on
// pg_advisory_xact_lock and on the batch being atomic).
type DB = sqlcgen.DBTX

// Limits and tunables from plan 4.6.
const (
	maxThreadMessages = 100
	maxReferences     = 50
	maxRefLen         = 250
	maxBodyTextBytes  = 512 * 1024
	snippetRunes      = 200
)
