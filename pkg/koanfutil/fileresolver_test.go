package koanfutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/knadh/koanf/v2"
)

func TestFileResolver(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     map[string]any
		files     map[string]string // path -> content
		want      map[string]any
		wantErr   bool
		errSubstr string
	}{
		{
			name: "normal string unchanged",
			input: map[string]any{
				"host": "localhost",
				"port": 5432,
			},
			want: map[string]any{
				"host": "localhost",
				"port": 5432,
			},
		},
		{
			name: "file uri resolved",
			input: map[string]any{
				"password": "file://{tmpdir}/secret",
			},
			files: map[string]string{
				"secret": "mysecretpassword",
			},
			want: map[string]any{
				"password": "mysecretpassword",
			},
		},
		{
			name: "whitespace trimmed",
			input: map[string]any{
				"password": "file://{tmpdir}/secret",
			},
			files: map[string]string{
				"secret": "  mypassword  \n",
			},
			want: map[string]any{
				"password": "mypassword",
			},
		},
		{
			name: "nested map resolved",
			input: map[string]any{
				"database": map[string]any{
					"host":     "postgres.svc",
					"password": "file://{tmpdir}/db-password",
				},
			},
			files: map[string]string{
				"db-password": "dbpass123",
			},
			want: map[string]any{
				"database": map[string]any{
					"host":     "postgres.svc",
					"password": "dbpass123",
				},
			},
		},
		{
			name: "deeply nested resolved",
			input: map[string]any{
				"level1": map[string]any{
					"level2": map[string]any{
						"secret": "file://{tmpdir}/deep-secret",
					},
				},
			},
			files: map[string]string{
				"deep-secret": "deepsecretvalue",
			},
			want: map[string]any{
				"level1": map[string]any{
					"level2": map[string]any{
						"secret": "deepsecretvalue",
					},
				},
			},
		},
		{
			name: "empty file returns empty string",
			input: map[string]any{
				"token": "file://{tmpdir}/empty",
			},
			files: map[string]string{
				"empty": "",
			},
			want: map[string]any{
				"token": "",
			},
		},
		{
			name: "missing file returns error",
			input: map[string]any{
				"password": "file://{tmpdir}/nonexistent",
			},
			wantErr:   true,
			errSubstr: "nonexistent",
		},
		{
			name: "mixed resolved and unresolved",
			input: map[string]any{
				"host":     "localhost",
				"port":     5432,
				"user":     "admin",
				"password": "file://{tmpdir}/pass",
			},
			files: map[string]string{
				"pass": "secretpass",
			},
			want: map[string]any{
				"host":     "localhost",
				"port":     5432,
				"user":     "admin",
				"password": "secretpass",
			},
		},
		{
			name: "non-string values preserved",
			input: map[string]any{
				"enabled": true,
				"count":   42,
				"ratio":   3.14,
				"items":   []string{"a", "b"},
			},
			want: map[string]any{
				"enabled": true,
				"count":   42,
				"ratio":   3.14,
				"items":   []string{"a", "b"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()

			for name, content := range tt.files {
				path := filepath.Join(tmpDir, name)
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatalf("write test file: %v", err)
				}
			}

			input := substituteDir(tt.input, tmpDir)

			k := koanf.New(".")
			if err := k.Load(mapProvider(input), nil); err != nil {
				t.Fatalf("load config: %v", err)
			}

			err := k.Load(FileResolver(k), nil)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errSubstr != "" && !contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			want := substituteDir(tt.want, tmpDir)
			assertMapsEqual(t, want, k.Raw())
		})
	}
}

func TestFileResolver_ReadBytes(t *testing.T) {
	t.Parallel()

	k := koanf.New(".")
	resolver := FileResolver(k)

	_, err := resolver.ReadBytes()
	if err == nil {
		t.Error("expected error from ReadBytes, got nil")
	}
}

// mapProvider is a simple koanf.Provider for testing.
type mapProvider map[string]any

func (m mapProvider) Read() (map[string]any, error) {
	return m, nil
}

func (m mapProvider) ReadBytes() ([]byte, error) {
	return nil, nil
}

func substituteDir(m map[string]any, dir string) map[string]any {
	result := make(map[string]any, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case string:
			result[k] = substituteString(val, dir)
		case map[string]any:
			result[k] = substituteDir(val, dir)
		default:
			result[k] = v
		}
	}
	return result
}

func substituteString(s, dir string) string {
	return replaceAll(s, "{tmpdir}", dir)
}

func replaceAll(s, old, new string) string {
	for {
		idx := indexOf(s, old)
		if idx == -1 {
			return s
		}
		s = s[:idx] + new + s[idx+len(old):]
	}
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func contains(s, substr string) bool {
	return indexOf(s, substr) != -1
}

func assertMapsEqual(t *testing.T, want, got map[string]any) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("map length mismatch: want %d, got %d", len(want), len(got))
		return
	}
	for k, wantVal := range want {
		gotVal, ok := got[k]
		if !ok {
			t.Errorf("missing key %q", k)
			continue
		}
		wantMap, wantIsMap := wantVal.(map[string]any)
		gotMap, gotIsMap := gotVal.(map[string]any)
		if wantIsMap && gotIsMap {
			assertMapsEqual(t, wantMap, gotMap)
			continue
		}
		if !equalValues(wantVal, gotVal) {
			t.Errorf("key %q: want %v (%T), got %v (%T)", k, wantVal, wantVal, gotVal, gotVal)
		}
	}
}

func equalValues(a, b any) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case int:
		bv, ok := b.(int)
		return ok && av == bv
	case int64:
		bv, ok := b.(int64)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case []string:
		bv, ok := b.([]string)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
		return true
	default:
		return false
	}
}
