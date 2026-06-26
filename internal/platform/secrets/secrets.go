// Package secrets defines a seam for reading runtime secrets by name.
//
// The abstraction decouples callers from the concrete secret store
// (process environment, a cloud secrets manager, or an in-memory map during
// tests) so that implementations can be swapped without modifying call sites.
//
// Typical production wiring:
//
//	src := secrets.Env()
//	apiToken, ok := src.Get("API_TOKEN")
//
// Typical test wiring:
//
//	src := secrets.MapSource{"API_TOKEN": "test-token"}
//	apiToken, ok := src.Get("API_TOKEN")
package secrets

import "os"

// Source is the interface that wraps the basic Get method.
//
// Get returns the value associated with name and a boolean indicating whether
// the name was found.  The semantics mirror [os.LookupEnv]: a name that is
// set to the empty string returns ("", true).
type Source interface {
	Get(name string) (value string, ok bool)
}

// EnvSource is a [Source] backed by the process environment.
// It delegates every lookup to [os.LookupEnv].
// The zero value is ready to use.
type EnvSource struct{}

// Env returns an [EnvSource].  Use it when a named constructor is preferred
// over a struct literal, e.g. when assigning to a [Source] variable.
func Env() Source {
	return EnvSource{}
}

// Get looks up name in the process environment.
func (EnvSource) Get(name string) (string, bool) {
	return os.LookupEnv(name)
}

// MapSource is an in-memory [Source] backed by a plain string map.
// It is intended for use in tests where injecting a real environment or a
// remote secrets manager is impractical.
//
// Like [os.LookupEnv], a key that maps to the empty string returns ("", true).
type MapSource map[string]string

// Get looks up name in the in-memory map.
func (m MapSource) Get(name string) (string, bool) {
	v, ok := m[name]
	return v, ok
}
