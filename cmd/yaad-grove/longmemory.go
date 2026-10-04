package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/honcho"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/trust"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/namespaces"
	"github.com/yaad-index/yaad-grove/internal/runtime"
)

// minLongMemoryWindow is the shortest session window: the purge runs a minute
// after each boundary, so a window must be much longer than that.
const minLongMemoryWindow = time.Hour

// longMemoryTokenEnv names the environment variable holding the memory
// service's token, when it requires one.
const longMemoryTokenEnv = "YAADGROVE_LONG_MEMORY_TOKEN"

// defaultDeriverInstructions steer the memory service's deriver to the speaker
// (ADR 0023 §3). They steer it and enforce nothing.
const defaultDeriverInstructions = "Form conclusions only about the person speaking: what they said, asked, prefer and think. " +
	"Do not keep anything they say about another person, and form no conclusions about anyone else."

// processSecrets are the environment secrets the process uses. They are
// resolved through the shared resolver before memory is used, so its scrubber
// removes every one of them from what memory keeps, from the first turn on.
var processSecrets = []string{modelKeyEnv, "YAADGROVE_EMBEDDING_API_KEY", "YAADGROVE_TELEGRAM_TOKEN", longMemoryTokenEnv}

// longMemory is long-term memory as serve runs it: the engine's memory, and a
// purger for every namespace in the record, configured or dropped, and the
// eraser that withdrawal erases through in all of them.
type longMemory struct {
	memory  *core.Memory
	purgers []runtime.Purger
	eraser  *runtime.Eraser
}

// withdrawal gives policy the eraser withdrawal erases through, and says
// whether the service derives, so the disclosure tells the user both. A nil
// long-term memory leaves policy without either.
func (lm *longMemory) withdrawal(policy *runtime.Policy, derive bool) {
	if lm == nil {
		return
	}
	policy.Erase, policy.MemoryDerive = lm.eraser, derive
}

// buildLongMemory builds long-term memory from c (ADR 0023): bonyan's memory
// stores over the memory service, one per namespace, the instance's and any a
// group chat was given, each scrubbing with secrets' scrubber. Every configured
// namespace is in the record of namespaces before anything is kept, and every
// recorded one is purged. It is nil when no service URL is set, and an error
// when memory is set up only in part.
func buildLongMemory(c *ServeCmd, secrets *secret.Resolver, now time.Time) (*longMemory, error) {
	if c.LongMemoryURL == "" {
		if c.LongMemoryNamespace != "" || len(c.LongMemoryGroupNamespaces) > 0 || c.LongMemoryRetention != 0 || c.LongMemoryDerive || c.LongMemoryInstructions != "" {
			return nil, errors.New("long-term memory options are set without --long-memory-url: set the URL to turn it on, or remove them")
		}
		return nil, nil
	}
	switch {
	case c.LongMemoryNamespace == "":
		return nil, errors.New("--long-memory-namespace is required with --long-memory-url: set this instance's own namespace; there is no default")
	case c.LongMemoryWindow < minLongMemoryWindow:
		return nil, fmt.Errorf("--long-memory-window must be at least %s", minLongMemoryWindow)
	case c.LongMemoryRetention <= 0:
		return nil, errors.New("--long-memory-retention is required with --long-memory-url and must be positive")
	case c.LongMemoryRetention%c.LongMemoryWindow != 0:
		return nil, fmt.Errorf("--long-memory-retention (%s) must be a whole number of --long-memory-window (%s), so a purge deletes whole sessions", c.LongMemoryRetention, c.LongMemoryWindow)
	case c.LongMemoryRecord == "":
		return nil, errors.New("--long-memory-record is required with --long-memory-url: a file on persistent storage, the same at every start, recording every namespace memory was kept in")
	}
	groups, err := parseGroupNamespaces(c.LongMemoryGroupNamespaces)
	if err != nil {
		return nil, err
	}

	values := map[string]string{}
	scoped := secrets.Scope(processSecrets...)
	for _, name := range processSecrets {
		v, err := scoped.Resolve(context.Background(), name)
		switch {
		case err == nil:
			values[name] = v.Reveal()
		case !errors.Is(err, secret.ErrNotFound):
			return nil, fmt.Errorf("long-term memory: resolve %s: %w", name, err)
		}
	}

	instructions := c.LongMemoryInstructions
	if instructions == "" {
		instructions = defaultDeriverInstructions
	}
	open := func(namespace string) (memory.Backend, error) {
		return honcho.Open(honcho.Options{
			URL:          c.LongMemoryURL,
			Namespace:    namespace,
			Derive:       c.LongMemoryDerive,
			Instructions: instructions,
			Token:        values[longMemoryTokenEnv],
		})
	}
	record, err := namespaces.Open(c.LongMemoryRecord)
	if err != nil {
		return nil, err
	}
	return newLongMemory(&storeSet{open: open, retention: c.LongMemoryRetention, scrubber: secrets.Scrubber()}, record, c.LongMemoryNamespace, groups, c.LongMemoryWindow, now)
}

// storeSet opens one store per namespace, each namespace's backend once.
type storeSet struct {
	open      func(namespace string) (memory.Backend, error)
	retention time.Duration
	scrubber  *secret.Scrubber
	stores    map[string]*memory.Store
	backends  map[string]memory.Backend
}

// get is ns's store. The trust policy is bonyan's default inside the
// registry's guard, as every policy it assembles is: it fails closed, and with
// no recorder configured its decisions are recorded nowhere.
func (s *storeSet) get(ns string) (*memory.Store, error) {
	if st, ok := s.stores[ns]; ok {
		return st, nil
	}
	backend, err := s.open(ns)
	if err != nil {
		return nil, err
	}
	st, err := memory.NewStore(backend, memory.Options{
		Namespace:  ns,
		Policy:     registry.GuardPolicy(nil, nil),
		PolicyName: trust.DefaultName,
		Retention:  s.retention,
		Scrubber:   s.scrubber,
	})
	if err != nil {
		return nil, fmt.Errorf("long-term memory namespace %q: %w", ns, err)
	}
	if s.stores == nil {
		s.stores, s.backends = map[string]*memory.Store{}, map[string]memory.Backend{}
	}
	s.stores[ns], s.backends[ns] = st, backend
	return st, nil
}

// newLongMemory builds the engine's memory over namespace and the namespaces
// groups gives group chats, records them as configured, records every other
// recorded namespace as dropped at now unless it already was, and makes a
// purger for every recorded namespace.
func newLongMemory(set *storeSet, record *namespaces.Record, namespace string, groups map[string]string, window time.Duration, now time.Time) (*longMemory, error) {
	m := &core.Memory{Window: window, Scrubber: set.scrubber}
	var err error
	if m.Store, err = set.get(namespace); err != nil {
		return nil, err
	}
	configured := map[string]bool{namespace: true}
	for chat, ns := range groups {
		s, err := set.get(ns)
		if err != nil {
			return nil, err
		}
		if m.Groups == nil {
			m.Groups = map[string]*memory.Store{}
		}
		m.Groups[chat] = s
		configured[ns] = true
	}
	names := make([]string, 0, len(configured))
	for ns := range configured {
		names = append(names, ns)
	}
	// On disk before the engine can keep anything in them.
	if err := record.Configured(names...); err != nil {
		return nil, err
	}
	lm := &longMemory{memory: m, eraser: &runtime.Eraser{Memory: m, Record: record, Stores: map[string]runtime.SubjectDeleter{}}}
	for _, ns := range record.Namespaces() {
		st, err := set.get(ns)
		if err != nil {
			return nil, err
		}
		lm.eraser.Stores[ns] = st
		var dropped time.Time
		if !configured[ns] {
			if dropped, err = record.Dropped(ns, now); err != nil {
				return nil, err
			}
		}
		lm.purgers = append(lm.purgers, runtime.NamespacePurge{
			Namespace: ns,
			Backend:   set.backends[ns],
			Record:    record,
			Retention: set.retention,
			Window:    window,
			Dropped:   dropped,
		})
	}
	return lm, nil
}

// parseGroupNamespaces parses --long-memory-group-namespace specs, each
// "chatid=namespace", into a map from chat ID to namespace. A spec missing
// either side, or a chat listed twice, is an error.
func parseGroupNamespaces(specs []string) (map[string]string, error) {
	groups := map[string]string{}
	for _, spec := range specs {
		chat, ns, ok := strings.Cut(spec, "=")
		chat, ns = strings.TrimSpace(chat), strings.TrimSpace(ns)
		if !ok || chat == "" || ns == "" {
			return nil, fmt.Errorf("--long-memory-group-namespace %q: want chatid=namespace", spec)
		}
		if _, dup := groups[chat]; dup {
			return nil, fmt.Errorf("--long-memory-group-namespace: chat %q is given a namespace twice", chat)
		}
		groups[chat] = ns
	}
	return groups, nil
}
