package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

const snapshotVersion = 1

type snapshot struct {
	Version  int                                        `json:"version"`
	Bindings map[brainapi.BindingKey]brainapi.TenantID  `json:"bindings"`
	Projects map[brainapi.TenantID]projectSnapshot      `json:"projects"`
	Jobs     map[string]brainapi.JobSnapshot            `json:"jobs"`
	Sources  map[brainapi.TenantID][]brainapi.SourceRef `json:"sources"`
	Docs     map[brainapi.TenantID][]documentSnapshot   `json:"docs"`
}

type projectSnapshot struct {
	Owner    brainapi.Principal    `json:"owner"`
	Metadata map[string]string     `json:"metadata,omitempty"`
	State    brainapi.ProjectState `json:"state"`
}

type documentSnapshot struct {
	Source  brainapi.SourceRef `json:"source"`
	Title   string             `json:"title"`
	Content string             `json:"content"`
	FreshAt time.Time          `json:"fresh_at"`
}

func (c *Core) loadSnapshot() error {
	if c.persistencePath == "" {
		return nil
	}
	data, err := os.ReadFile(c.persistencePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return brainapi.E(brainapi.KindInternal, "load_snapshot", "read memory snapshot", err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return brainapi.E(brainapi.KindInternal, "load_snapshot", "decode memory snapshot", err)
	}
	if snap.Version != snapshotVersion {
		return brainapi.E(brainapi.KindInternal, "load_snapshot", fmt.Sprintf("unsupported memory snapshot version %d", snap.Version), nil)
	}

	bindings := make(map[brainapi.BindingKey]brainapi.TenantID, len(snap.Bindings))
	for binding, tenantID := range snap.Bindings {
		bindings[binding] = tenantID
	}
	projects := make(map[brainapi.TenantID]project, len(snap.Projects))
	for tenantID, proj := range snap.Projects {
		projects[tenantID] = project{owner: proj.Owner, metadata: cloneMap(proj.Metadata), state: proj.State}
	}
	jobs := make(map[string]brainapi.JobSnapshot, len(snap.Jobs))
	for key, job := range snap.Jobs {
		jobs[key] = job
	}
	sources := make(map[brainapi.TenantID][]brainapi.SourceRef, len(snap.Sources))
	for tenantID, refs := range snap.Sources {
		sources[tenantID] = append([]brainapi.SourceRef(nil), refs...)
	}
	docs := make(map[brainapi.TenantID][]document, len(snap.Docs))
	// chunks is built locally then pushed to c.vectorStore so the VectorStore
	// seam receives a clean Replace rather than incremental Adds.
	chunksByTenant := make(map[brainapi.TenantID][]Chunk, len(snap.Docs))
	for tenantID, persistedDocs := range snap.Docs {
		docs[tenantID] = make([]document, 0, len(persistedDocs))
		for sourceIndex, persistedDoc := range persistedDocs {
			doc := document{source: persistedDoc.Source, title: persistedDoc.Title, content: persistedDoc.Content, freshAt: persistedDoc.FreshAt}
			docs[tenantID] = append(docs[tenantID], doc)
			chunksByTenant[tenantID] = append(chunksByTenant[tenantID], chunksFrom(tenantID, sourceIndex, doc, len(chunksByTenant[tenantID]), c.embedder)...)
		}
	}

	c.bindings = bindings
	c.projects = projects
	c.jobs = jobs
	c.sources = sources
	c.docs = docs
	for tenantID, chunks := range chunksByTenant {
		c.vectorStore.Replace(tenantID, chunks)
	}
	// TODO(WB-01-followup): abstract the durable snapshot store behind a
	// PersistenceStore interface so PostgreSQL or object-storage backends can
	// plug in here without re-refactoring the core.
	return nil
}

func (c *Core) persistLocked() error {
	if c.persistencePath == "" {
		return nil
	}
	data, err := marshalSnapshot(c.snapshotLocked())
	if err != nil {
		return brainapi.E(brainapi.KindInternal, "persist_snapshot", "encode memory snapshot", err)
	}
	if err := atomicWriteFile(c.persistencePath, data, 0o600); err != nil {
		return brainapi.E(brainapi.KindInternal, "persist_snapshot", "write memory snapshot", err)
	}
	return nil
}

func (c *Core) snapshotLocked() snapshot {
	bindings := make(map[brainapi.BindingKey]brainapi.TenantID, len(c.bindings))
	for binding, tenantID := range c.bindings {
		bindings[binding] = tenantID
	}
	projects := make(map[brainapi.TenantID]projectSnapshot, len(c.projects))
	for tenantID, proj := range c.projects {
		projects[tenantID] = projectSnapshot{Owner: proj.owner, Metadata: cloneMap(proj.metadata), State: proj.state}
	}
	jobs := make(map[string]brainapi.JobSnapshot, len(c.jobs))
	for key, job := range c.jobs {
		jobs[key] = job
	}
	sources := make(map[brainapi.TenantID][]brainapi.SourceRef, len(c.sources))
	for tenantID, refs := range c.sources {
		sources[tenantID] = append([]brainapi.SourceRef(nil), refs...)
	}
	docs := make(map[brainapi.TenantID][]documentSnapshot, len(c.docs))
	for tenantID, tenantDocs := range c.docs {
		docs[tenantID] = make([]documentSnapshot, 0, len(tenantDocs))
		for _, doc := range tenantDocs {
			docs[tenantID] = append(docs[tenantID], documentSnapshot{Source: doc.source, Title: doc.title, Content: doc.content, FreshAt: doc.freshAt})
		}
	}
	return snapshot{Version: snapshotVersion, Bindings: bindings, Projects: projects, Jobs: jobs, Sources: sources, Docs: docs}
}

type atomicTempFile interface {
	Name() string
	Write([]byte) (int, error)
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

var (
	marshalSnapshot = func(snap snapshot) ([]byte, error) {
		return json.MarshalIndent(snap, "", "  ")
	}
	createAtomicTemp = func(dir, pattern string) (atomicTempFile, error) {
		return os.CreateTemp(dir, pattern)
	}
	renameAtomicFile = os.Rename
)

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := createAtomicTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := renameAtomicFile(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
