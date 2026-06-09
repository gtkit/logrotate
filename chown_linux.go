package logrotate

import (
	"os"
	"syscall"
)

// osChown is a var so we can mock it out during tests.
var osChown = os.Chown

type chownFile interface {
	Close() error
}

// osOpenFile is a var so we can mock it out during tests.
var osOpenFile = func(name string, flag int, perm os.FileMode) (chownFile, error) {
	return os.OpenFile(name, flag, perm)
}

func chown(name string, info os.FileInfo) error {
	f, err := osOpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	stat := info.Sys().(*syscall.Stat_t)
	return osChown(name, int(stat.Uid), int(stat.Gid))
}
