package brainapi

import "testing"

func TestValidateBindingKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  BindingKey
		want bool
	}{
		{"slack channel", "slack:channel:C123", true},
		{"web session", "web:session:abc", true},
		{"missing part", "slack:channel", false},
		{"empty part", "slack::C123", false},
		{"space", "slack:channel:C 123", false},
		{"blank", "", false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateBindingKey(tt.key)
			if (err == nil) != tt.want {
				t.Fatalf("ValidateBindingKey(%q) err=%v, want valid=%v", tt.key, err, tt.want)
			}
		})
	}
}

func FuzzValidateBindingKey(f *testing.F) {
	for _, seed := range []string{"slack:channel:C123", "", "a:b:c", "a::c", "a:b:c d"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		err := ValidateBindingKey(BindingKey(s))
		if err == nil {
			parts := 0
			for _, r := range s {
				if r == ':' {
					parts++
				}
			}
			if parts != 2 {
				t.Fatalf("accepted key with wrong separator count: %q", s)
			}
		}
	})
}

func TestValidateTenantAndJobID(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		fn   func() error
		want bool
	}{
		{"tenant valid", func() error { return ValidateTenantID("tenant-a") }, true},
		{"tenant blank", func() error { return ValidateTenantID(" ") }, false},
		{"tenant space", func() error { return ValidateTenantID("tenant a") }, false},
		{"job valid", func() error { return ValidateJobID("job-a") }, true},
		{"job blank", func() error { return ValidateJobID("") }, false},
		{"job tab", func() error { return ValidateJobID("job\ta") }, false},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.fn()
			if (err == nil) != tt.want {
				t.Fatalf("err=%v want valid=%v", err, tt.want)
			}
		})
	}
}
