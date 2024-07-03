// Package fastwalk provides a faster version of filepath.Walk for file system
// scanning tools.
package fastwalk

/*
 * This code borrows heavily from golang.org/x/tools/internal/fastwalk
 * and as such the Go license can be found in the go.LICENSE file and
 * is reproduced below:
 *
 * Copyright (c) 2009 The Go Authors. All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are
 * met:
 *
 *    * Redistributions of source code must retain the above copyright
 * notice, this list of conditions and the following disclaimer.
 *    * Redistributions in binary form must reproduce the above
 * copyright notice, this list of conditions and the following disclaimer
 * in the documentation and/or other materials provided with the
 * distribution.
 *    * Neither the name of Google Inc. nor the names of its
 * contributors may be used to endorse or promote products derived from
 * this software without specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
 * "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
 * LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
 * A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
 * OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
 * SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
 * LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
 * DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
 * THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
 * (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
 * OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
 */

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// ErrTraverseLink is used as a return value from WalkFuncs to indicate that the
// symlink named in the call may be traversed.
var ErrTraverseLink = errors.New("fastwalk: traverse symlink, assuming target is a directory")

// ErrSkipFiles is a used as a return value from WalkFuncs to indicate that the
// callback should not be called for any other files in the current directory.
// Child directories will still be traversed.
var ErrSkipFiles = errors.New("fastwalk: skip remaining files in directory")

// SkipDir is used as a return value from WalkDirFuncs to indicate that
// the directory named in the call is to be skipped. It is not returned
// as an error by any function.
var SkipDir = fs.SkipDir

// DefaultNumWorkers returns the default number of worker goroutines to use in
// [fastwalk.Walk] and is the value of [runtime.GOMAXPROCS](-1) clamped to a range
// of 4 to 32 except on Darwin where it is either 4 (8 cores or less) or 6
// (more than 8 cores). This is because Walk / IO performance on Darwin
// degrades with more concurrency.
//
// The optimal number for your workload may be lower or higher. The results
// of BenchmarkFastWalkNumWorkers benchmark may be informative.
func DefaultNumWorkers() int {
	numCPU := runtime.GOMAXPROCS(-1)
	if numCPU < 4 {
		return 4
	}
	// Darwin IO performance on APFS slows with more workers.
	// Stat performance is best around 2-4 and file IO is best
	// around 4-6. More workers only benefit CPU intensive tasks.
	if runtime.GOOS == "darwin" {
		if numCPU <= 8 {
			return 4
		}
		return 6
	}
	if numCPU > 32 {
		return 32
	}
	return numCPU
}

// DefaultToSlash returns true if this is a Go program compiled for Windows
// running in an environment ([WSL] or [MSYS/MSYS2]) that uses forward slashes
// as the path separator instead of the native backslash.
// On all other platforms this is a no-op and returns false since the native
// path separator is a forward slash and does not need to be converted.
//
// This check does not apply to programs compiled in [WSL] [MSYS/MSYS2] or for
// Linux (GOOS=linux). It only applies to Go programs compiled for Windows
// (GOOS=windows) that are executed from [WSL] or [MSYS/MSYS2].
//
// To detect if we're running in [MSYS/MSYS2] we check that "MSYSTEM" environment
// variable is either "MINGW64", "MINGW32", or "MSYS".
//
// The following heuristics are used to detect if we're running in [WSL]:
//
//   - Existence of "/proc/sys/fs/binfmt_misc/WSLInterop".
//   - If the "WSL_DISTRO_NAME" environment variable is set.
//   - If "/proc/version" contains either "Microsoft" or "microsoft".
//
// The result of the WSL check is cached for performance reasons.
//
// See: https://github.com/junegunn/fzf/issues/3859
//
// [WSL]: https://learn.microsoft.com/en-us/windows/wsl/about
// [MSYS/MSYS2]: https://www.msys2.org/
func DefaultToSlash() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	return useForwardSlash()
}

// DefaultConfig is the default Config used when none is supplied.
var DefaultConfig = Config{
	Follow:     false,
	ToSlash:    DefaultToSlash(),
	NumWorkers: DefaultNumWorkers(),
}

type Config struct {
	// TODO: do we want to pass a sentinel error to WalkFunc if
	// a symlink loop is detected?

	// Follow symbolic links ignoring directories that would lead
	// to infinite loops; that is, entering a previously visited
	// directory that is an ancestor of the last file encountered.
	//
	// The sentinel error ErrTraverseLink is ignored when Follow
	// is true (this to prevent users from defeating the loop
	// detection logic), but SkipDir and ErrSkipFiles are still
	// respected.
	Follow bool

	// Join all paths using a forward slash "/" instead of the system
	// default (the root path will be converted with filepath.ToSlash).
	// This option exists for users on Windows Subsystem for Linux (WSL)
	// that are running a Windows executable (like FZF) in WSL and need
	// forward slashes for compatibility (since binary was compiled for
	// Windows the path separator will be "\" which can cause issues in
	// in a Unix shell).
	//
	// This option has no effect when the OS path separator is a
	// forward slash "/".
	//
	// See FZF issue: https://github.com/junegunn/fzf/issues/3859
	ToSlash bool

	// Number of parallel workers to use. If NumWorkers if ≤ 0 then
	// [DefaultNumWorkers] is used.
	NumWorkers int
}

// A DirEntry extends the fs.DirEntry interface to add a Stat() method
// that returns the result of calling os.Stat() on the underlying file.
// The results of Info() and Stat() are cached.
//
// The fs.DirEntry argument passed to the fs.WalkDirFunc by Walk is
// always a DirEntry. The only exception is the root directory with
// with Walk is called.
type DirEntry interface {
	fs.DirEntry

	// Stat returns the FileInfo for the file or subdirectory described
	// by the entry. The returned FileInfo may be from the time of the
	// original directory read or from the time of the call to Stat.
	// If the entry denotes a symbolic link, Stat reports the information
	// about the target itself, not the link.
	Stat() (fs.FileInfo, error)
}

// Walk is a faster implementation of filepath.Walk.
//
// filepath.Walk's design necessarily calls os.Lstat on each file, even if
// the caller needs less info. Many tools need only the type of each file.
// On some platforms, this information is provided directly by the readdir
// system call, avoiding the need to stat each file individually.
// fastwalk_unix.go contains a fork of the syscall routines.
//
// See golang.org/issue/16399
//
// Walk walks the file tree rooted at root, calling walkFn for each file or
// directory in the tree, including root.
//
// If walkFn returns filepath.SkipDir, the directory is skipped.
//
// Unlike filepath.WalkDir:
//   - File stat calls must be done by the user and should be done via
//     the DirEntry argument to walkFn since it caches the results of
//     Stat and Lstat.
//   - The fs.DirEntry argument is always a fastwalk.DirEntry, which has
//     a Stat() method that returns the result of calling os.Stat() on the
//     file. The result of Stat() may be cached.
//   - Multiple goroutines stat the filesystem concurrently. The provided
//     walkFn must be safe for concurrent use.
//   - Walk can follow symlinks if walkFn returns the ErrTraverseLink
//     sentinel error. It is the walkFn's responsibility to prevent
//     Walk from going into symlink cycles.
func Walk(conf *Config, root string, walkFn fs.WalkDirFunc) error {
	fi, err := os.Stat(root)
	if err != nil {
		return err
	}
	if conf == nil {
		dupe := DefaultConfig
		conf = &dupe
	}
	if conf.ToSlash {
		root = filepath.ToSlash(root)
	}

	// Make sure to wait for all workers to finish, otherwise
	// walkFn could still be called after returning. This Wait call
	// runs after close(e.donec) below.
	var wg sync.WaitGroup
	defer wg.Wait()

	numWorkers := conf.NumWorkers
	if numWorkers <= 0 {
		numWorkers = DefaultNumWorkers()
	}

	w := &walker{
		fn:       walkFn,
		enqueuec: make(chan walkItem, numWorkers), // buffered for performance
		workc:    make(chan walkItem, numWorkers), // buffered for performance
		donec:    make(chan struct{}),

		// buffered for correctness & not leaking goroutines:
		resc: make(chan error, numWorkers),

		follow:  conf.Follow,
		toSlash: conf.ToSlash,
	}
	if w.follow {
		w.ignoredDirs = append(w.ignoredDirs, fi)
	}

	defer close(w.donec)

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go w.doWork(&wg)
	}

	root = cleanRootPath(root)
	todo := []walkItem{{dir: root, info: fileInfoToDirEntry(filepath.Dir(root), fi)}}
	out := 0
	for {
		workc := w.workc
		var workItem walkItem
		if len(todo) == 0 {
			workc = nil
		} else {
			workItem = todo[len(todo)-1]
		}
		select {
		case workc <- workItem:
			todo = todo[:len(todo)-1]
			out++
		case it := <-w.enqueuec:
			todo = append(todo, it)
		case err := <-w.resc:
			out--
			if err != nil {
				return err
			}
			if out == 0 && len(todo) == 0 {
				// It's safe to quit here, as long as the buffered
				// enqueue channel isn't also readable, which might
				// happen if the worker sends both another unit of
				// work and its result before the other select was
				// scheduled and both w.resc and w.enqueuec were
				// readable.
				select {
				case it := <-w.enqueuec:
					todo = append(todo, it)
				default:
					return nil
				}
			}
		}
	}
}

// doWork reads directories as instructed (via workc) and runs the
// user's callback function.
func (w *walker) doWork(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-w.donec:
			return
		case it := <-w.workc:
			select {
			case <-w.donec:
				return
			case w.resc <- w.walk(it.dir, it.info, !it.callbackDone):
			}
		}
	}
}

type walker struct {
	fn fs.WalkDirFunc

	donec    chan struct{} // closed on fastWalk's return
	workc    chan walkItem // to workers
	enqueuec chan walkItem // from workers
	resc     chan error    // from workers

	ignoredDirs []os.FileInfo
	follow      bool
	toSlash     bool
}

type walkItem struct {
	dir          string
	info         fs.DirEntry
	callbackDone bool // callback already called; don't do it again
}

func (w *walker) enqueue(it walkItem) {
	select {
	case w.enqueuec <- it:
	case <-w.donec:
	}
}

func (w *walker) shouldSkipDir(fi os.FileInfo) bool {
	for _, ignored := range w.ignoredDirs {
		if os.SameFile(ignored, fi) {
			return true
		}
	}
	return false
}

func (w *walker) shouldTraverse(path string, de fs.DirEntry) bool {
	// TODO: do we need to use filepath.EvalSymlinks() here?
	ts, err := StatDirEntry(path, de)
	if err != nil {
		return false
	}
	if !ts.IsDir() {
		return false
	}
	if w.shouldSkipDir(ts) {
		return false
	}
	for {
		parent := filepath.Dir(path)
		if parent == path {
			return true
		}
		parentInfo, err := os.Stat(parent)
		if err != nil {
			return false
		}
		if os.SameFile(ts, parentInfo) {
			return false
		}
		path = parent
	}
}

func (w *walker) joinPaths(dir, base string) string {
	// Handle the case where the root path argument to Walk is "/"
	// without this the returned path is prefixed with "//".
	if os.PathSeparator == '/' {
		if dir == "/" {
			return dir + base
		}
		return dir + "/" + base
	}
	if w.toSlash {
		return dir + "/" + base
	}
	return dir + string(os.PathSeparator) + base
}

func (w *walker) onDirEnt(dirName, baseName string, de fs.DirEntry) error {
	joined := w.joinPaths(dirName, baseName)
	typ := de.Type()
	if typ == os.ModeDir {
		w.enqueue(walkItem{dir: joined, info: de})
		return nil
	}

	err := w.fn(joined, de, nil)
	if typ == os.ModeSymlink {
		if err == ErrTraverseLink {
			if !w.follow {
				// Set callbackDone so we don't call it twice for both the
				// symlink-as-symlink and the symlink-as-directory later:
				w.enqueue(walkItem{dir: joined, info: de, callbackDone: true})
				return nil
			}
			err = nil // Ignore ErrTraverseLink when Follow is true.
		}
		if err == filepath.SkipDir {
			// Permit SkipDir on symlinks too.
			return nil
		}
		if err == nil && w.follow && w.shouldTraverse(joined, de) {
			// Traverse symlink
			w.enqueue(walkItem{dir: joined, info: de, callbackDone: true})
		}
	}
	return err
}

func (w *walker) walk(root string, info fs.DirEntry, runUserCallback bool) error {
	if runUserCallback {
		err := w.fn(root, info, nil)
		if err == filepath.SkipDir {
			return nil
		}
		if err != nil {
			return err
		}
	}

	err := readDir(root, w.onDirEnt)
	if err != nil {
		// Second call, to report ReadDir error.
		return w.fn(root, info, err)
	}
	return nil
}

func cleanRootPath(root string) string {
	for i := len(root) - 1; i >= 0; i-- {
		if !os.IsPathSeparator(root[i]) {
			return root[:i+1]
		}
	}
	if root != "" {
		return root[0:1] // root is all path separators ("//")
	}
	return root
}
