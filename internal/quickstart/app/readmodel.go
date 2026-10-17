package app

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepworx/go-utils/pkg/postgres"
)

// InMemoryReadModel keeps the task_list read model in memory. Its writes go
// through postgres.Stage, so they commit or roll back with the Cursor
// transaction of an InMemoryRuntime.
type InMemoryReadModel struct {
	mu    sync.Mutex
	tasks map[string][]Task
}

// NewInMemoryReadModel returns an empty InMemoryReadModel.
func NewInMemoryReadModel() *InMemoryReadModel {
	return &InMemoryReadModel{tasks: map[string][]Task{}}
}

// Insert adds task.
func (m *InMemoryReadModel) Insert(ctx context.Context, tenant string, task Task) error {
	postgres.Stage(ctx, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.tasks[tenant] = append(m.tasks[tenant], task)
	})
	return nil
}

// SetDone marks the task id as done.
func (m *InMemoryReadModel) SetDone(ctx context.Context, tenant string, id uuid.UUID) error {
	m.update(ctx, tenant, id, func(t *Task) { t.Done = true })
	return nil
}

// SetNotified marks the task id as notified.
func (m *InMemoryReadModel) SetNotified(ctx context.Context, tenant string, id uuid.UUID) error {
	m.update(ctx, tenant, id, func(t *Task) { t.Notified = true })
	return nil
}

func (m *InMemoryReadModel) update(ctx context.Context, tenant string, id uuid.UUID, change func(*Task)) {
	postgres.Stage(ctx, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := range m.tasks[tenant] {
			if m.tasks[tenant][i].ID == id {
				change(&m.tasks[tenant][i])
			}
		}
	})
}

// List returns the tenant's tasks in the order they were opened.
func (m *InMemoryReadModel) List(_ context.Context, tenant string) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Task(nil), m.tasks[tenant]...), nil
}

// Clear removes every task.
func (m *InMemoryReadModel) Clear(ctx context.Context) error {
	postgres.Stage(ctx, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		clear(m.tasks)
	})
	return nil
}

// PgReadModel keeps the task_list read model in the task_list_v1 table and
// reads it through the task_list view (migrations/000002_task_list.up.sql).
// Its writes join the Cursor transaction through postgres.Conn.
type PgReadModel struct {
	pool *pgxpool.Pool
}

// NewPgReadModel returns a PgReadModel on pool.
func NewPgReadModel(pool *pgxpool.Pool) *PgReadModel {
	return &PgReadModel{pool: pool}
}

// Insert adds task.
func (m *PgReadModel) Insert(ctx context.Context, tenant string, task Task) error {
	_, err := postgres.Conn(ctx, m.pool).Exec(ctx,
		`INSERT INTO task_list_v1 (tenant_id, id, title, assignee, opened_at) VALUES ($1, $2, $3, $4, $5)`,
		tenant, task.ID, task.Title, task.Assignee, task.OpenedAt)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

// SetDone marks the task id as done.
func (m *PgReadModel) SetDone(ctx context.Context, tenant string, id uuid.UUID) error {
	_, err := postgres.Conn(ctx, m.pool).Exec(ctx,
		`UPDATE task_list_v1 SET done = true WHERE tenant_id = $1 AND id = $2`, tenant, id)
	if err != nil {
		return fmt.Errorf("set task done: %w", err)
	}
	return nil
}

// SetNotified marks the task id as notified.
func (m *PgReadModel) SetNotified(ctx context.Context, tenant string, id uuid.UUID) error {
	_, err := postgres.Conn(ctx, m.pool).Exec(ctx,
		`UPDATE task_list_v1 SET notified = true WHERE tenant_id = $1 AND id = $2`, tenant, id)
	if err != nil {
		return fmt.Errorf("set task notified: %w", err)
	}
	return nil
}

// List returns the tenant's tasks in the order they were opened.
func (m *PgReadModel) List(ctx context.Context, tenant string) ([]Task, error) {
	rows, err := postgres.Conn(ctx, m.pool).Query(ctx,
		`SELECT id, title, assignee, done, notified, opened_at FROM task_list
		 WHERE tenant_id = $1 ORDER BY opened_at, id`, tenant)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Task, error) {
		var t Task
		err := row.Scan(&t.ID, &t.Title, &t.Assignee, &t.Done, &t.Notified, &t.OpenedAt)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan tasks: %w", err)
	}
	return tasks, nil
}

// Clear removes every task.
func (m *PgReadModel) Clear(ctx context.Context) error {
	if _, err := postgres.Conn(ctx, m.pool).Exec(ctx, `DELETE FROM task_list_v1`); err != nil {
		return fmt.Errorf("clear tasks: %w", err)
	}
	return nil
}
