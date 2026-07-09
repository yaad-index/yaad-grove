// Package transport defines the platform-neutral adapter boundary. A transport
// is a pluggable, cleanly extractable module: the core carries zero transport
// dependencies, and "plug anywhere" means adding another adapter, not touching
// core (ADR 0001).
//
// Features are defined here once in platform-neutral terms through a capability
// interface, never written against any one platform's API. Where a platform
// lacks a capability (say reactions), the feature degrades gracefully — falls
// back to commands — rather than becoming platform-only.
package transport

import (
	"context"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// Inbound is a message a transport received, normalized toward core.Query. The
// transport fills identity and surface; the runtime turns it into a core.Query
// after the access/consent gates pass.
type Inbound struct {
	User    core.User
	Surface core.Surface
	Text    string
	// ReplyTo is an opaque, transport-owned handle for where to send the reply
	// (chat id, thread, etc.). The core never interprets it.
	ReplyTo string
}

// Handler processes one inbound message and is supplied by the runtime. A
// transport calls it for each message it receives.
type Handler func(ctx context.Context, in Inbound) (core.Reply, error)

// Capability names an optional, platform-varying feature. The core asks a
// transport what it supports and degrades gracefully when it does not.
type Capability int

const (
	// CapReactions: the transport can attach emoji reactions to a message.
	CapReactions Capability = iota
	// CapEditMessage: the transport can edit a previously sent message.
	CapEditMessage
)

// Transport is one platform adapter. Implementations live in sub-packages
// (transport/telegram, later transport/discord, ...). The interface stays
// platform-neutral so no feature may assume the transport.
type Transport interface {
	// Name identifies the adapter for logs and config.
	Name() string
	// Supports reports whether an optional capability is available here.
	Supports(c Capability) bool
	// Run receives messages and dispatches each to handler until ctx is done.
	Run(ctx context.Context, handler Handler) error
	// Send delivers a reply to the place identified by replyTo.
	Send(ctx context.Context, replyTo string, reply core.Reply) error
}
