// Package koanfutil provides utilities for koanf configuration loading.
package koanfutil

import (
	"fmt"
	"os"
	"strings"

	"github.com/knadh/koanf/v2"
)

const fileURIPrefix = "file://"

// fileResolver implements koanf.Provider for resolving file:// URIs.
type fileResolver struct {
	k *koanf.Koanf
}

// FileResolver returns a koanf.Provider that resolves file:// URIs
// in string values to their file contents.
//
// Usage:
//
//	k := koanf.New(".")
//	k.Load(file.Provider("config.toml"), toml.Parser())
//	k.Load(koanfutil.FileResolver(k), nil)  // Resolves file:// URIs
//
// String values like "file:///etc/secrets/password" are replaced
// with the trimmed contents of /etc/secrets/password.
// Returns error if any file:// URI cannot be resolved.
func FileResolver(k *koanf.Koanf) koanf.Provider {
	return &fileResolver{k: k}
}

// Read returns config with all file:// URIs resolved.
func (r *fileResolver) Read() (map[string]any, error) {
	return r.resolveFileURIs(r.k.Raw())
}

// ReadBytes is not supported for this provider.
func (r *fileResolver) ReadBytes() ([]byte, error) {
	return nil, fmt.Errorf("koanfutil: ReadBytes not supported")
}

func (r *fileResolver) resolveFileURIs(m map[string]any) (map[string]any, error) {
	result := make(map[string]any, len(m))
	for key, val := range m {
		switch v := val.(type) {
		case string:
			resolved, err := r.resolveString(v)
			if err != nil {
				return nil, fmt.Errorf("resolve %s: %w", key, err)
			}
			result[key] = resolved
		case map[string]any:
			nested, err := r.resolveFileURIs(v)
			if err != nil {
				return nil, err
			}
			result[key] = nested
		default:
			result[key] = v
		}
	}
	return result, nil
}

func (r *fileResolver) resolveString(s string) (string, error) {
	if !strings.HasPrefix(s, fileURIPrefix) {
		return s, nil
	}
	path := strings.TrimPrefix(s, fileURIPrefix)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}
