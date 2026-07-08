package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestPersistentCoreLoadsExistingSnapshot(t *testing.T) {
	fixed := time.Date(2026, 6, 25, 1, 2, 3, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "memory.json")
	ctx := context.Background()

	core, err := NewPersistent(path, WithClock(func() time.Time { return fixed }))
	if err != nil {
		t.Fatalf("NewPersistent: %v", err)
	}
	mustCreate(t, core, "tenant-a", "slack:channel:C1")
	if err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1", Source: brainapi.SourceRef{URI: "text://a", Name: "alpha", MimeType: "text/plain"}, Metadata: map[string]string{"content": "durable alpha content"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := core.CompleteJob("tenant-a", "job-1", brainapi.JobCompleted, "result://1", ""); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	if err := core.SetProjectState(ctx, "tenant-a", brainapi.ProjectOff); err != nil {
		t.Fatalf("SetProjectState off: %v", err)
	}

	loaded, err := NewPersistent(path)
	if err != nil {
		t.Fatalf("reload NewPersistent: %v", err)
	}
	tenantID, err := loaded.ResolveBinding(ctx, "slack:channel:C1")
	if err != nil {
		t.Fatalf("ResolveBinding after reload: %v", err)
	}
	if tenantID != "tenant-a" {
		t.Fatalf("tenant after reload = %q", tenantID)
	}
	status, err := loaded.JobStatus(ctx, "tenant-a", "job-1")
	if err != nil {
		t.Fatalf("JobStatus after reload: %v", err)
	}
	if status.Status != brainapi.JobCompleted || status.ResultRef != "result://1" || !status.UpdatedAt.Equal(fixed) {
		t.Fatalf("status after reload = %+v", status)
	}
	_, err = loaded.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "alpha"})
	if !brainapi.IsKind(err, brainapi.KindConflict) {
		t.Fatalf("off state not loaded kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if err := loaded.SetProjectState(ctx, "tenant-a", brainapi.ProjectOn); err != nil {
		t.Fatalf("SetProjectState on after reload: %v", err)
	}
	answer, err := loaded.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "alpha"})
	if err != nil {
		t.Fatalf("Query after reload: %v", err)
	}
	if !answer.GroundingAvailable || len(answer.Sources) != 1 || answer.Sources[0].TenantID != "tenant-a" || !strings.Contains(answer.Answer, "durable alpha") {
		t.Fatalf("reloaded chunks missing or leaked: %+v", answer)
	}
}

func TestPersistentCoreMutationPersists(t *testing.T) {
	fixed := time.Date(2026, 6, 25, 4, 5, 6, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "memory.json")
	ctx := context.Background()
	core, err := NewPersistent(path, WithClock(func() time.Time { return fixed }))
	if err != nil {
		t.Fatalf("NewPersistent: %v", err)
	}

	tests := []struct {
		name       string
		mutate     func() error
		wantInFile []string
	}{
		{
			name: "create_project",
			mutate: func() error {
				return core.CreateProject(ctx, brainapi.CreateProjectRequest{TenantID: "tenant-a", BindingKey: "api:space:A", OwnerPrincipal: brainapi.Principal{ID: "owner"}, Metadata: map[string]string{"tier": "local"}})
			},
			wantInFile: []string{"tenant-a", "api:space:A", "local"},
		},
		{
			name: "ingest",
			mutate: func() error {
				return core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1", Source: brainapi.SourceRef{URI: "text://persist", Name: "persist-doc", MimeType: "text/plain"}, Metadata: map[string]string{"content": "persisted chunk text"}})
			},
			wantInFile: []string{"job-1", "text://persist", "persisted chunk text"},
		},
		{
			name: "complete_job",
			mutate: func() error {
				return core.CompleteJob("tenant-a", "job-1", brainapi.JobFailed, "", "worker failed")
			},
			wantInFile: []string{"failed", "worker failed"},
		},
		{
			name: "set_project_state",
			mutate: func() error {
				return core.SetProjectState(ctx, "tenant-a", brainapi.ProjectOff)
			},
			wantInFile: []string{`"state": "off"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.mutate(); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile snapshot: %v", err)
			}
			for _, want := range tt.wantInFile {
				if !strings.Contains(string(data), want) {
					t.Fatalf("snapshot missing %q in %s", want, data)
				}
			}
		})
	}
}

func TestPersistentCoreWrapsWriteErrors(t *testing.T) {
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("file blocks mkdir"), 0o600); err != nil {
		t.Fatalf("WriteFile blocker: %v", err)
	}
	core := newCore(WithPersistence(filepath.Join(blockedParent, "memory.json")))
	err := core.CreateProject(context.Background(), brainapi.CreateProjectRequest{TenantID: "tenant-a", BindingKey: "api:space:A", OwnerPrincipal: brainapi.Principal{ID: "owner"}})
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("persist error kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if !strings.Contains(err.Error(), "persist_snapshot") {
		t.Fatalf("persist error not wrapped with op: %v", err)
	}
	// After rollback the in-memory state must be clean: the binding must not exist.
	_, resolveErr := core.ResolveBinding(context.Background(), "api:space:A")
	if !brainapi.IsKind(resolveErr, brainapi.KindNotFound) {
		t.Fatalf("binding should not exist after CreateProject rollback, kind=%q err=%v", brainapi.KindOf(resolveErr), resolveErr)
	}
}

// TestMutationRollsBackOnPersistFailure verifies that Ingest, CompleteJob, and
// SetProjectState roll back their in-memory writes atomically when persistLocked fails.
// marshalSnapshotFn is injected on each test's own Core instance to trigger a
// deterministic persist failure without touching the filesystem.
func TestMutationRollsBackOnPersistFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("ingest", func(t *testing.T) {
		core := newCore(WithPersistence(filepath.Join(t.TempDir(), "m.json")))
		core.projects[brainapi.TenantID("tenant-a")] = project{owner: brainapi.Principal{ID: "owner"}, state: brainapi.ProjectOn}
		core.marshalSnapshotFn = func(_ snapshot) ([]byte, error) { return nil, errors.New("encode failed") }

		err := core.Ingest(ctx, brainapi.IngestRequest{TenantID: "tenant-a", JobID: "job-1", Source: brainapi.SourceRef{URI: "file://a"}})
		if !brainapi.IsKind(err, brainapi.KindInternal) {
			t.Fatalf("ingest persist fail kind=%q err=%v", brainapi.KindOf(err), err)
		}
		// After rollback the job must not exist.
		_, jobErr := core.JobStatus(ctx, "tenant-a", "job-1")
		if !brainapi.IsKind(jobErr, brainapi.KindNotFound) {
			t.Fatalf("job should be rolled back, kind=%q err=%v", brainapi.KindOf(jobErr), jobErr)
		}
	})

	t.Run("complete_job", func(t *testing.T) {
		core := newCore(WithPersistence(filepath.Join(t.TempDir(), "m.json")))
		core.projects[brainapi.TenantID("tenant-a")] = project{owner: brainapi.Principal{ID: "owner"}, state: brainapi.ProjectOn}
		core.jobs[jobKey("tenant-a", "job-1")] = brainapi.JobSnapshot{TenantID: "tenant-a", JobID: "job-1", Status: brainapi.JobRunning}
		core.marshalSnapshotFn = func(_ snapshot) ([]byte, error) { return nil, errors.New("encode failed") }

		err := core.CompleteJob("tenant-a", "job-1", brainapi.JobCompleted, "result://1", "")
		if !brainapi.IsKind(err, brainapi.KindInternal) {
			t.Fatalf("complete job persist fail kind=%q err=%v", brainapi.KindOf(err), err)
		}
		// After rollback the job must still be Running.
		status, statusErr := core.JobStatus(ctx, "tenant-a", "job-1")
		if statusErr != nil {
			t.Fatalf("JobStatus after rollback: %v", statusErr)
		}
		if status.Status != brainapi.JobRunning {
			t.Fatalf("status should be rolled back to Running, got %q", status.Status)
		}
	})

	t.Run("set_project_state", func(t *testing.T) {
		core := newCore(WithPersistence(filepath.Join(t.TempDir(), "m.json")))
		core.projects[brainapi.TenantID("tenant-a")] = project{owner: brainapi.Principal{ID: "owner"}, state: brainapi.ProjectOn}
		core.marshalSnapshotFn = func(_ snapshot) ([]byte, error) { return nil, errors.New("encode failed") }

		err := core.SetProjectState(ctx, "tenant-a", brainapi.ProjectOff)
		if !brainapi.IsKind(err, brainapi.KindInternal) {
			t.Fatalf("set state persist fail kind=%q err=%v", brainapi.KindOf(err), err)
		}
		// After rollback the project must still be On: Query must not return KindConflict.
		_, queryErr := core.Query(ctx, brainapi.QueryRequest{TenantID: "tenant-a", Question: "test"})
		if queryErr != nil {
			t.Fatalf("project should be rolled back to On state, got err: %v", queryErr)
		}
	})
}

func TestPersistentCoreWrapsMarshalErrors(t *testing.T) {
	core := newCore(WithPersistence(filepath.Join(t.TempDir(), "memory.json")))
	core.marshalSnapshotFn = func(snapshot) ([]byte, error) {
		return nil, errors.New("encode failed")
	}
	err := core.CreateProject(context.Background(), brainapi.CreateProjectRequest{TenantID: "tenant-a", BindingKey: "api:space:A", OwnerPrincipal: brainapi.Principal{ID: "owner"}})
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("marshal error kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if !strings.Contains(err.Error(), "encode memory snapshot") {
		t.Fatalf("marshal error not wrapped: %v", err)
	}
}

func TestPersistentCoreLoadErrorsAreInternal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0o600); err != nil {
		t.Fatalf("WriteFile snapshot: %v", err)
	}
	_, err := NewPersistent(path)
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("load error kind=%q err=%v", brainapi.KindOf(err), err)
	}

	core := New(WithPersistence(path))
	_, err = core.ResolveBinding(context.Background(), "api:space:A")
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("deferred load error kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoadSnapshotReturnsNilWhenPersistenceIsDisabled(t *testing.T) {
	t.Parallel()
	core := New()
	if err := core.loadSnapshot(); err != nil {
		t.Fatalf("loadSnapshot without path: %v", err)
	}
}

func TestPersistentCoreWrapsReadAndDecodeErrors(t *testing.T) {
	t.Parallel()
	t.Run("read directory", func(t *testing.T) {
		_, err := NewPersistent(t.TempDir())
		if !brainapi.IsKind(err, brainapi.KindInternal) {
			t.Fatalf("read error kind=%q err=%v", brainapi.KindOf(err), err)
		}
		if !strings.Contains(err.Error(), "read memory snapshot") {
			t.Fatalf("read error not wrapped: %v", err)
		}
	})

	t.Run("decode invalid json", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "memory.json")
		if err := os.WriteFile(path, []byte(`{not-json`), 0o600); err != nil {
			t.Fatalf("WriteFile invalid snapshot: %v", err)
		}
		_, err := NewPersistent(path)
		if !brainapi.IsKind(err, brainapi.KindInternal) {
			t.Fatalf("decode error kind=%q err=%v", brainapi.KindOf(err), err)
		}
		if !strings.Contains(err.Error(), "decode memory snapshot") {
			t.Fatalf("decode error not wrapped: %v", err)
		}
	})
}

func TestSyncDirReturnsOpenErrorForMissingDirectory(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	if err := syncDir(missing); err == nil {
		t.Fatalf("syncDir(%q) succeeded, want open error", missing)
	}
}

func TestAtomicWriteFileReturnsCreateTempAndRenameErrors(t *testing.T) {
	t.Run("create temp error", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("directory permission bits do not reliably block CreateTemp on Windows")
		}
		dir := filepath.Join(t.TempDir(), "readonly")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatalf("Mkdir readonly: %v", err)
		}
		defer func() { _ = os.Chmod(dir, 0o700) }()
		err := atomicWriteFile(filepath.Join(dir, "memory.json"), []byte("{}"), 0o600, defaultCreateAtomicTemp, os.Rename)
		if err == nil {
			t.Fatalf("atomicWriteFile in readonly dir succeeded, want CreateTemp error")
		}
	})

	t.Run("rename over directory error", func(t *testing.T) {
		targetDir := filepath.Join(t.TempDir(), "memory.json")
		if err := os.Mkdir(targetDir, 0o755); err != nil {
			t.Fatalf("Mkdir target directory: %v", err)
		}
		err := atomicWriteFile(targetDir, []byte("{}"), 0o600, defaultCreateAtomicTemp, os.Rename)
		if err == nil {
			t.Fatalf("atomicWriteFile over directory succeeded, want Rename error")
		}
	})
}

func TestAtomicWriteFileReturnsInjectedFileOperationErrors(t *testing.T) {
	tests := []struct {
		name string
		file *failingAtomicTemp
	}{
		{
			name: "write",
			file: &failingAtomicTemp{writeErr: errors.New("write failed")},
		},
		{
			name: "chmod",
			file: &failingAtomicTemp{chmodErr: errors.New("chmod failed")},
		},
		{
			name: "sync",
			file: &failingAtomicTemp{syncErr: errors.New("sync failed")},
		},
		{
			name: "close",
			file: &failingAtomicTemp{closeErr: errors.New("close failed")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			createTemp := func(dir, pattern string) (atomicTempFile, error) {
				tt.file.name = filepath.Join(dir, "."+tt.name+".tmp")
				return tt.file, nil
			}

			err := atomicWriteFile(filepath.Join(t.TempDir(), "memory.json"), []byte("{}"), 0o600, createTemp, os.Rename)
			if err == nil {
				t.Fatalf("atomicWriteFile succeeded, want %s error", tt.name)
			}
		})
	}
}

func TestAtomicWriteFileReturnsInjectedCreateTempAndRenameErrors(t *testing.T) {
	t.Run("create temp", func(t *testing.T) {
		createTemp := func(dir, pattern string) (atomicTempFile, error) {
			return nil, errors.New("create temp failed")
		}

		err := atomicWriteFile(filepath.Join(t.TempDir(), "memory.json"), []byte("{}"), 0o600, createTemp, os.Rename)
		if err == nil {
			t.Fatalf("atomicWriteFile succeeded, want create temp error")
		}
	})

	t.Run("rename", func(t *testing.T) {
		renameFile := func(oldpath, newpath string) error {
			return errors.New("rename failed")
		}

		err := atomicWriteFile(filepath.Join(t.TempDir(), "memory.json"), []byte("{}"), 0o600, defaultCreateAtomicTemp, renameFile)
		if err == nil {
			t.Fatalf("atomicWriteFile succeeded, want rename error")
		}
	})
}

type failingAtomicTemp struct {
	name     string
	writeErr error
	chmodErr error
	syncErr  error
	closeErr error
}

func (f *failingAtomicTemp) Name() string {
	return f.name
}

func (f *failingAtomicTemp) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *failingAtomicTemp) Chmod(os.FileMode) error {
	return f.chmodErr
}

func (f *failingAtomicTemp) Sync() error {
	return f.syncErr
}

func (f *failingAtomicTemp) Close() error {
	return f.closeErr
}
