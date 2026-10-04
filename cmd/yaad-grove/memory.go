package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/yaad-index/bonyan/secret"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/runtime"
)

// MemoryCmd manages long-term memory outside the running bot.
type MemoryCmd struct {
	Erase MemoryEraseCmd `cmd:"" help:"Erase users' long-term memory in every recorded namespace, as consent withdrawal does. Run it with the bot stopped and serve's configuration; like serve at start, it updates --long-memory-record."`
}

// MemoryEraseCmd erases users' long-term memory the way withdrawal does (ADR
// 0023 §5), for users who withdrew while the bot could not erase. It takes
// serve's flags and reads serve's section of the configuration file. It runs
// with the bot stopped: --unconsented reads the access-control store, which the
// running bot holds open. Like serve at start, it writes the record of
// namespaces: the configured ones, and any no longer configured as dropped.
type MemoryEraseCmd struct {
	ServeCmd `embed:""`

	Users       []string `name:"user" help:"A user ID to erase (repeatable)."`
	Unconsented bool     `name:"unconsented" help:"Erase every user in the access-control store whose consent is not granted: everyone who withdrew, and everyone seen but never opted in, who has nothing to erase."`
}

// Run erases the chosen users and prints how each namespace went.
func (m *MemoryEraseCmd) Run(log *slog.Logger) error {
	c := &m.ServeCmd
	if (len(m.Users) > 0) == m.Unconsented {
		return errors.New("memory erase: give --user or --unconsented, one of them")
	}
	lm, err := buildLongMemory(c, secret.NewResolver(secret.Env{}), time.Now())
	if err != nil {
		return err
	}
	if lm == nil {
		return errors.New("memory erase: long-term memory is not configured (--long-memory-url)")
	}
	users := m.Users
	if m.Unconsented {
		store, err := acl.OpenBolt(c.ACLDB)
		if err != nil {
			return fmt.Errorf("memory erase: %w (stop the bot first: it holds the access-control store open)", err)
		}
		users, err = store.Unconsented(context.Background())
		_ = store.Close()
		if err != nil {
			return err
		}
	}
	log.Info("memory erase", "users", len(users))
	return eraseUsers(context.Background(), lm.eraser, users, os.Stdout)
}

// eraseUsers erases each user, writes one line per user and namespace, and
// fails if any erase failed. Erasing is idempotent: a user with nothing in a
// namespace is erased there at once.
func eraseUsers(ctx context.Context, e runtime.MemoryEraser, users []string, w io.Writer) error {
	var b strings.Builder
	failed := 0
	for _, u := range users {
		results := e.Erase(ctx, u)
		for _, r := range results {
			if r.Err != nil {
				failed++
				fmt.Fprintf(&b, "%s\t%s\tfailed: %v\n", u, r.Namespace, r.Err)
				continue
			}
			fmt.Fprintf(&b, "%s\t%s\terased\n", u, r.Namespace)
		}
	}
	fmt.Fprintf(&b, "%d users, %d failed erases\n", len(users), failed)
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("memory erase: %d erases failed; run it again to retry", failed)
	}
	return nil
}
