// Command server is the Postgres main of the quickstart service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/deepworx/go-utils/internal/quickstart/app"
	"github.com/deepworx/go-utils/pkg/eventsourcing/consumer"
	"github.com/deepworx/go-utils/pkg/eventsourcing/eventstore"
	"github.com/deepworx/go-utils/pkg/inbox"
	"github.com/deepworx/go-utils/pkg/otel"
	"github.com/deepworx/go-utils/pkg/postgres"
	"github.com/deepworx/go-utils/pkg/postgres/lease"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
	"github.com/deepworx/go-utils/pkg/scheduler"
	"github.com/deepworx/go-utils/pkg/shutdown"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdown.DefaultShutdownTimeout)
		defer cancel()
		_ = shutdown.Shutdown(ctx)
	}()

	if err := otel.Setup(ctx, otel.Config{ServiceName: "todo", ServiceVersion: "dev"}); err != nil {
		return fmt.Errorf("otel: %w", err)
	}
	pgCfg := postgres.DefaultConfig()
	pgCfg.DSN = os.Getenv("DATABASE_URL")
	pool, err := postgres.NewPool(ctx, pgCfg)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", ":8080")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	return serve(ctx, serveInput{
		Pool:     pool,
		Schema:   os.Getenv("EVENTSOURCING_SCHEMA"),
		Listener: ln,
		Mailer:   logMailer{},
		Tenant:   "default",
	})
}

type serveInput struct {
	Pool *pgxpool.Pool
	// Schema holds the library tables; empty means public.
	Schema   string
	Listener net.Listener
	Mailer   app.Mailer
	// Tenant is the fixed Tenant of this single-tenant service.
	Tenant string
}

// serve wires the service on Postgres and runs it until ctx ends.
func serve(ctx context.Context, in serveInput) error {
	codec := app.Codec()
	store, err := eventstore.NewPgStore(eventstore.PgStoreConfig{Pool: in.Pool, Codec: codec, Schema: in.Schema})
	if err != nil {
		return fmt.Errorf("event store: %w", err)
	}
	uow, err := eventstore.NewPgUnitOfWork(eventstore.PgUnitOfWorkConfig{
		UnitOfWork: postgres.NewUnitOfWork(in.Pool),
		Schema:     in.Schema,
	})
	if err != nil {
		return fmt.Errorf("unit of work: %w", err)
	}
	listener := notify.NewListener(in.Pool, notify.Config{})
	claims := lease.NewClaims(in.Pool, lease.Config{})
	rt, err := consumer.NewPgRuntime(consumer.PgRuntimeConfig{
		Pool:     in.Pool,
		Store:    store,
		Listener: listener,
		Claims:   claims,
	})
	if err != nil {
		return fmt.Errorf("consumer runtime: %w", err)
	}
	a, err := app.New(app.Config{
		Codec:      codec,
		Store:      store,
		UnitOfWork: uow,
		Runtime:    rt,
		ReadModel:  app.NewPgReadModel(in.Pool),
		Mailer:     in.Mailer,
	})
	if err != nil {
		return err
	}

	inboxStore, err := inbox.NewStore(in.Pool, inbox.StoreConfig{Schema: in.Schema})
	if err != nil {
		return fmt.Errorf("inbox: %w", err)
	}
	tracker, err := inbox.NewWorker(inboxStore, inbox.Config{
		Source: app.TrackerSource,
		Handle: a.HandleTrackerWebhook,
	}, inbox.WithListener(listener), inbox.WithClaims(claims))
	if err != nil {
		return fmt.Errorf("tracker worker: %w", err)
	}

	sched, err := scheduler.New(in.Pool, claims, scheduler.Config{Schema: in.Schema, Location: time.UTC})
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	if err := sched.Register(inbox.CleanupJob(inboxStore, 0)); err != nil {
		return fmt.Errorf("register inbox cleanup: %w", err)
	}

	mux := http.NewServeMux()
	a.Routes(mux)
	mux.Handle("POST /webhooks/tracker", app.ReceiveTrackerWebhook(inboxStore))
	srv := &http.Server{
		Handler:           app.Metadata(in.Tenant, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return listener.Run(ctx) })
	g.Go(func() error { return claims.Run(ctx) })
	g.Go(func() error { return a.Consumers().Run(ctx) })
	g.Go(func() error { return tracker.Run(ctx) })
	g.Go(func() error { return sched.Run(ctx) })
	g.Go(func() error {
		if err := srv.Serve(in.Listener); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	return g.Wait()
}

// logMailer stands in for a real mail provider.
type logMailer struct{}

func (logMailer) Send(ctx context.Context, m app.Mail) error {
	slog.InfoContext(ctx, "mail sent", slog.String("to", m.To), slog.String("subject", m.Subject))
	return nil
}
