package vfs

import (
	"errors"
	"github.com/dchest/safefile"
	"go4.org/lock"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

type osFS struct {
	root string
	tmp  bool
}

var (
	ErrNotDirectory = errors.New("not a directory")
)

func OpenDir(dir string, create bool) (FileSystem, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) && create {
			err = os.MkdirAll(dir, 0755)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	} else if !info.IsDir() {
		return nil, ErrNotDirectory
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &osFS{root: dir}, nil
}

func CreateTempDir() (FileSystem, error) {
	dir, err := ioutil.TempDir("", "tmpdir")
	if err != nil {
		return nil, err
	}
	return &osFS{root: dir, tmp: true}, nil
}

func (fs *osFS) Path() string {
	return fs.root
}

func (fs *osFS) String() string {
	return fs.Path()
}

func (fs *osFS) Close() error {
	if fs.tmp && strings.HasPrefix(filepath.Base(fs.root), "tmpdir") {
		return os.RemoveAll(fs.root)
	}
	return nil
}

func (fs *osFS) Prefix(name string) string {
	return filepath.Clean(filepath.Join(fs.root, filepath.Clean(name)))
}

func (fs *osFS) Lock(name string) (io.Closer, error) {
	closer, err := lock.Lock(fs.Prefix(name))
	if err != nil {
		if strings.Contains(err.Error(), "already locked") {
			err = ErrLocked
		}
		err = &os.PathError{Op: "lock", Path: name, Err: err}
	}
	return closer, err
}

func (fs *osFS) ReadDir() ([]os.FileInfo, error) {
	return ioutil.ReadDir(fs.root)
}

func (fs *osFS) Rename(oldname, newname string) error {
	return os.Rename(fs.Prefix(oldname), fs.Prefix(newname))
}

func (fs *osFS) Remove(name string) error {
	return os.Remove(fs.Prefix(name))
}

func (fs *osFS) OpenFile(name string) (InputFile, error) {
	return os.Open(fs.Prefix(name))
}

func (fs *osFS) CreateFile(name string, overwrite bool) (OutputFile, error) {
	flags := os.O_RDWR | os.O_CREATE
	if overwrite {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	return os.OpenFile(fs.Prefix(name), flags, 0666)
}

func (fs *osFS) CreateAtomicFile(name string) (AtomicOutputFile, error) {
	return safefile.Create(fs.Prefix(name), 0666)
}