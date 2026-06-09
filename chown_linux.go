package logrotate

import (
	"os"
	"syscall"
)

// osChown 用于测试中替换 os.Chown。
var osChown = os.Chown

type chownFile interface {
	Close() error
}

// osOpenFile 用于测试中替换 os.OpenFile。
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
	// 用 comma-ok 断言：非常规文件系统下 Sys() 可能不是 *syscall.Stat_t，
	// 此时无法获知原属主，跳过 chown 而不是 panic。
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return osChown(name, int(stat.Uid), int(stat.Gid))
}
