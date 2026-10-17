package eventstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/deepworx/go-utils/pkg/ctxutil"
)

// UnitOfWork runs Command transactions. A Command transaction changes
// exactly one Stream, identified by the Tenant in ctx and the stream ID;
// writers to the same Stream are serialized from before the load until
// commit.
type UnitOfWork interface {
	// Execute opens a Command transaction for stream and runs fn in it. It
	// commits when fn returns nil and rolls back otherwise. It fails with
	// ErrNestedUnitOfWork inside an open Command transaction and with
	// ErrNoTenant without a Tenant.
	Execute(ctx context.Context, stream uuid.UUID, fn func(ctx context.Context) error) error
	// Within joins the open Command transaction when it holds stream, or
	// opens one like Execute. It fails with ErrStreamMismatch when the open
	// Command transaction holds another Stream.
	Within(ctx context.Context, stream uuid.UUID, fn func(ctx context.Context) error) error
}

// commandTx marks a context as running inside a Command transaction.
type commandTx struct {
	tenant string
	stream uuid.UUID
	// correlation is the Correlation used when ctx carries none.
	correlation string
}

type commandTxKey struct{}

func commandTxFrom(ctx context.Context) (*commandTx, bool) {
	tx, ok := ctx.Value(commandTxKey{}).(*commandTx)
	return tx, ok
}

// openCommandTx returns ctx marked with a new Command transaction for
// stream.
func openCommandTx(ctx context.Context, stream uuid.UUID) (context.Context, *commandTx, error) {
	if open, ok := commandTxFrom(ctx); ok {
		return nil, nil, fmt.Errorf("open %s inside %s: %w", stream, open.stream, ErrNestedUnitOfWork)
	}
	tenant, err := tenantFrom(ctx)
	if err != nil {
		return nil, nil, err
	}
	correlation, err := uuid.NewV7()
	if err != nil {
		return nil, nil, fmt.Errorf("generate correlation: %w", err)
	}
	tx := &commandTx{tenant: tenant, stream: stream, correlation: correlation.String()}
	return context.WithValue(ctx, commandTxKey{}, tx), tx, nil
}

// executor is the Execute method of a UnitOfWork.
type executor func(ctx context.Context, stream uuid.UUID, fn func(ctx context.Context) error) error

// within joins the open Command transaction for stream, or opens one with
// execute.
func (execute executor) within(ctx context.Context, stream uuid.UUID, fn func(ctx context.Context) error) error {
	if _, ok := commandTxFrom(ctx); !ok {
		return execute(ctx, stream, fn)
	}
	if _, err := commandTxFor(ctx, stream); err != nil {
		return err
	}
	return fn(ctx)
}

// commandTxFor returns the open Command transaction if it holds stream for
// the Tenant in ctx.
func commandTxFor(ctx context.Context, stream uuid.UUID) (*commandTx, error) {
	tx, ok := commandTxFrom(ctx)
	if !ok {
		return nil, ErrNotCommandTransaction
	}
	tenant, _ := ctxutil.TenantID(ctx)
	if tx.stream != stream || tenant != tx.tenant {
		return nil, fmt.Errorf("transaction holds %s/%s, got %s/%s: %w", tx.tenant, tx.stream, tenant, stream, ErrStreamMismatch)
	}
	return tx, nil
}

func tenantFrom(ctx context.Context) (string, error) {
	tenant, ok := ctxutil.TenantID(ctx)
	if !ok {
		return "", ErrNoTenant
	}
	return tenant, nil
}
