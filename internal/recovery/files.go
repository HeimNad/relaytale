// Package recovery implements offline, operator-owned backup bundles. It never
// starts a worker, changes delivery state, or releases the restore startup gate.
package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"relaytale/migrations"
)

const Marker = ".restore-pending"
const maxManifest = 64 << 20

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	StorageRoot string    `json:"storage_root"`
	WorkingDir  string    `json:"working_directory"`
	Schema      string    `json:"schema_sha256"`
	Files       []File    `json:"files"`
}

// CheckStartup fails closed even when the marker cannot be inspected.
func CheckStartup(root string) error {
	_, err := os.Lstat(filepath.Join(root, Marker))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.New("restore gate is present or inaccessible; verify and reconcile recovery before starting")
}

func schemaHash() string {
	h := sha256.New()
	entries, _ := fs.ReadDir(migrations.Files, ".")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, _ := migrations.Files.ReadFile(e.Name())
		h.Write([]byte(e.Name() + "\x00"))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func schemaVersion() int64 {
	entries, _ := fs.ReadDir(migrations.Files, ".")
	var latest int64
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(prefix, 10, 64)
		if err == nil && n > latest {
			latest = n
		}
	}
	return latest
}

func within(root, name string) bool {
	r, err := filepath.Rel(root, name)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(os.PathSeparator)) && !filepath.IsAbs(r)
}

// Paths must be local and have no symlink component. Bundles and roots require
// exclusive operator ownership throughout the operation (not a hostile filesystem).
func regular(path string) (fs.FileInfo, error) {
	a, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	for p := a; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symlinks are not supported")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	info, err := os.Stat(a)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("expected a regular file")
	}
	return info, nil
}

type reader struct {
	ctx context.Context
	io.Reader
}

func (r reader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(b)
}

func digest(ctx context.Context, path string) (File, error) {
	info, err := regular(path)
	if err != nil {
		return File{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, reader{ctx, f})
	if err != nil {
		return File{}, err
	}
	after, err := f.Stat()
	if err != nil || n != info.Size() || !os.SameFile(info, after) || !after.ModTime().Equal(info.ModTime()) {
		return File{}, errors.New("file changed during verification")
	}
	return File{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func mkdir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	// Sync the directory chain so a published manifest cannot outlive its parents.
	for p := path; ; p = filepath.Dir(p) {
		if err := syncDir(p); err != nil {
			return err
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return nil
}
func copyFile(ctx context.Context, src, dst string) (File, error) {
	if _, err := regular(src); err != nil {
		return File{}, err
	}
	if err := mkdir(filepath.Dir(dst)); err != nil {
		return File{}, err
	}
	in, err := os.Open(src)
	if err != nil {
		return File{}, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return File{}, err
	}
	defer out.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), reader{ctx, in})
	if err != nil {
		return File{}, err
	}
	if err = out.Sync(); err != nil {
		return File{}, err
	}
	if err = out.Close(); err != nil {
		return File{}, err
	}
	if err = syncDir(filepath.Dir(dst)); err != nil {
		return File{}, err
	}
	return File{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// VerifyBundle checks completeness and corruption, not authenticity. Only
// restore trusted bundles: PostgreSQL dumps contain executable SQL.
func VerifyBundle(ctx context.Context, dir string) (Manifest, error) {
	var m Manifest
	path := filepath.Join(dir, "manifest.json")
	info, err := regular(path)
	if err != nil {
		return m, err
	}
	if info.Size() > maxManifest {
		return m, errors.New("manifest too large")
	}
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, maxManifest+1))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil {
		return m, errors.New("invalid manifest")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return m, errors.New("trailing manifest content")
	}
	if m.Version != 1 || m.Schema != schemaHash() || !filepath.IsAbs(m.StorageRoot) || !filepath.IsAbs(m.WorkingDir) || (len(m.Files) == 0 || len(m.Files) > 100000) {
		return m, errors.New("unsupported bundle version, schema or paths")
	}
	seen := make(map[string]bool, len(m.Files))
	for _, v := range m.Files {
		if !fs.ValidPath(v.Path) || v.Path == "." || seen[v.Path] || (v.Path != "database.dump" && !strings.HasPrefix(v.Path, "eml/")) || v.Size < 0 {
			return m, errors.New("invalid bundle entry")
		}
		if v.Path == "eml/"+Marker {
			return m, errors.New("bundle contains restore gate")
		}
		seen[v.Path] = true
		got, err := digest(ctx, filepath.Join(dir, filepath.FromSlash(v.Path)))
		if err != nil {
			return m, err
		}
		if got.Size != v.Size || got.SHA256 != v.SHA256 {
			return m, errors.New("bundle checksum mismatch")
		}
	}
	if !seen["database.dump"] {
		return m, errors.New("missing database dump")
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("bundle symlink")
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(dir, path)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if rel != "manifest.json" && !seen[rel] {
			return errors.New("unlisted bundle file")
		}
		return nil
	})
	return m, err
}
