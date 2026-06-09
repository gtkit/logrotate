# logrotate

`logrotate` 是一个 Go 日志文件切割包，实现 `io.WriteCloser`，可以作为标准库 `log`、`slog`、`zap`、`logrus` 等日志库的文件输出端。

它只负责日志文件写入、切割、压缩和清理，不负责日志格式化、日志级别、字段编码或采样。

支持的切割方式：

- 按文件大小切割：通过 `MaxSize` 控制单个日志文件最大 MB 数。
- 按天切割，活跃文件名不变：通过 `Daily` 跨天轮转，当前写入文件仍是固定 `Filename`。
- 按天切割，活跃文件名带日期：通过 `DailyFilename` 写入 `app-2026-06-09.log` 这类日期文件。
- 大小和按天组合切割：`MaxSize` 可以和 `Daily` / `DailyFilename` 同时生效。
- 手动切割：通过 `Rotate()` 主动轮转，适合配合 `SIGHUP`。
- 显式启动：通过 `Open()` 初始化文件并同步清理历史积压，通过 `Cleanup()` 单独触发清理。
- 旧日志清理：通过 `MaxBackups` 限制旧文件数量，通过 `MaxAge` 限制保留天数。
- 旧日志压缩：通过 `Compress` 将轮转后的旧日志压缩为 `.gz`。

## 安装

```bash
go get github.com/gtkit/logrotate
```

## 快速上手

### 配合标准库 `log`

```go
package main

import (
	"log"

	"github.com/gtkit/logrotate"
)

func main() {
	w := &logrotate.Logger{
		Filename:   "/var/log/myapp/app.log",
		MaxSize:    500, // MB
		MaxBackups: 7,
		MaxAge:     30, // 天
		Compress:   true,
	}
	defer w.Close()

	log.SetOutput(w)
	log.Println("service started")
}
```

### 配合 `slog`

```go
package main

import (
	"log/slog"

	"github.com/gtkit/logrotate"
)

func main() {
	w := &logrotate.Logger{
		Filename: "/var/log/myapp/app.log",
		MaxSize:  200,
		Compress: true,
	}
	defer w.Close()

	logger := slog.New(slog.NewJSONHandler(w, nil))
	logger.Info("service started", "module", "api")
}
```

### 配合 `zap`

```go
package main

import (
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/gtkit/logrotate"
)

func main() {
	w := &logrotate.Logger{
		Filename:      "/var/log/myapp/app.log",
		DailyFilename: true,
		MaxSize:       500,
		MaxBackups:    14,
		MaxAge:        30,
		Location:      time.Local,
		Compress:      true,
	}
	if err := w.Open(); err != nil {
		panic(err)
	}
	defer w.Close()

	encoderCfg := zap.NewProductionEncoderConfig()
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.AddSync(w),
		zap.InfoLevel,
	)
	logger := zap.New(core)
	defer logger.Sync()

	logger.Info("service started", zap.String("module", "api"))
}
```

### 配合 `logrus`

```go
package main

import (
	"github.com/sirupsen/logrus"

	"github.com/gtkit/logrotate"
)

func main() {
	w := &logrotate.Logger{
		Filename:   "/var/log/myapp/app.log",
		Daily:      true,
		MaxSize:    500,
		MaxBackups: 14,
		MaxAge:     30,
		LocalTime:  true,
		Compress:   true,
	}
	defer w.Close()

	logger := logrus.New()
	logger.SetOutput(w)
	logger.SetFormatter(&logrus.JSONFormatter{})

	logger.WithField("module", "api").Info("service started")
}
```

## 常用配置

```go
w := &logrotate.Logger{
	Filename:      "/var/log/myapp/app.log",
	MaxSize:       500,
	MaxBackups:    7,
	MaxAge:        30,
	Location:      time.Local,
	Compress:      true,
	Daily:         true,
	DailyFilename: false,
}
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `Filename` | `os.TempDir()` 下的 `<进程名>-logrotate.log` | 当前活跃日志文件路径 |
| `MaxSize` | `100` | 单个日志文件最大大小，单位 MB |
| `MaxAge` | `0` | 旧日志最长保留天数；`0` 表示不按时间删除 |
| `MaxBackups` | `0` | 最多保留旧日志数量；`0` 表示不按数量删除 |
| `LocalTime` | `false` | 轮转时间和日期文件名默认使用 UTC；设为 `true` 后使用本地时间 |
| `Location` | `nil` | 指定轮转时间、日期文件名和清理截止时间使用的时区；非空时优先于 `LocalTime` |
| `Compress` | `false` | 是否用 gzip 压缩旧日志 |
| `Daily` | `false` | 是否在跨天时轮转当前日志文件 |
| `DailyFilename` | `false` | 当前活跃日志文件名是否带日期 |
| `Now` | `nil` | 当前时间源；为空时使用 `time.Now`，主要用于测试 |
| `OnError` | `nil` | 后台清理或压缩失败时的错误回调 |

配置字段应在首次 `Write`、`Rotate` 或 `Close` 前设置好。开始使用后不要并发修改这些字段。

## 切割模式

### 1. 只按大小切割

```go
w := &logrotate.Logger{
	Filename:   "/var/log/myapp/app.log",
	MaxSize:    500,
	MaxBackups: 5,
	MaxAge:     14,
	Compress:   true,
}
```

当 `/var/log/myapp/app.log` 即将超过 `500 MB` 时，会生成带时间戳的备份文件：

```text
app-2026-06-09T10-30-00.000.log
app-2026-06-09T10-30-00.000.log.gz
```

活跃文件始终还是：

```text
app.log
```

### 2. 按天切割，活跃文件名不变

```go
w := &logrotate.Logger{
	Filename: "/var/log/myapp/app.log",
	Daily:    true,
	MaxSize:  500,
}
```

跨天后，旧的 `app.log` 会被改名为带时间戳的备份文件，新日志继续写入新的 `app.log`。

适合希望日志采集器始终读取固定文件名的场景。

### 3. 活跃文件名带日期

```go
w := &logrotate.Logger{
	Filename:      "/var/log/myapp/app.log",
	DailyFilename: true,
	MaxSize:       500,
}
```

实际活跃文件会变成：

```text
app-2026-06-09.log
app-2026-06-10.log
```

每天写入当天对应文件。`MaxSize` 仍然生效，同一天内文件过大时会继续生成时间戳备份。

适合按日期直接查看日志文件的场景。

## 清理旧日志

`MaxBackups` 和 `MaxAge` 可以同时使用。

```go
w := &logrotate.Logger{
	Filename:   "/var/log/myapp/app.log",
	MaxSize:    500,
	MaxBackups: 10,
	MaxAge:     30,
	Compress:   true,
}
```

含义：

- `MaxBackups: 10`：最多保留 10 组旧日志。
- `MaxAge: 30`：删除时间戳超过 30 天的旧日志。
- `Compress: true`：轮转后的旧日志会压缩为 `.gz`。

如果 `MaxBackups` 和 `MaxAge` 都是 `0`，不会自动删除旧日志。

清理判断使用文件名中的日期或时间戳，不使用文件的修改时间。这样日志保留规则不受复制、解压、备份恢复或手工 `touch` 影响。按天文件使用 `app-2006-01-02.log` 里的日期；按大小备份使用 `app-2006-01-02T15-04-05.000.log` 里的时间戳。

默认清理和压缩在后台执行，不阻塞 `Write()` / `Rotate()`。如果应用需要启动时立即回收历史积压文件，调用 `Open()`：

```go
w := &logrotate.Logger{
	Filename:      "/var/log/myapp/app.log",
	DailyFilename: true,
	MaxAge:        30,
	MaxBackups:    14,
	Compress:      true,
	OnError: func(err error) {
		log.Printf("rotate cleanup: %v", err)
	},
}
if err := w.Open(); err != nil {
	log.Fatal(err)
}
defer w.Close()
```

只需要同步清理、不需要打开当前日志文件时，调用 `Cleanup()`。后台清理或压缩失败会传给 `OnError`；显式 `Cleanup()` 的失败直接通过返回值暴露。

## 手动轮转

可以在收到 `SIGHUP` 时主动切割日志。

```go
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gtkit/logrotate"
)

func main() {
	w := &logrotate.Logger{
		Filename: "/var/log/myapp/app.log",
		MaxSize:  500,
	}
	defer w.Close()

	log.SetOutput(w)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)

	go func() {
		for range ch {
			if err := w.Rotate(); err != nil {
				log.Printf("rotate log: %v", err)
			}
		}
	}()

	select {}
}
```

## 在业务 logger 项目中使用

如果业务 logger 项目只需要文件切割能力，不需要重复实现按天切割。直接把 `logrotate.Logger` 作为底层 `io.Writer` 注入即可。

示例：

```go
func NewFileWriter(path string) *logrotate.Logger {
	return &logrotate.Logger{
		Filename:      path,
		DailyFilename: true,
		MaxSize:       500,
		MaxBackups:    14,
		MaxAge:        30,
		Location:      time.Local,
		Compress:      true,
	}
}
```

上层 logger 继续负责：

- 日志级别。
- 文本或 JSON 格式。
- trace id、request id 等字段。
- 多输出端分发。

`logrotate` 负责：

- 文件写入。
- 按大小切割。
- 按天切割。
- 旧文件压缩。
- 旧文件清理。

## 并发与进程约束

- `Logger` 可以被多个 goroutine 并发写入。
- 并发写入会被串行化，以保证文件大小统计、轮转判断和写入顺序一致。
- `Write()`、`Rotate()`、`Close()` 之间需串行调用：`Close()` 或 `Rotate()` 期间不要并发调用 `Write()`，`Close()` 期间也不要并发调用 `Rotate()`。
- `Logger` 内部持有锁，首次使用后不得复制；请始终通过指针（`*logrotate.Logger`）传递和使用。
- 配置字段必须在首次使用前设置完成，运行中不要并发修改。
- 旧日志的压缩和清理在后台异步执行，不阻塞 `Write`。`Close()` 只等待本实例已调度的后台任务完成后才返回，不等待其他 `Logger` 实例的后台任务。
- `Open()` 会初始化当前文件并同步执行清理；如果清理历史积压文件失败，错误会直接返回。
- `Close()` 不会使 `Logger` 进入终止状态；之后再次 `Write` 会按现有配置重新打开日志文件。
- 同一份日志文件只能由一个进程写入。多个进程使用相同 `Filename` 会导致不正确的轮转行为。

## API 概览

| 方法 | 说明 |
|------|------|
| `Open() error` | 打开或创建当前活跃文件，并同步执行一次旧日志清理/压缩 |
| `Write(p []byte) (int, error)` | 写入日志内容，必要时自动轮转 |
| `Rotate() error` | 立即手动轮转当前日志文件 |
| `Cleanup() error` | 同步执行一次旧日志清理/压缩，并返回失败原因 |
| `CurrentFilename() string` | 返回当前活跃日志文件路径 |
| `Sync() error` | 将当前文件的内核缓冲刷写到磁盘，满足 `zapcore.WriteSyncer` 等需要 `Sync` 的接口 |
| `Close() error` | 关闭当前打开的日志文件，并等待本实例已调度的后台清理/压缩完成后才返回；不会终止后续写入 |

## 文件命名规则

假设配置为：

```go
Filename: "/var/log/myapp/app.log"
```

按大小或 `Daily` 轮转时，备份文件格式为：

```text
/var/log/myapp/app-2026-06-09T10-30-00.000.log
```

启用压缩后：

```text
/var/log/myapp/app-2026-06-09T10-30-00.000.log.gz
```

启用 `DailyFilename` 后，活跃文件格式为：

```text
/var/log/myapp/app-2026-06-09.log
```

## 致谢与来源

本包基于 [lumberjack](https://github.com/natefinch/lumberjack)（作者 Nate Finch，MIT 许可证）改造，在其文件切割、压缩、清理能力之上新增了按天切割（`Daily` / `DailyFilename`）等功能。感谢原作者的工作。

## License
