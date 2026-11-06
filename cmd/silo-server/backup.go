package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/backup"
	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/config"
	"github.com/silopost/silo-server/internal/secrets"
	"github.com/silopost/silo-server/internal/store"
)

func init() {
	commands["export-metadata"] = exportMetadataCmd
	commands["import-metadata"] = importMetadataCmd
}

// backupEnv is what the backup commands need: database, secret keys and the
// blob store of the instance (SILO_BLOB_BACKEND).
type backupEnv struct {
	pool  *pgxpool.Pool
	keys  *secrets.Keyring
	blobs blob.BlobStore
}

func openBackupEnv(ctx context.Context, needKeys bool) (*backupEnv, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return nil, errors.New("SILO_DATABASE_URL is required")
	}
	e := &backupEnv{}
	if needKeys {
		if cfg.SecretKey == "" {
			return nil, errors.New("SILO_SECRET_KEY is required (account secrets stay encrypted with it)")
		}
		current, previous, err := cfg.SecretKeys()
		if err != nil {
			return nil, err
		}
		if e.keys, err = secrets.NewKeyring(current, previous); err != nil {
			return nil, err
		}
		if e.blobs, err = blob.Open(ctx, cfg); err != nil {
			return nil, err
		}
	}
	if e.pool, err = store.Connect(ctx, cfg.DatabaseURL); err != nil {
		return nil, err
	}
	return e, nil
}

func printWarnings(st *backup.Stats) {
	for _, w := range st.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
}

// exportMetadataCmd implements `silo-server export-metadata [--out FILE|-]
// [--user EMAIL] [--no-local-blobs]`: the metadata backup (package backup).
// A file is written next to its final name and renamed when complete.
func exportMetadataCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export-metadata", flag.ContinueOnError)
	out := fs.String("out", "-", "output file, - for stdout")
	user := fs.String("user", "", "export only this user (email)")
	noBlobs := fs.Bool("no-local-blobs", false, "leave the bytes of draft attachments and queued messages out (sha256 only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := openBackupEnv(ctx, true)
	if err != nil {
		return err
	}
	defer e.pool.Close()

	var w io.Writer = os.Stdout
	var tmp *os.File
	if *out != "-" {
		if tmp, err = os.CreateTemp(filepath.Dir(*out), ".silo-export-*"); err != nil {
			return err
		}
		defer func() {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}()
		w = tmp
	}
	st, err := backup.Export(ctx, e.pool, w, backup.ExportOptions{Keys: e.keys, Blobs: e.blobs, User: *user, NoLocalBlobs: *noBlobs})
	if err != nil {
		return err
	}
	if tmp != nil {
		if err := tmp.Sync(); err != nil {
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), *out); err != nil {
			return err
		}
	}
	printWarnings(st)
	fmt.Fprintf(os.Stderr, "exported %d users, %d accounts, %d labels, %d label assignments, %d snoozes, %d filters, %d contacts, %d drafts, %d queued, %d blobs (%d bytes)\n",
		st.Users, st.Accounts, st.Labels, st.LabelAssignments, st.Snoozes, st.Filters, st.Contacts, st.Drafts, st.Outbound, st.Blobs, st.BlobBytes)
	return nil
}

// importMetadataCmd implements `silo-server import-metadata --in FILE|-
// [--merge]`. Run it on a migrated database (`silo-server migrate up`) with
// the SILO_SECRET_KEY of the export; the server picks up the queued jobs
// (initial sync, metadata_restore, send) when it runs.
func importMetadataCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import-metadata", flag.ContinueOnError)
	in := fs.String("in", "", "input file, - for stdin")
	merge := fs.Bool("merge", false, "import into existing users (accounts updated by key, labels upserted, nothing duplicated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" {
		return errors.New("--in is required (a file or - for stdin)")
	}
	e, err := openBackupEnv(ctx, true)
	if err != nil {
		return err
	}
	defer e.pool.Close()
	var r io.Reader = os.Stdin
	if *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	st, err := backup.Import(ctx, e.pool, r, backup.ImportOptions{Keys: e.keys, Blobs: e.blobs, Merge: *merge})
	if err != nil {
		return err
	}
	printWarnings(st)
	fmt.Printf("imported %d users, %d accounts, %d labels, %d label assignments and %d snoozes (staged until the sync finds the messages), %d filters, %d contacts, %d drafts, %d queued, %d blobs\n",
		st.Users, st.Accounts, st.Labels, st.LabelAssignments, st.Snoozes, st.Filters, st.Contacts, st.Drafts, st.Outbound, st.Blobs)
	return nil
}
