// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const (
	// objectsTree and typesTree are the two directories under a Dir's root.
	// They mirror each other: the file of a key in the first holds the
	// object's bytes, and the file of the same key in the second its content
	// type.
	objectsTree = "objects"
	typesTree   = "types"

	// tempMark follows the one dot that starts the name of a file that is
	// still being written.
	tempMark = "tmp-"

	// dirMode is the mode of every directory the store makes: the objects
	// are their owner's alone.
	dirMode = 0o700

	// writeAttempts bounds how often Put writes a file again after a Delete
	// pruned its directory away. Each loss needs a prune to land between two
	// system calls, so a handful is more than a run ever takes.
	writeAttempts = 8
)

// Dir is a Store over a directory of files that several processes on one
// machine may open at once: the API and the workers of a development setup,
// and the suites that start them. The bytes of an object are the file
// objects/<key> under the root and its content type is the file types/<key>,
// so a stored image is an image file a person can open, and a listing walks
// one tree.
//
// A file is written under a temporary name beside its place and renamed into
// it, so a reader in any process finds a file whole or not at all. The bytes
// land before the type: between the two renames an object reads with the
// content type it had before, and with DefaultContentType when it had none.
// Nothing is synced to disk, so the store survives a process that dies and
// not a machine that does. A process that dies while it writes leaves its
// temporary file behind, which no listing shows.
//
// A file system is not a flat key space, and three things follow. Put fails
// for a key with a segment longer than a file name may be. Put fails for a
// key that is also the directory of another, as a and a/b are. And where the
// file system folds case, two keys that differ only by case are one object.
type Dir struct {
	root string
}

// NewDir opens the store under root, making the directory when it is
// missing. A second Dir on the same root, in this process or another, sees
// the same objects.
func NewDir(root string) (*Dir, error) {
	if root == "" {
		return nil, errors.New("blob: the store's directory is not named")
	}
	for _, tree := range []string{objectsTree, typesTree} {
		if err := os.MkdirAll(filepath.Join(root, tree), dirMode); err != nil {
			return nil, fmt.Errorf("blob: making the store's directory: %w", err)
		}
	}
	return &Dir{root: root}, nil
}

// Put writes the bytes and then the content type, each through a rename.
func (d *Dir) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := write(d.path(objectsTree, key), data); err != nil {
		return fmt.Errorf("blob: writing an object: %w", err)
	}
	if err := write(d.path(typesTree, key), []byte(cmp.Or(contentType, DefaultContentType))); err != nil {
		return fmt.Errorf("blob: writing an object's content type: %w", err)
	}
	return nil
}

// Get reads the bytes and then the content type.
func (d *Dir) Get(ctx context.Context, key string) ([]byte, string, error) {
	if err := ValidKey(key); err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(d.path(objectsTree, key))
	switch {
	case absent(err):
		return nil, "", ErrNotFound
	case err != nil:
		return nil, "", fmt.Errorf("blob: reading an object: %w", err)
	}
	contentType, err := os.ReadFile(d.path(typesTree, key))
	switch {
	case absent(err):
		// The bytes are there and the type is not: a Put is between its two
		// renames, or a process died there.
		return data, DefaultContentType, nil
	case err != nil:
		return nil, "", fmt.Errorf("blob: reading an object's content type: %w", err)
	}
	return data, string(contentType), nil
}

// Delete removes both files of the key and the directories that held only
// them. The bytes go first: without them the key holds no object, whatever
// is left of its type.
func (d *Dir) Delete(ctx context.Context, key string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, tree := range []string{objectsTree, typesTree} {
		file := d.path(tree, key)
		// A path that is a directory with files in it is the directory of
		// other keys and not an object, so it is left as it is.
		if err := os.Remove(file); err != nil && !absent(err) && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("blob: removing an object: %w", err)
		}
		if err := prune(filepath.Join(d.root, tree), filepath.Dir(file)); err != nil {
			return fmt.Errorf("blob: removing an empty directory: %w", err)
		}
	}
	return nil
}

// List walks the objects tree. A file system orders a directory's names and
// not whole keys, so the keys are sorted after the walk.
func (d *Dir) List(ctx context.Context, prefix string) ([]string, error) {
	if err := validPrefix(prefix); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tree := filepath.Join(d.root, objectsTree)
	// Every key that starts with the prefix lies under the directory the
	// prefix names up to its last slash, so the walk starts there and not at
	// the root of the tree.
	under, _ := path.Split(prefix)
	var keys []string
	err := filepath.WalkDir(filepath.Join(tree, filepath.FromSlash(under)), func(file string, entry fs.DirEntry, err error) error {
		switch {
		case absent(err):
			// The directory is not there, or a Delete removed it under the walk.
			return nil
		case err != nil:
			return err
		case !entry.Type().IsRegular():
			return nil
		}
		dir, name := filepath.Split(strings.TrimPrefix(strings.TrimPrefix(file, tree), string(filepath.Separator)))
		switch dots := tempDots(name); {
		case dots == 1:
			return nil // a file that is still being written
		case dots > 1:
			name = name[1:]
		}
		if key := filepath.ToSlash(dir) + name; strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("blob: listing objects: %w", err)
	}
	slices.Sort(keys)
	return keys, nil
}

// path is the file of a key in one of the two trees. The last segment of a
// key is the file's name, except for a segment that would read as a
// temporary file or as the name such a segment is stored under: that one is
// stored under one more dot, and List takes the dot off again. So every key
// has a file of its own, and a temporary file is never taken for an object.
func (d *Dir) path(tree, key string) string {
	dir, name := path.Split(key)
	if tempDots(name) > 0 {
		name = "." + name
	}
	return filepath.Join(d.root, tree, filepath.FromSlash(dir), name)
}

// tempDots counts the dots a name starts with when tempMark follows them,
// and is 0 for every other name. A file with 1 is a temporary file, and a
// file with more is the file of a key whose last segment has one fewer.
func tempDots(name string) int {
	rest := strings.TrimLeft(name, ".")
	if strings.HasPrefix(rest, tempMark) {
		return len(name) - len(rest)
	}
	return 0
}

// write puts data at file through a temporary file beside it. The rename is
// what makes the file appear, whole, to every process at once.
//
// A Delete in any process may prune the directory, or one above it, while
// the file is written, and the write then starts over. A prune removes only
// an empty directory, so on most systems it can win only before the
// temporary file exists. Darwin lets a file be created in a directory that
// is being removed at that moment: the file is then lost with the directory,
// and the rename finds nothing to move.
func write(file string, data []byte) error {
	var err error
	for range writeAttempts {
		if err = writeOnce(file, data); !pruned(err) {
			break
		}
	}
	return err
}

// writeOnce makes the directory, writes the temporary file and renames it.
// The temporary file is removed when a step after its creation fails.
func writeOnce(file string, data []byte) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+tempMark+"*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err = errors.Join(err, tmp.Close()); err == nil {
		err = os.Rename(tmp.Name(), file)
	}
	if err == nil {
		return nil
	}
	// A temporary file that left with its directory needs no removal.
	if rm := os.Remove(tmp.Name()); rm != nil && !errors.Is(rm, fs.ErrNotExist) {
		err = errors.Join(err, rm)
	}
	return err
}

// pruned reports whether a step of a write failed the way it does when a
// prune took a directory from under it. The directory or the temporary file
// is gone; or another Put made the directory first, which the step was told,
// and it is gone again; or the system calls the argument invalid, which is
// what Darwin answers for a new entry in a directory that is being removed.
func pruned(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.EINVAL)
}

// prune removes dir and the directories above it for as long as they are
// empty, and stops below tree, which stays. It removes directories only: a
// name that another process has since given to an object is left alone. A
// directory that still holds a file ends the climb, and one that another
// prune removed first does not.
func prune(tree, dir string) error {
	for len(dir) > len(tree) {
		err := syscall.Rmdir(dir)
		switch {
		case errors.Is(err, fs.ErrExist), errors.Is(err, syscall.ENOTDIR):
			return nil
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return &fs.PathError{Op: "rmdir", Path: dir, Err: err}
		}
		dir = filepath.Dir(dir)
	}
	return nil
}

// absent reports whether a read or a walk failed because no file is at the
// path: nothing is there, the path is a directory, or a segment above it is
// a file.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENOTDIR)
}
