package tracing

import (
	"context"
	"errors"
	"testing"
)

func TestWithSpan(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		err := WithSpan(context.Background(), "test-span", func(ctx context.Context) error {
			return nil
		})
		if err != nil {
			t.Errorf("WithSpan() error = %v, want nil", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		expectedErr := errors.New("test error")
		err := WithSpan(context.Background(), "test-span", func(ctx context.Context) error {
			return expectedErr
		})
		if !errors.Is(err, expectedErr) {
			t.Errorf("WithSpan() error = %v, want %v", err, expectedErr)
		}
	})
}

func TestWithSpanResult(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		result, err := WithSpanResult(context.Background(), "test-span", func(ctx context.Context) (string, error) {
			return "hello", nil
		})
		if err != nil {
			t.Errorf("WithSpanResult() error = %v, want nil", err)
		}
		if result != "hello" {
			t.Errorf("WithSpanResult() result = %v, want hello", result)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		expectedErr := errors.New("test error")
		result, err := WithSpanResult(context.Background(), "test-span", func(ctx context.Context) (string, error) {
			return "", expectedErr
		})
		if !errors.Is(err, expectedErr) {
			t.Errorf("WithSpanResult() error = %v, want %v", err, expectedErr)
		}
		if result != "" {
			t.Errorf("WithSpanResult() result = %v, want empty", result)
		}
	})

	t.Run("struct result", func(t *testing.T) {
		t.Parallel()
		type User struct {
			ID   string
			Name string
		}
		result, err := WithSpanResult(context.Background(), "fetch-user", func(ctx context.Context) (User, error) {
			return User{ID: "123", Name: "Test"}, nil
		})
		if err != nil {
			t.Errorf("WithSpanResult() error = %v, want nil", err)
		}
		if result.ID != "123" || result.Name != "Test" {
			t.Errorf("WithSpanResult() result = %v, want {123 Test}", result)
		}
	})
}
