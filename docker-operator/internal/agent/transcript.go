package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path"
	"strings"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/filestore"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
	"github.com/psenna/ai-sandbox/docker-operator/internal/wsbridge"
)

// maxTranscriptFileBytes caps any single file extracted into a transcript
// archive -- generous for a session JSONL file or the raw output log, but
// not unbounded against a corrupted or hostile tar stream.
const maxTranscriptFileBytes = 512 << 20

// transcriptMetadata is written as transcripts/<id>/metadata.json: once an
// agent is deleted its bare ID tells you nothing, so this is the only
// record of its identity an archive carries on its own.
type transcriptMetadata struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Harness     string    `json:"harness"`
	Backend     string    `json:"backend"`
	Repo        string    `json:"repo"`
	Image       string    `json:"image"`
	CreatedAt   time.Time `json:"created_at"`
	DeletedAt   time.Time `json:"deleted_at"`
}

// archiveTranscript copies a's session history into transcripts/<a.ID>/ in
// the centralized file store. It never returns an error: any failure (no
// file store, no container reference, a copy or write error) is logged at
// Warn and swallowed, so an archiving failure can never hold up Delete. See
// docs/superpowers/specs/2026-10-10-agent-transcript-archive-design.md.
//
// a must already have its derived names filled in (withDerivedNames) --
// teardown calls this after that step, so the container reference below
// mirrors teardown's own firstNonEmpty(a.ContainerID, a.ContainerName)
// pattern.
func (m *Manager) archiveTranscript(ctx context.Context, a store.Agent) {
	if m.files == nil {
		return
	}
	if a.ID == "" {
		return
	}
	ref := firstNonEmpty(a.ContainerID, a.ContainerName)
	if ref == "" {
		return
	}

	dest := path.Join(filestore.TranscriptsDir, a.ID)
	if err := m.files.Mkdir(dest); err != nil {
		m.log.WarnContext(ctx, "archiving transcript: creating destination directory", "agent_id", a.ID, "error", err)
		return
	}

	meta := transcriptMetadata{
		ID:          a.ID,
		Name:        a.Name,
		Description: a.Description,
		Harness:     HarnessOf(a),
		Backend:     a.Backend,
		Repo:        a.Repo,
		Image:       a.Image,
		CreatedAt:   a.CreatedAt,
		DeletedAt:   time.Now(),
	}
	if err := m.saveTranscriptMetadata(dest, meta); err != nil {
		// Keep going: the source copy below is the valuable part. A
		// missing metadata.json still leaves a diagnosable partial archive.
		m.log.WarnContext(ctx, "archiving transcript: writing metadata.json", "agent_id", a.ID, "error", err)
	}

	var srcPath string
	var extract func(io.Reader) error
	if HarnessOf(a) == config.HarnessOpenCode {
		srcPath = wsbridge.OutputLogPath
		extract = func(r io.Reader) error { return m.extractTranscriptFile(r, path.Join(dest, "output.log")) }
	} else {
		srcPath = configMount + "/projects"
		extract = func(r io.Reader) error { return m.extractTranscriptDir(r, dest) }
	}

	rc, err := m.docker.CopyFromContainer(ctx, ref, srcPath)
	if err != nil {
		if !dockerclient.IsNotFound(err) {
			m.log.WarnContext(ctx, "archiving transcript: copying from container", "agent_id", a.ID, "path", srcPath, "error", err)
		}
		// IsNotFound (nothing was ever written -- e.g. an agent deleted
		// before its first session) is the unremarkable common case, not a
		// failure worth a warning.
		return
	}
	defer func() { _ = rc.Close() }()

	if err := extract(rc); err != nil {
		m.log.WarnContext(ctx, "archiving transcript: extracting archive", "agent_id", a.ID, "error", err)
	}
}

func (m *Manager) saveTranscriptMetadata(dest string, meta transcriptMetadata) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	_, err = m.files.Save(path.Join(dest, "metadata.json"), bytes.NewReader(b), int64(len(b)))
	return err
}

// extractTranscriptDir walks r as a tar stream produced by copying a whole
// directory out of a container (CopyFromContainer with a directory
// SourcePath; see internal/dockerclient's CopyFromContainerLifecycle
// conformance case for the exact entry-naming convention this depends on)
// and writes every regular-file entry into the file store under destRoot,
// preserving the tar's own relative paths. Non-regular entries (directory
// headers, symlinks) are skipped, and so is any entry whose cleaned name
// tries to climb above destRoot (a leading ".." after path.Clean, which
// fully resolves any internal "a/../.." redundancy) -- the agent that
// produced this tar controls every name under its own
// $CLAUDE_CONFIG_DIR/projects/, so a crafted name must be assumed possible,
// not dismissed as something only a well-behaved process would write.
// destRoot's parent must already exist; Mkdir(path.Dir(...)) per file
// creates every other directory as needed, so an empty tar is a correct
// no-op.
func (m *Manager) extractTranscriptDir(r io.Reader, destRoot string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		rel := path.Clean(hdr.Name)
		if rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		dest := path.Join(destRoot, rel)
		if err := m.files.Mkdir(path.Dir(dest)); err != nil {
			return err
		}
		if _, err := m.files.Save(dest, tr, maxTranscriptFileBytes); err != nil {
			return err
		}
	}
}

// extractTranscriptFile reads the single regular-file entry r's tar stream
// carries -- CopyFromContainer always tar-wraps, even for a one-file source
// -- and writes it into the file store at dest, under the name dest names,
// not the tar entry's own name: Docker tars a single-file source as just
// its basename (e.g. ".agent-output.log"), but this package's own
// destination convention is "output.log". dest's parent directory must
// already exist.
func (m *Manager) extractTranscriptFile(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	hdr, err := tr.Next()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil
	}
	_, err = m.files.Save(dest, tr, maxTranscriptFileBytes)
	return err
}
