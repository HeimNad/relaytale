package operations

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type FileReport struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// WriteAtomic publishes only complete files, without replacing existing exports.
func WriteAtomic(path string, write func(io.Writer) error) (FileReport, error) {
	report := FileReport{Path: path}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return report, err
	}
	f, err := os.CreateTemp(dir, ".export-*")
	if err != nil {
		return report, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	hash := sha256.New()
	if err := write(io.MultiWriter(f, hash)); err != nil {
		return report, err
	}
	if err := f.Sync(); err != nil {
		return report, err
	}
	info, err := f.Stat()
	if err != nil {
		return report, err
	}
	if err := f.Close(); err != nil {
		return report, err
	}
	if err := os.Link(f.Name(), path); err != nil {
		return report, err
	}
	if err := os.Remove(f.Name()); err != nil {
		return report, err
	}
	d, err := os.Open(dir)
	if err != nil {
		return report, err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return report, err
	}
	report.SHA256 = fmt.Sprintf("%x", hash.Sum(nil))
	report.Bytes = info.Size()
	return report, nil
}
