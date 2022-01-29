//go:build (linux || darwin || freebsd || openbsd || netbsd) && !appengine
// +build linux darwin freebsd openbsd netbsd
// +build !appengine

package fastwalk_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charlievieth/fastwalk"
)

func formatFileInfo(fi os.FileInfo) string {
	return fmt.Sprintf("%+v", struct {
		Name    string
		Size    int64
		Mode    os.FileMode
		ModTime time.Time
		IsDir   bool
		Sys     string
	}{
		Name:    fi.Name(),
		Size:    fi.Size(),
		Mode:    fi.Mode(),
		ModTime: fi.ModTime(),
		IsDir:   fi.IsDir(),
		Sys:     fmt.Sprintf("%+v", fi.Sys()),
	})
}

func TestLstatDirent(t *testing.T) {
	tempdir := t.TempDir()

	fileName := filepath.Join(tempdir, "file.txt")
	if err := writeFile(fileName, "file.txt", 0644); err != nil {
		t.Fatal(err)
	}
	linkName := filepath.Join(tempdir, "link.link")
	if err := symlink(t, filepath.Base(fileName), linkName); err != nil {
		t.Fatal(err)
	}

	var linkEnt os.DirEntry
	var fileEnt os.DirEntry
	fastwalk.Walk(nil, tempdir, func(path string, d os.DirEntry, err error) error {
		switch path {
		case linkName:
			linkEnt = d
		case fileName:
			fileEnt = d
		}
		return nil
	})
	if fileEnt == nil || linkEnt == nil {
		t.Fatal("error walking directory")
	}

	t.Run("Lstat", func(t *testing.T) {
		want, err := os.Lstat(linkName)
		if err != nil {
			t.Fatal(err)
		}
		got, err := fastwalk.LstatDirent(linkName, linkEnt)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(want, got) {
			t.Errorf("lstat mismatch\n got:\n%s\nwant:\n%s", formatFileInfo(got), formatFileInfo(want))
		}
	})

	t.Run("Stat", func(t *testing.T) {
		want, err := os.Stat(fileName)
		if err != nil {
			t.Fatal(err)
		}
		got, err := fastwalk.StatDirent(linkName, fileEnt)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(want, got) {
			t.Errorf("lstat mismatch\n got:\n%s\nwant:\n%s", formatFileInfo(got), formatFileInfo(want))
		}
		fi, err := fileEnt.Info()
		if err != nil {
			t.Fatal(err)
		}
		if fi != got {
			t.Error("failed to return or cache FileInfo")
		}
		de := fileEnt.(fastwalk.DirEntry)
		fi, err = de.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if fi != got {
			t.Error("failed to use cached Info result for non-symlink")
		}
	})
}
