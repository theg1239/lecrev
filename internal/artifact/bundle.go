package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func BundleFromFiles(files map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)

	for _, name := range paths {
		clean, err := cleanArchivePath(name)
		if err != nil {
			return nil, err
		}
		content := files[name]
		hdr := &tar.Header{
			Name: clean,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(content); err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func BundleFromDirectory(root string, extraFiles map[string][]byte) ([]byte, error) {
	files := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = content
		return nil
	})
	if err != nil {
		return nil, err
	}
	for name, content := range extraFiles {
		files[name] = content
	}
	return BundleFromFiles(files)
}

func ExtractTarGz(data []byte, dest string) error {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean, err := cleanArchivePath(hdr.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.FromSlash(clean))
		if !strings.HasPrefix(target, dest) {
			return fmt.Errorf("archive path %q escapes destination", hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
}

func cleanArchivePath(path string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(path))
	clean = strings.TrimPrefix(clean, "./")
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("invalid archive path %q", path)
	}
	return clean, nil
}
