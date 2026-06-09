package logrotate

import (
	"log"
)

// 使用标准库 log 时，在应用启动时把 Logger 传给 SetOutput 即可。
func Example() {
	log.SetOutput(&Logger{
		Filename:   "/var/log/myapp/foo.log",
		MaxSize:    500, // MB
		MaxBackups: 3,
		MaxAge:     28,   // 天
		Compress:   true, // 默认关闭
	})
}
