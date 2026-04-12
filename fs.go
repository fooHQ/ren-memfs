package mem

import (
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/foohq/ren"
)

var (
	ErrExist         = os.ErrExist
	ErrNotExist      = os.ErrNotExist
	ErrBadDescriptor = errors.New("bad descriptor")
	ErrIsDirectory   = errors.New("is a directory")
	ErrNotDirectory  = errors.New("is not a directory")
)

type node struct {
	name     string
	content  []byte
	mode     ren.FileMode
	modTime  time.Time
	children map[string]*node
	isDir    bool
	symlink  string
	mu       sync.RWMutex
}

var _ ren.FS = (*FS)(nil)

type FS struct {
	root *node
	mu   sync.RWMutex
}

// NewFS creates a new virtual filesystem
func NewFS() (*FS, error) {
	return &FS{
		root: &node{
			name:     "/",
			children: make(map[string]*node),
			isDir:    true,
			modTime:  time.Now(),
			mode:     0755,
		},
	}, nil
}

// Mkdir creates a new directory
func (fs *FS) Mkdir(name string, perm ren.FileMode) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.mkdirInternal(cleanPath(name), perm)
}

// MkdirAll creates a directory and all necessary parents
func (fs *FS) MkdirAll(pth string, perm ren.FileMode) error {
	return fs.mkdirAllInternal(cleanPath(pth), perm)
}

// MkdirTemp creates a new temporary directory in the directory dir
// and returns the pathname of the new directory.
func (fs *FS) MkdirTemp(dir, pattern string) (string, error) {
	if dir == "" {
		dir = cleanPath("tmp")
		err := fs.mkdirAllInternal(dir, 0755)
		if err != nil {
			return "", err
		}
	}

	prefix, suffix, ok := strings.Cut(pattern, "*")
	if !ok {
		prefix = pattern
		suffix = ""
	}

	for i := 0; i < 10000; i++ {
		name := path.Join(dir, prefix+randomString()+suffix)
		err := fs.mkdirInternal(cleanPath(name), 0755)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, ErrExist) {
			return "", err
		}
	}

	return "", errors.New("cannot create temporary directory")
}

// OpenFile opens a file with specified flags
func (fs *FS) OpenFile(name string, flag int, perm ren.FileMode) (ren.File, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	pth := cleanPath(name)
	parent, base, err := fs.getParent(pth)
	if err != nil {
		return nil, err
	}

	if parent == fs.root && base == "" {
		if flag&os.O_WRONLY != 0 || flag&os.O_RDWR != 0 {
			return nil, ErrIsDirectory
		}

		return &virtualFile{
			node: parent,
			flag: flag,
		}, nil
	}

	parent.mu.Lock()
	defer parent.mu.Unlock()

	if n, exists := parent.children[base]; exists {
		if n.isDir && (flag&os.O_WRONLY != 0 || flag&os.O_RDWR != 0) {
			return nil, ErrIsDirectory
		}
		return &virtualFile{
			node: n,
			flag: flag,
		}, nil
	}

	if flag&os.O_CREATE == 0 {
		return nil, ErrNotExist
	}

	n := &node{
		name:    base,
		content: []byte{},
		mode:    perm,
		modTime: time.Now(),
	}
	parent.children[base] = n
	return &virtualFile{
		node: n,
		flag: flag,
	}, nil
}

// ReadFile reads the entire contents of a file
func (fs *FS) ReadFile(name string) ([]byte, error) {
	f, err := fs.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	buf := make([]byte, info.Size())
	_, err = f.Read(buf)
	return buf, err
}

// Remove removes a file or empty directory
func (fs *FS) Remove(name string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	pth := cleanPath(name)
	parent, base, err := fs.getParent(pth)
	if err != nil {
		return err
	}

	parent.mu.Lock()
	defer parent.mu.Unlock()

	if n, exists := parent.children[base]; exists {
		if n.isDir && len(n.children) > 0 {
			return errors.New("directory not empty")
		}
		delete(parent.children, base)
		return nil
	}
	return ErrNotExist
}

// RemoveAll removes a path and all its children
func (fs *FS) RemoveAll(pth string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	cleaned := cleanPath(pth)
	if cleaned == "/" {
		fs.root.children = make(map[string]*node)
		return nil
	}

	parent, base, err := fs.getParent(cleaned)
	if err != nil {
		return err
	}

	parent.mu.Lock()
	defer parent.mu.Unlock()

	if _, exists := parent.children[base]; exists {
		delete(parent.children, base)
		return nil
	}
	return ErrNotExist
}

// Rename renames a file or directory
func (fs *FS) Rename(oldpath, newpath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	oldClean := cleanPath(oldpath)
	newClean := cleanPath(newpath)

	oldParent, oldBase, err := fs.getParent(oldClean)
	if err != nil {
		return err
	}

	newParent, newBase, err := fs.getParent(newClean)
	if err != nil {
		return err
	}

	oldParent.mu.Lock()
	defer oldParent.mu.Unlock()

	if oldParent != newParent {
		newParent.mu.Lock()
		defer newParent.mu.Unlock()
	}

	n, exists := oldParent.children[oldBase]
	if !exists {
		return ErrNotExist
	}

	if _, exists := newParent.children[newBase]; exists {
		return ErrExist
	}

	n.name = newBase
	delete(oldParent.children, oldBase)
	newParent.children[newBase] = n
	return nil
}

// Stat returns file information
func (fs *FS) Stat(name string) (ren.FileInfo, error) {
	n, err := fs.getNode(cleanPath(name))
	if err != nil {
		return nil, err
	}
	return &fileInfo{node: n}, nil
}

// Symlink creates a symbolic link
func (fs *FS) Symlink(oldname, newname string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	newClean := cleanPath(newname)
	parent, base, err := fs.getParent(newClean)
	if err != nil {
		return err
	}

	parent.mu.Lock()
	defer parent.mu.Unlock()

	if _, exists := parent.children[base]; exists {
		return ErrExist
	}

	parent.children[base] = &node{
		name:    base,
		symlink: oldname,
		mode:    0777,
		modTime: time.Now(),
	}
	return nil
}

// WriteFile writes data to a file
func (fs *FS) WriteFile(name string, data []byte, perm ren.FileMode) error {
	f, err := fs.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(data)
	return err
}

// ReadDir reads directory contents
func (fs *FS) ReadDir(name string) ([]ren.DirEntry, error) {
	n, err := fs.getNode(cleanPath(name))
	if err != nil {
		return nil, err
	}

	if !n.isDir {
		return nil, ErrNotDirectory
	}

	n.mu.RLock()
	defer n.mu.RUnlock()

	var entries []ren.DirEntry
	for _, child := range n.children {
		entries = append(entries, &dirEntry{node: child})
	}
	return entries, nil
}

type virtualFile struct {
	node   *node
	flag   int
	offset int64
}

func (f *virtualFile) Read(b []byte) (int, error) {
	if f.flag&os.O_WRONLY != 0 {
		return 0, ErrBadDescriptor
	}

	f.node.mu.RLock()
	defer f.node.mu.RUnlock()

	if f.offset >= int64(len(f.node.content)) {
		return 0, io.EOF
	}

	n := copy(b, f.node.content[f.offset:])
	f.offset += int64(n)
	return n, nil
}

func (f *virtualFile) Write(b []byte) (int, error) {
	if f.flag&os.O_WRONLY == 0 && f.flag&os.O_RDWR == 0 {
		return 0, ErrBadDescriptor
	}

	f.node.mu.Lock()
	defer f.node.mu.Unlock()

	f.node.content = append(f.node.content[:f.offset], b...)
	f.offset += int64(len(b))
	f.node.modTime = time.Now()
	return len(b), nil
}

func (f *virtualFile) Close() error {
	return nil
}

func (f *virtualFile) Stat() (ren.FileInfo, error) {
	return &fileInfo{
		node: f.node,
	}, nil
}

type fileInfo struct {
	node *node
}

func (fi *fileInfo) Name() string {
	fi.node.mu.RLock()
	defer fi.node.mu.RUnlock()
	return fi.node.name
}

func (fi *fileInfo) Size() int64 {
	fi.node.mu.RLock()
	defer fi.node.mu.RUnlock()
	return int64(len(fi.node.content))
}

func (fi *fileInfo) Mode() ren.FileMode {
	fi.node.mu.RLock()
	defer fi.node.mu.RUnlock()
	mode := fi.node.mode
	if fi.node.isDir {
		mode = os.ModeDir | mode
	}
	return mode
}

func (fi *fileInfo) ModTime() time.Time {
	fi.node.mu.RLock()
	defer fi.node.mu.RUnlock()
	return fi.node.modTime
}

func (fi *fileInfo) IsDir() bool {
	fi.node.mu.RLock()
	defer fi.node.mu.RUnlock()
	return fi.node.isDir
}

func (fi *fileInfo) Sys() any {
	return nil
}

type dirEntry struct {
	node *node
}

func (de *dirEntry) Name() string {
	de.node.mu.RLock()
	defer de.node.mu.RUnlock()
	return de.node.name
}

func (de *dirEntry) IsDir() bool {
	de.node.mu.RLock()
	defer de.node.mu.RUnlock()
	return de.node.isDir
}

func (de *dirEntry) Type() ren.FileMode {
	de.node.mu.RLock()
	defer de.node.mu.RUnlock()
	return de.node.mode
}

func (de *dirEntry) Info() (ren.FileInfo, error) {
	return &fileInfo{
		node: de.node,
	}, nil
}

func (de *dirEntry) HasInfo() bool {
	return true
}

// Helper methods
func (fs *FS) getNode(pth string) (*node, error) {
	if pth == "/" {
		return fs.root, nil
	}

	parts := strings.Split(pth[1:], "/")
	current := fs.root

	for _, part := range parts {
		current.mu.RLock()

		if !current.isDir {
			current.mu.RUnlock()
			return nil, errors.New("not a directory")
		}

		next, ok := current.children[part]
		if !ok {
			current.mu.RUnlock()
			return nil, ErrNotExist
		}

		current.mu.RUnlock()
		current = next
	}

	return current, nil
}

func (fs *FS) getParent(pth string) (*node, string, error) {
	dir, base := path.Split(pth)
	if dir == "/" {
		return fs.root, base, nil
	}

	n, err := fs.getNode(dir[:len(dir)-1])
	if err != nil {
		return nil, "", err
	}

	return n, base, nil
}

func (fs *FS) mkdirInternal(pth string, perm ren.FileMode) error {
	parent, base, err := fs.getParent(pth)
	if err != nil {
		return err
	}

	parent.mu.Lock()
	defer parent.mu.Unlock()

	if _, exists := parent.children[base]; exists {
		return ErrExist
	}

	parent.children[base] = &node{
		name:     base,
		children: make(map[string]*node),
		isDir:    true,
		mode:     perm,
		modTime:  time.Now(),
	}
	return nil
}

func (fs *FS) mkdirAllInternal(pth string, perm ren.FileMode) error {
	if pth == "/" {
		return nil
	}

	parts := strings.Split(pth[1:], "/")
	current := fs.root

	for _, part := range parts {
		current.mu.Lock()
		next, ok := current.children[part]
		if ok {
			current.mu.Unlock()
			next.mu.RLock()
			if !next.isDir {
				next.mu.RUnlock()
				return ErrNotDirectory
			}
			next.mu.RUnlock()
			current = next
		} else {
			newNode := &node{
				name:     part,
				children: make(map[string]*node),
				isDir:    true,
				mode:     perm,
				modTime:  time.Now(),
			}
			current.children[part] = newNode
			current.mu.Unlock()
			current = newNode
		}
	}
	return nil
}

func cleanPath(pth string) string {
	return path.Clean("/" + pth)
}

const tmpDirChars = "0123456789abcdefghijklmnopqrstuvwxyz"

func randomString() string {
	var buf [10]byte
	v := rand.Uint64()
	for i := range buf {
		buf[i] = tmpDirChars[v%36]
		v /= 36
	}
	return string(buf[:])
}
