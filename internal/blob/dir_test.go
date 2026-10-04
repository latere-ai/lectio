// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package blob_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/lectio/internal/blob"
)

// What holds of the store over a directory alone: where its files are, what
// it leaves on disk, and what several processes on one root see of each
// other. A process is stood in for by a Dir of its own on the shared root:
// a Dir holds no lock and no state but the root, so two of them share exactly
// what two processes share, the file system.

// files returns every file under root, as slash-separated paths relative to
// it, in walk order.
func files(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// exists reports whether a path under root is there.
func exists(t *testing.T, root, rel string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("looking at %s: %v", rel, err)
	}
	return err == nil
}

// TestADirKeepsAnObjectAsTwoFiles: the bytes of an object are the file
// objects/<key> and its content type the file types/<key>, so a stored image
// is an image file, and nothing else is left on disk: no temporary file
// after a put, after a put that replaced an object, or after a put that
// failed.
func TestADirKeepsAnObjectAsTwoFiles(t *testing.T) {
	root := t.TempDir()
	s := openDir(t, root)
	png := []byte("\x89PNG\r\n\x1a\n and the rest of an image")
	put(t, s, "parses/prs_a/pages/1.1.png", png, "image/png")
	put(t, s, "parses/prs_a/pages/1.1.png", png, "image/png")
	put(t, s, "parses/prs_a/document.1.json", []byte("{}"), "")

	// A key that is the directory of other keys cannot be written, and the
	// attempt leaves nothing.
	if err := s.Put(t.Context(), "parses/prs_a/pages", []byte("x"), ""); err == nil {
		t.Fatal("a put over a directory of objects was accepted")
	}

	want := []string{
		"objects/parses/prs_a/document.1.json", "objects/parses/prs_a/pages/1.1.png",
		"types/parses/prs_a/document.1.json", "types/parses/prs_a/pages/1.1.png",
	}
	if got := files(t, root); !slices.Equal(got, want) {
		t.Fatalf("the files under the root are %q, want %q", got, want)
	}
	for path, want := range map[string]string{
		"objects/parses/prs_a/pages/1.1.png":   string(png),
		"types/parses/prs_a/pages/1.1.png":     "image/png",
		"types/parses/prs_a/document.1.json":   "application/octet-stream",
		"objects/parses/prs_a/document.1.json": "{}",
	} {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || string(got) != want {
			t.Errorf("the file %s holds %q, %v, want %q", path, got, err, want)
		}
	}
	info, err := os.Stat(filepath.Join(root, "objects", "parses", "prs_a", "pages", "1.1.png"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("an object's file has mode %v, %v, want 0600", info.Mode().Perm(), err)
	}
}

// TestADirListsNoTemporaryFile: a file that is still being written, or that
// a process left when it died, is not an object. It is never listed and
// never read, and a key whose last segment looks like one is still a key
// with an object of its own.
func TestADirListsNoTemporaryFile(t *testing.T) {
	root := t.TempDir()
	s := openDir(t, root)
	put(t, s, "parses/prs_a/pages/1.1.json", []byte("{}"), "application/json")
	// What a process leaves when it dies between creating its temporary file
	// and renaming it.
	left := filepath.Join(root, "objects", "parses", "prs_a", "pages", ".tmp-1234567890")
	if err := os.WriteFile(left, []byte(`{"half":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if keys := list(t, s, ""); !slices.Equal(keys, []string{"parses/prs_a/pages/1.1.json"}) {
		t.Fatalf("with a temporary file on disk the store lists %q", keys)
	}
	if _, _, err := s.Get(t.Context(), "parses/prs_a/pages/.tmp-1234567890"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a temporary file was read as an object: %v", err)
	}

	// Keys that start the way a temporary file's name does, and the way the
	// name such a key is stored under does.
	odd := []string{"parses/prs_a/pages/...tmp-c", "parses/prs_a/pages/..tmp-b", "parses/prs_a/pages/.tmp-1234567890", "parses/prs_a/pages/.tmp-a"}
	for _, key := range odd {
		put(t, s, key, []byte(key), "text/plain")
	}
	for _, key := range odd {
		if data, contentType := get(t, s, key); string(data) != key || contentType != "text/plain" {
			t.Errorf("the object %s read back as %q, %q", key, data, contentType)
		}
	}
	if keys, want := list(t, s, "parses/prs_a/pages/"), append(slices.Clone(odd), "parses/prs_a/pages/1.1.json"); !slices.Equal(keys, want) {
		t.Fatalf("with keys that look like temporary files the store lists %q, want %q", keys, want)
	}
	if got, err := os.ReadFile(left); err != nil || string(got) != `{"half":` {
		t.Fatalf("a put wrote over another process's temporary file: %q, %v", got, err)
	}
	for _, key := range odd {
		if err := s.Delete(t.Context(), key); err != nil {
			t.Fatalf("Delete(%q): %v", key, err)
		}
	}
	if keys := list(t, s, ""); !slices.Equal(keys, []string{"parses/prs_a/pages/1.1.json"}) {
		t.Fatalf("after the deletes the store lists %q", keys)
	}
}

// TestADirDeletePrunesEmptyDirectories: a delete removes the directories
// that held only the deleted object, in both trees, up to the tree's own
// root, and leaves every directory that still holds an object.
func TestADirDeletePrunesEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	s := openDir(t, root)
	put(t, s, "parses/prs_a/pages/1.1.json", []byte("{}"), "application/json")
	put(t, s, "parses/prs_a/document.1.json", []byte("{}"), "application/json")
	put(t, s, "parses/prs_b/pages/1.1.json", []byte("{}"), "application/json")

	if err := s.Delete(t.Context(), "parses/prs_a/pages/1.1.json"); err != nil {
		t.Fatal(err)
	}
	for _, tree := range []string{"objects", "types"} {
		if exists(t, root, tree+"/parses/prs_a/pages") || !exists(t, root, tree+"/parses/prs_a/document.1.json") {
			t.Fatalf("after one delete the tree %s kept an empty directory or lost a full one: %q", tree, files(t, root))
		}
	}

	for _, key := range []string{"parses/prs_a/document.1.json", "parses/prs_b/pages/1.1.json"} {
		if err := s.Delete(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}
	for _, tree := range []string{"objects", "types"} {
		if exists(t, root, tree+"/parses") || !exists(t, root, tree) {
			t.Fatalf("after the last delete the tree %s is not an empty directory", tree)
		}
	}
	// A delete under directories that were never made climbs to the tree's
	// root and removes nothing.
	if err := s.Delete(t.Context(), "parses/prs_never/pages/1.1.json"); err != nil {
		t.Fatal(err)
	}
	put(t, s, "parses/prs_a/pages/1.2.json", []byte("{}"), "application/json")
	if keys := list(t, s, ""); !slices.Equal(keys, []string{"parses/prs_a/pages/1.2.json"}) {
		t.Fatalf("a put after the prune lists %q", keys)
	}
}

// TestASecondDirSeesWhatTheFirstWrote: the store is the directory. A Dir
// opened later on the same root, as a second process opens it, reads, lists
// and deletes what the first wrote, and the first sees what the second did.
func TestASecondDirSeesWhatTheFirstWrote(t *testing.T) {
	root := filepath.Join(t.TempDir(), "made", "on", "open")
	first := openDir(t, root)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the root that NewDir made is %v, %v", info, err)
	}
	put(t, first, "parses/prs_a/pages/1.1.json", []byte(`{"number":1}`), "application/json")

	second := openDir(t, root)
	if data, contentType := get(t, second, "parses/prs_a/pages/1.1.json"); string(data) != `{"number":1}` || contentType != "application/json" {
		t.Fatalf("the second store read %q, %q", data, contentType)
	}
	put(t, second, "parses/prs_a/pages/2.1.json", []byte(`{"number":2}`), "application/json")
	if err := second.Delete(t.Context(), "parses/prs_a/pages/1.1.json"); err != nil {
		t.Fatal(err)
	}
	if keys := list(t, first, ""); !slices.Equal(keys, []string{"parses/prs_a/pages/2.1.json"}) {
		t.Fatalf("the first store lists %q after the second wrote and deleted", keys)
	}
}

// TestADirNeverServesATornObject: writers in several processes put one key
// over and over while readers get it. Every read returns one of the written
// values whole, never the start of one, a mix of two, or nothing: a file is
// renamed into place, so no reader sees it before it is complete.
func TestADirNeverServesATornObject(t *testing.T) {
	root := t.TempDir()
	const (
		key     = "parses/prs_a/pages/1.1.json"
		writers = 4
		readers = 4
		rounds  = 200
	)
	// Each writer puts a value of its own length filled with its own byte,
	// so a torn object shows as a wrong length or as two bytes in one value.
	value := func(w int) []byte { return bytes.Repeat([]byte{byte('a' + w)}, 16<<10+w*1000) }
	put(t, openDir(t, root), key, value(0), "application/json")

	var wg sync.WaitGroup
	done := make(chan struct{})
	errs := make(chan error, writers+readers)
	for w := range writers {
		s := openDir(t, root)
		wg.Go(func() {
			for range rounds {
				if err := s.Put(t.Context(), key, value(w), "application/json"); err != nil {
					errs <- fmt.Errorf("writer %d: %w", w, err)
					return
				}
			}
		})
	}
	var reading sync.WaitGroup
	for r := range readers {
		s := openDir(t, root)
		reading.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				data, contentType, err := s.Get(t.Context(), key)
				if err != nil {
					errs <- fmt.Errorf("reader %d: %w", r, err)
					return
				}
				whole := false
				if len(data) > 0 {
					w := int(data[0]) - 'a'
					whole = w >= 0 && w < writers && bytes.Equal(data, value(w))
				}
				if !whole || contentType != "application/json" {
					errs <- fmt.Errorf("reader %d read a torn object: %d bytes of %q that start with %q", r, len(data), contentType, data[:min(len(data), 8)])
					return
				}
			}
		})
	}
	wg.Wait()
	close(done)
	reading.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := files(t, root); len(got) != 2 {
		t.Fatalf("the writers left %q on disk", got)
	}
}

// TestADirPutOutlastsAPruneOfItsDirectory: several processes put and delete
// keys of their own in one directory. Every delete that leaves the directory
// empty prunes it, in the moment a put of another process is about to write
// into it. The put makes the directory again: no call fails, and an object
// that was put is there until its own process deletes it.
func TestADirPutOutlastsAPruneOfItsDirectory(t *testing.T) {
	root := t.TempDir()
	const (
		processes = 4
		rounds    = 400
	)
	var wg sync.WaitGroup
	errs := make(chan error, processes+1)
	for p := range processes {
		s := openDir(t, root)
		key := fmt.Sprintf("parses/prs_a/pages/%d.1.json", p)
		wg.Go(func() {
			for round := range rounds {
				if err := s.Put(t.Context(), key, []byte(key), "application/json"); err != nil {
					errs <- fmt.Errorf("round %d, put: %w", round, err)
					return
				}
				data, contentType, err := s.Get(t.Context(), key)
				if err != nil {
					errs <- fmt.Errorf("round %d, get of the object that was just put: %w", round, err)
					return
				}
				if string(data) != key || contentType != "application/json" {
					errs <- fmt.Errorf("round %d, the object that was just put read as %q, %q", round, data, contentType)
					return
				}
				if err := s.Delete(t.Context(), key); err != nil {
					errs <- fmt.Errorf("round %d, delete: %w", round, err)
					return
				}
			}
		})
	}
	// One more process lists the directory while it comes and goes. A
	// listing never fails for a directory that left under its walk, and
	// holds nothing but keys that were put.
	done := make(chan struct{})
	var listing sync.WaitGroup
	listing.Go(func() {
		s := openDir(t, root)
		for {
			select {
			case <-done:
				return
			default:
			}
			keys, err := s.List(t.Context(), "parses/")
			if err != nil {
				errs <- fmt.Errorf("a listing beside the puts and deletes: %w", err)
				return
			}
			if len(keys) > processes || slices.ContainsFunc(keys, func(key string) bool {
				return !strings.HasPrefix(key, "parses/prs_a/pages/") || !strings.HasSuffix(key, ".1.json")
			}) {
				errs <- fmt.Errorf("a listing beside the puts and deletes returned %q", keys)
				return
			}
		}
	})
	wg.Wait()
	close(done)
	listing.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := files(t, root); len(got) != 0 {
		t.Fatalf("after every object was deleted the root holds %q", got)
	}
}

// TestADirRootMustBeADirectory: a root that is a file, a root under a file
// and a root with no name are refused, and nothing is made for them.
func TestADirRootMustBeADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{file, filepath.Join(file, "below"), ""} {
		if store, err := blob.NewDir(root); err == nil {
			t.Errorf("NewDir(%q) = %v, want an error", root, store)
		}
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "not a directory" {
		t.Fatalf("the file a store was refused on is now %q, %v", got, err)
	}
}

// TestADirObjectWithNoTypeFileIsAnOctetStream: the bytes are renamed into
// place before the content type is, so a reader in another process may find
// the first without the second, and a process may die between the two. The
// object is there, with the content type of an object put with none.
func TestADirObjectWithNoTypeFileIsAnOctetStream(t *testing.T) {
	root := t.TempDir()
	s := openDir(t, root)
	put(t, s, "parses/prs_a/pages/1.1.json", []byte("{}"), "application/json")
	if err := os.Remove(filepath.Join(root, "types", "parses", "prs_a", "pages", "1.1.json")); err != nil {
		t.Fatal(err)
	}
	if data, contentType := get(t, s, "parses/prs_a/pages/1.1.json"); string(data) != "{}" || contentType != "application/octet-stream" {
		t.Fatalf("an object with no type file read as %q, %q", data, contentType)
	}
	// The whole types tree may be gone and the objects are still objects.
	if err := os.RemoveAll(filepath.Join(root, "types")); err != nil {
		t.Fatal(err)
	}
	if data, contentType := get(t, s, "parses/prs_a/pages/1.1.json"); string(data) != "{}" || contentType != "application/octet-stream" {
		t.Fatalf("an object with no types tree read as %q, %q", data, contentType)
	}
	if err := s.Delete(t.Context(), "parses/prs_a/pages/1.1.json"); err != nil {
		t.Fatalf("deleting an object with no type file: %v", err)
	}
}

// TestADirectoryIsNotAnObject: a file system cannot hold a key and the keys
// under it. The key that is a directory holds no object: it is not found,
// its delete removes nothing, and its put fails and leaves no file. The same
// holds for a key under a key that is an object. Neither call touches the
// objects that are there.
func TestADirectoryIsNotAnObject(t *testing.T) {
	root := t.TempDir()
	s := openDir(t, root)
	put(t, s, "parses/prs_a/pages/1.1.json", []byte(`{"number":1}`), "application/json")

	for _, key := range []string{"parses/prs_a/pages", "parses", "parses/prs_a/pages/1.1.json/below", "parses/prs_a/pages/1.1.json/far/below"} {
		if _, _, err := s.Get(t.Context(), key); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Get(%q) = %v, want not found", key, err)
		}
		if err := s.Delete(t.Context(), key); err != nil {
			t.Errorf("Delete(%q) = %v", key, err)
		}
		if err := s.Put(t.Context(), key, []byte("x"), "text/plain"); err == nil {
			t.Errorf("Put(%q) was accepted", key)
		}
	}
	if keys := list(t, s, "parses/prs_a/pages/1.1.json/far/"); len(keys) != 0 {
		t.Fatalf("a listing under an object returned %q", keys)
	}
	if data, contentType := get(t, s, "parses/prs_a/pages/1.1.json"); string(data) != `{"number":1}` || contentType != "application/json" {
		t.Fatalf("the object that was there is now %q, %q", data, contentType)
	}
	want := []string{"objects/parses/prs_a/pages/1.1.json", "types/parses/prs_a/pages/1.1.json"}
	if got := files(t, root); !slices.Equal(got, want) {
		t.Fatalf("the refused calls left %q on disk, want %q", got, want)
	}
}

// TestADirListsOnlyFiles: what is under the root and is not a file the store
// wrote is not a key. A link is not followed, so no listing leaves the root.
func TestADirListsOnlyFiles(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	s := openDir(t, root)
	put(t, s, "parses/prs_a/pages/1.1.json", []byte("{}"), "application/json")
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside the store"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "objects", "parses", "link")); err != nil {
		t.Skipf("this file system makes no link: %v", err)
	}
	if keys := list(t, s, ""); !slices.Equal(keys, []string{"parses/prs_a/pages/1.1.json"}) {
		t.Fatalf("with a link under the root the store lists %q", keys)
	}
}

// TestADirReportsWhatTheFileSystemRefuses: a failure of the file system is
// an error of the call and is never read as an absent object. A put that
// fails leaves no temporary file.
func TestADirReportsWhatTheFileSystemRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the superuser is refused nothing, so there is no failure to report")
	}
	// lock takes a mode's bits off a path until the case ends.
	lock := func(t *testing.T, path string, mode fs.FileMode) {
		t.Helper()
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(path, 0o700); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("giving %s its mode back: %v", path, err)
			}
		})
	}
	const key = "parses/prs_a/pages/1.1.json"
	refused := func(t *testing.T, call string, err error) {
		t.Helper()
		if err == nil || errors.Is(err, blob.ErrNotFound) || !errors.Is(err, fs.ErrPermission) || !strings.HasPrefix(err.Error(), "blob: ") {
			t.Fatalf("%s = %v, want the file system's refusal under the package's name", call, err)
		}
	}

	t.Run("a put into a directory that takes no file", func(t *testing.T) {
		root := t.TempDir()
		s := openDir(t, root)
		put(t, s, key, []byte("{}"), "application/json")
		lock(t, filepath.Join(root, "objects", "parses", "prs_a", "pages"), 0o500)
		refused(t, "Put", s.Put(t.Context(), "parses/prs_a/pages/2.1.json", []byte("{}"), ""))
		if got := files(t, root); len(got) != 2 {
			t.Fatalf("the refused put left %q", got)
		}
	})
	t.Run("a put whose content type cannot be written", func(t *testing.T) {
		root := t.TempDir()
		s := openDir(t, root)
		lock(t, filepath.Join(root, "types"), 0o500)
		refused(t, "Put", s.Put(t.Context(), key, []byte("{}"), "application/json"))
	})
	t.Run("a get of a file that may not be read", func(t *testing.T) {
		root := t.TempDir()
		s := openDir(t, root)
		put(t, s, key, []byte("{}"), "application/json")
		lock(t, filepath.Join(root, "types", "parses", "prs_a", "pages", "1.1.json"), 0o000)
		_, _, err := s.Get(t.Context(), key)
		refused(t, "Get with an unreadable content type", err)
		lock(t, filepath.Join(root, "objects", "parses", "prs_a", "pages", "1.1.json"), 0o000)
		_, _, err = s.Get(t.Context(), key)
		refused(t, "Get with unreadable bytes", err)
	})
	t.Run("a delete from a directory that gives up no file", func(t *testing.T) {
		root := t.TempDir()
		s := openDir(t, root)
		put(t, s, key, []byte("{}"), "application/json")
		lock(t, filepath.Join(root, "objects", "parses", "prs_a", "pages"), 0o500)
		refused(t, "Delete", s.Delete(t.Context(), key))
	})
	t.Run("a delete whose empty directory cannot be removed", func(t *testing.T) {
		root := t.TempDir()
		s := openDir(t, root)
		put(t, s, key, []byte("{}"), "application/json")
		lock(t, filepath.Join(root, "objects", "parses", "prs_a"), 0o500)
		refused(t, "Delete", s.Delete(t.Context(), key))
	})
	t.Run("a listing through a directory that may not be read", func(t *testing.T) {
		root := t.TempDir()
		s := openDir(t, root)
		put(t, s, key, []byte("{}"), "application/json")
		lock(t, filepath.Join(root, "objects", "parses", "prs_a"), 0o000)
		_, err := s.List(t.Context(), "")
		refused(t, "List", err)
	})
}
