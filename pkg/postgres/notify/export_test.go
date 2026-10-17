package notify

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ListeningChannels returns pg_listening_channels() of l's current LISTEN
// connection.
func ListeningChannels(ctx context.Context, l *Listener) ([]string, error) {
	var channels []string
	err := onConn(ctx, l, func(ctx context.Context, conn *pgx.Conn) error {
		rows, err := conn.Query(ctx, "SELECT pg_listening_channels()")
		if err != nil {
			return err
		}
		channels, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return channels, err
}

// BackendPID returns the backend PID of l's current LISTEN connection.
func BackendPID(ctx context.Context, l *Listener) (uint32, error) {
	var pid uint32
	err := onConn(ctx, l, func(_ context.Context, conn *pgx.Conn) error {
		pid = conn.PgConn().PID()
		return nil
	})
	return pid, err
}

func onConn(ctx context.Context, l *Listener, fn func(context.Context, *pgx.Conn) error) error {
	result := make(chan error, 1)
	select {
	case l.onConn <- func(connCtx context.Context, conn *pgx.Conn) { result <- fn(connCtx, conn) }:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
