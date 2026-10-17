package projection

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/deepworx/go-utils/internal/projectionhook"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
)

// Version is one Projection version: its own tables, built by its own
// Consumer.
type Version struct {
	// N numbers the version, from 1. Its Consumer is named <name>@vN.
	N int
	// Handler writes the version's tables through the Cursor transaction in
	// ctx (postgres.Conn, or postgres.Stage in memory).
	Handler consumer.Handler
	// Clear empties the version's tables through the transaction in ctx.
	// Only Reset needs it.
	Clear func(ctx context.Context) error
}

// Config configures a Projection.
type Config struct {
	// Name names the Projection. It must not contain '@'.
	Name string
	// EventTypes is the filter of every version. Leave it empty to take each
	// version's filter from its Handler (a Router), so a new version can
	// handle more types than the active one.
	EventTypes []string
	// Active is the version readers see. Only it sends change
	// notifications and is the target of Await.
	Active Version
	// Shadow is a newer version building beside the active one, or nil.
	Shadow *Version
}

// Projection runs the Consumers of a Projection's active version and, during
// a version bump, its shadow version.
type Projection struct {
	active    *consumer.Consumer
	shadow    *consumer.Consumer
	consumers []*consumer.Consumer
	clears    []func(ctx context.Context) error
}

// New creates the Consumers of cfg's versions on rt. They always run
// Transactional; opts apply to each of them. New fails with
// consumer.ErrInvalidConfig for an empty or '@'-containing Name, a version
// below 1, a nil Handler or a Shadow that is not newer than Active, and with
// consumer.ErrDuplicate when rt already runs one of the versions.
func New(rt consumer.Runtime, cfg Config, opts ...consumer.Option) (*Projection, error) {
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("new projection %q: %w", cfg.Name, err)
	}
	changes, ok := projectionhook.ChangesOf(rt)
	if !ok {
		return nil, fmt.Errorf("new projection %q: unknown runtime %T: %w", cfg.Name, rt, consumer.ErrInvalidConfig)
	}

	in := versionInput{rt: rt, cfg: cfg, version: cfg.Active, changes: changes, opts: opts}
	active, err := newVersion(in)
	if err != nil {
		return nil, err
	}
	p := &Projection{
		active:    active,
		consumers: []*consumer.Consumer{active},
		clears:    []func(context.Context) error{cfg.Active.Clear},
	}
	if cfg.Shadow != nil {
		in.version, in.changes = *cfg.Shadow, nil
		if p.shadow, err = newVersion(in); err != nil {
			return nil, err
		}
		p.consumers = append(p.consumers, p.shadow)
		p.clears = append(p.clears, cfg.Shadow.Clear)
	}
	return p, nil
}

type versionInput struct {
	rt      consumer.Runtime
	cfg     Config
	version Version
	// changes sends the version's notifications; nil for the shadow.
	changes projectionhook.Changes
	opts    []consumer.Option
}

func newVersion(in versionInput) (*consumer.Consumer, error) {
	c, err := consumer.New(in.rt, consumer.Config{
		Name:       consumerName(in.cfg.Name, in.version.N),
		EventTypes: in.cfg.EventTypes,
		Mode:       consumer.Transactional,
		Handler:    newHandler(in.version.Handler, in.changes),
	}, in.opts...)
	if err != nil {
		return nil, fmt.Errorf("new projection %q: %w", in.cfg.Name, err)
	}
	return c, nil
}

func validate(cfg Config) error {
	switch {
	case cfg.Name == "":
		return fmt.Errorf("empty name: %w", consumer.ErrInvalidConfig)
	case strings.Contains(cfg.Name, "@"):
		return fmt.Errorf("name contains '@': %w", consumer.ErrInvalidConfig)
	case cfg.Active.N < 1:
		return fmt.Errorf("active version %d: %w", cfg.Active.N, consumer.ErrInvalidConfig)
	case cfg.Active.Handler == nil:
		return fmt.Errorf("nil active handler: %w", consumer.ErrInvalidConfig)
	case cfg.Shadow == nil:
		return nil
	case cfg.Shadow.N <= cfg.Active.N:
		return fmt.Errorf("shadow version %d not newer than active %d: %w",
			cfg.Shadow.N, cfg.Active.N, consumer.ErrInvalidConfig)
	case cfg.Shadow.Handler == nil:
		return fmt.Errorf("nil shadow handler: %w", consumer.ErrInvalidConfig)
	}
	return nil
}

// consumerName returns the name of the Consumer of version n, as
// consumer_pending and consumer_cursors know it.
func consumerName(name string, n int) string {
	return name + "@v" + strconv.Itoa(n)
}

// Consumers returns the Consumers to run, the active version's first and
// the shadow's second. Put them into a consumer.Group.
func (p *Projection) Consumers() []*consumer.Consumer {
	return append([]*consumer.Consumer(nil), p.consumers...)
}

// Await waits until the active version has caught up to pos, as
// consumer.Consumer.Await does. The shadow is never awaited.
func (p *Projection) Await(ctx context.Context, pos eventstore.Cursor) consumer.Freshness {
	return p.active.Await(ctx, pos)
}

// CaughtUp reports whether the shadow version, or the active one when there
// is no shadow, has processed every visible event it matches and is not
// Stopped. Use it to decide when the next release may switch to the shadow.
func (p *Projection) CaughtUp(ctx context.Context) (bool, error) {
	target := p.active
	if p.shadow != nil {
		target = p.shadow
	}
	s, err := target.Status(ctx)
	if err != nil {
		return false, fmt.Errorf("caught up: %w", err)
	}
	return s.Stopped == nil && s.Pending == 0, nil
}

// Reset is for prototypes and development, where readers may see an empty
// read model. It runs the Clear of every version and rewinds their Cursors
// to the start of the log in one transaction, and clears their Stopped
// state; the versions then rebuild from the first event. It fails with
// consumer.ErrRunning while a version runs or drains, here or in another
// process, and with consumer.ErrInvalidConfig when a version has no Clear.
// In production, repair a read model with a version bump instead.
func (p *Projection) Reset(ctx context.Context) error {
	consumers := make([]any, len(p.consumers))
	for i, c := range p.consumers {
		if p.clears[i] == nil {
			return fmt.Errorf("reset %q: no Clear: %w", c.Name(), consumer.ErrInvalidConfig)
		}
		consumers[i] = c
	}
	err := projectionhook.Reset(ctx, projectionhook.ResetInput{
		Consumers: consumers,
		Clear: func(ctx context.Context) error {
			for _, clear := range p.clears {
				if err := clear(ctx); err != nil {
					return err
				}
			}
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("projection: %w", err)
	}
	return nil
}
