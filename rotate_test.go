//go:build linux

package logrotate

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

// 响应 SIGHUP 时可以主动轮转日志文件。
func ExampleLogger_Rotate() {
	l := &Logger{}
	log.SetOutput(l)
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP)

	go func() {
		for {
			<-c
			if err := l.Rotate(); err != nil {
				log.Printf("rotate log: %v", err)
			}
		}
	}()
}
