package secrets_test

import (
	"testing"

	"github.com/sangyi/workspace-brain/internal/platform/secrets"
)

// TestEnv exercises the Env() constructor and EnvSource.Get.
// It covers the present-key and absent-key branches of EnvSource.
func TestEnv(t *testing.T) {
	t.Setenv("WB_SECRETS_TEST_KEY", "hello")
	src := secrets.Env()

	// Present key: value and ok must both be returned correctly.
	v, ok := src.Get("WB_SECRETS_TEST_KEY")
	if !ok || v != "hello" {
		t.Fatalf("Get(present) = %q, %v; want %q, true", v, ok, "hello")
	}

	// Absent key: ok must be false regardless of value.
	_, ok = src.Get("_WB_SECRETS_DEFINITELY_ABSENT_XYZ_789_")
	if ok {
		t.Fatal("Get(absent) ok=true; want false")
	}
}

// TestMapSource exercises the in-memory implementation used in tests.
// It checks present keys (including an empty-string value) and an absent key.
func TestMapSource(t *testing.T) {
	m := secrets.MapSource{
		"KEY":   "val",
		"EMPTY": "",
	}

	// Present key with non-empty value.
	v, ok := m.Get("KEY")
	if !ok || v != "val" {
		t.Fatalf("Get(KEY) = %q, %v; want %q, true", v, ok, "val")
	}

	// Present key with empty-string value mirrors os.LookupEnv semantics.
	v, ok = m.Get("EMPTY")
	if !ok || v != "" {
		t.Fatalf("Get(EMPTY) = %q, %v; want \"\", true", v, ok)
	}

	// Absent key.
	_, ok = m.Get("ABSENT")
	if ok {
		t.Fatal("Get(ABSENT) ok=true; want false")
	}
}
