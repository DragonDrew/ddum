// Package configenv builds the environment map that bot and agent config
// parsing reads from, resolving <NAME>_FILE secret variants.
package configenv

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/caarlos0/env/v11"
)

// Parse fills target (a pointer to a struct with `env` tags) from m. Parse
// errors name the environment variable rather than the Go field, so users see
// "REGISTRY_CHECK_INTERVAL: invalid value" instead of an internal name.
func Parse(target any, m map[string]string) error {
	err := env.ParseWithOptions(target, env.Options{Environment: m})
	if err == nil {
		return nil
	}
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return err
	}
	t := reflect.TypeOf(target).Elem()
	out := make([]error, 0, len(agg.Errors))
	for _, e := range agg.Errors {
		var pe env.ParseError
		if errors.As(e, &pe) {
			if f, ok := t.FieldByName(pe.Name); ok {
				name, _, _ := strings.Cut(f.Tag.Get("env"), ",")
				e = fmt.Errorf("%s: invalid value: %w", name, pe.Err)
			}
		}
		out = append(out, e)
	}
	return errors.Join(out...)
}

// Resolve turns environ ("KEY=value" pairs, as from os.Environ) into a map.
// For each name in secrets, if <NAME>_FILE is set, the file's contents
// (with one trailing newline trimmed) replace <NAME>; the _FILE variant wins
// when both are set. readFile is os.ReadFile outside tests.
func Resolve(environ []string, secrets []string, readFile func(string) ([]byte, error)) (map[string]string, error) {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k != "" {
			m[k] = v
		}
	}
	for _, name := range secrets {
		path, ok := m[name+"_FILE"]
		if !ok || path == "" {
			continue
		}
		data, err := readFile(path)
		if err != nil {
			// The error names the variable and path, never the contents.
			return nil, fmt.Errorf("%s_FILE: %w", name, err)
		}
		m[name] = strings.TrimRight(string(data), "\r\n")
	}
	return m, nil
}

// FromOS is Resolve over the process environment and filesystem.
func FromOS(secrets []string) (map[string]string, error) {
	return Resolve(os.Environ(), secrets, os.ReadFile)
}
