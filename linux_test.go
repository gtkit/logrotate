//go:build linux
// +build linux

package logrotate

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestMaintainMode(t *testing.T) {
	dir := makeTempDir("TestMaintainMode", t)
	defer removeAll(dir)

	filename := logFile(dir)

	mode := os.FileMode(0600)
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, mode)
	isNil(err, t)
	f.Close()

	l := &Logger{
		Filename:   filename,
		MaxBackups: 1,
		MaxSize:    100, // megabytes
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	newFakeTime()

	err = l.Rotate()
	isNil(err, t)

	filename2 := backupFile(dir)
	info, err := os.Stat(filename)
	isNil(err, t)
	info2, err := os.Stat(filename2)
	isNil(err, t)
	equals(mode, info.Mode(), t)
	equals(mode, info2.Mode(), t)
}

func TestMaintainOwner(t *testing.T) {
	fakeFS := newFakeFS()
	osChown = fakeFS.Chown
	osStat = fakeFS.Stat
	defer func() {
		osChown = os.Chown
		osStat = os.Stat
	}()
	dir := makeTempDir("TestMaintainOwner", t)
	defer removeAll(dir)

	filename := logFile(dir)

	f, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0o644)
	isNil(err, t)
	f.Close()

	l := &Logger{
		Filename:   filename,
		MaxBackups: 1,
		MaxSize:    100, // megabytes
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	newFakeTime()

	err = l.Rotate()
	isNil(err, t)

	equals(555, fakeFS.files[filename].uid, t)
	equals(666, fakeFS.files[filename].gid, t)
}

func TestCompressMaintainMode(t *testing.T) {

	dir := makeTempDir("TestCompressMaintainMode", t)
	defer removeAll(dir)

	filename := logFile(dir)

	mode := os.FileMode(0600)
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, mode)
	isNil(err, t)
	f.Close()

	l := &Logger{
		Compress:   true,
		Filename:   filename,
		MaxBackups: 1,
		MaxSize:    100, // megabytes
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	newFakeTime()

	err = l.Rotate()
	isNil(err, t)

	// we need to wait a little bit since the files get compressed on a different
	// goroutine.
	<-time.After(10 * time.Millisecond)

	// a compressed version of the log file should now exist with the correct
	// mode.
	filename2 := backupFile(dir)
	info, err := os.Stat(filename)
	isNil(err, t)
	info2, err := os.Stat(filename2 + compressSuffix)
	isNil(err, t)
	equals(mode, info.Mode(), t)
	equals(mode, info2.Mode(), t)
}

func TestCompressMaintainOwner(t *testing.T) {
	fakeFS := newFakeFS()
	osChown = fakeFS.Chown
	osStat = fakeFS.Stat
	defer func() {
		osChown = os.Chown
		osStat = os.Stat
	}()
	dir := makeTempDir("TestCompressMaintainOwner", t)
	defer removeAll(dir)

	filename := logFile(dir)

	f, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0o644)
	isNil(err, t)
	f.Close()

	l := &Logger{
		Compress:   true,
		Filename:   filename,
		MaxBackups: 1,
		MaxSize:    100, // megabytes
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	newFakeTime()

	err = l.Rotate()
	isNil(err, t)

	// we need to wait a little bit since the files get compressed on a different
	// goroutine.
	<-time.After(10 * time.Millisecond)

	// a compressed version of the log file should now exist with the correct
	// owner.
	filename2 := backupFile(dir)
	equals(555, fakeFS.files[filename2+compressSuffix].uid, t)
	equals(666, fakeFS.files[filename2+compressSuffix].gid, t)
}

func TestChownReturnsCloseError(t *testing.T) {
	tests := []struct {
		name       string
		closeErr   error
		wantErr    error
		wantChown  bool
		wantOpened bool
	}{
		{
			name:       "close error",
			closeErr:   errors.New("close failed"),
			wantErr:    errors.New("close failed"),
			wantOpened: true,
		},
	}

	oldOpenFile := osOpenFile
	oldChown := osChown
	defer func() {
		osOpenFile = oldOpenFile
		osChown = oldChown
	}()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opened := false
			chowned := false
			osOpenFile = func(string, int, os.FileMode) (chownFile, error) {
				opened = true
				return closeErrorFile{err: tt.closeErr}, nil
			}
			osChown = func(string, int, int) error {
				chowned = true
				return nil
			}

			err := chown("ignored.log", fakeStatFileInfo{})
			equals(tt.wantErr.Error(), err.Error(), t)
			equals(tt.wantOpened, opened, t)
			equals(tt.wantChown, chowned, t)
		})
	}
}

type fakeFile struct {
	uid int
	gid int
}

type closeErrorFile struct {
	err error
}

func (f closeErrorFile) Close() error {
	return f.err
}

type fakeStatFileInfo struct{}

func (fakeStatFileInfo) Name() string {
	return "ignored.log"
}

func (fakeStatFileInfo) Size() int64 {
	return 0
}

func (fakeStatFileInfo) Mode() os.FileMode {
	return 0o644
}

func (fakeStatFileInfo) ModTime() time.Time {
	return time.Time{}
}

func (fakeStatFileInfo) IsDir() bool {
	return false
}

func (fakeStatFileInfo) Sys() any {
	return &syscall.Stat_t{}
}

type fakeFS struct {
	files map[string]fakeFile
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: make(map[string]fakeFile)}
}

func (fs *fakeFS) Chown(name string, uid, gid int) error {
	fs.files[name] = fakeFile{uid: uid, gid: gid}
	return nil
}

func (fs *fakeFS) Stat(name string) (os.FileInfo, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	stat := info.Sys().(*syscall.Stat_t)
	stat.Uid = 555
	stat.Gid = 666
	return info, nil
}
