package logrotate

import "fmt"

// New 可以用 Functional Options 创建 Logger。
func Example() {
	l := New(
		WithFilename("/var/log/myapp/foo.log"),
		WithMaxSize(500),
		WithMaxBackups(3),
		WithMaxAge(28),
		WithCompress(true),
	)

	fmt.Println(l.Filename, l.MaxSize, l.MaxBackups, l.MaxAge, l.Compress)
	// Output: /var/log/myapp/foo.log 500 3 28 true
}
