package logrotate

import "time"

// Option 配置 New 创建的 Logger。
type Option func(*Logger)

// New 返回按 opts 配置的 Logger。
//
// 不传选项时，New 返回的 Logger 与 &Logger{} 使用相同默认行为。nil 选项会被忽略。
func New(opts ...Option) *Logger {
	l := &Logger{}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	return l
}

// WithFilename 设置当前活跃日志文件路径。
func WithFilename(filename string) Option {
	return func(l *Logger) {
		l.Filename = filename
	}
}

// WithMaxSize 设置单个日志文件轮转前的最大大小，单位 MB。
// 负数会按 0 处理，即使用 Logger 的默认大小。
func WithMaxSize(maxSize int) Option {
	return func(l *Logger) {
		l.MaxSize = nonNegative(maxSize)
	}
}

// WithMaxAge 设置旧日志最多保留天数。
// 负数会按 0 处理，即不按时间删除。
func WithMaxAge(maxAge int) Option {
	return func(l *Logger) {
		l.MaxAge = nonNegative(maxAge)
	}
}

// WithMaxBackups 设置最多保留的旧日志数量。
// 负数会按 0 处理，即不按数量删除。
func WithMaxBackups(maxBackups int) Option {
	return func(l *Logger) {
		l.MaxBackups = nonNegative(maxBackups)
	}
}

// WithLocalTime 设置轮转时间和日期文件名是否使用本地时间。
func WithLocalTime(localTime bool) Option {
	return func(l *Logger) {
		l.LocalTime = localTime
	}
}

// WithCompress 设置轮转后的旧日志是否使用 gzip 压缩。
func WithCompress(compress bool) Option {
	return func(l *Logger) {
		l.Compress = compress
	}
}

// WithDaily 设置是否在日期变化时轮转日志文件。
func WithDaily(daily bool) Option {
	return func(l *Logger) {
		l.Daily = daily
	}
}

// WithDailyFilename 设置活跃日志文件名是否包含当前日期。
func WithDailyFilename(dailyFilename bool) Option {
	return func(l *Logger) {
		l.DailyFilename = dailyFilename
	}
}

// WithNow 设置当前时间源。
func WithNow(now func() time.Time) Option {
	return func(l *Logger) {
		l.Now = now
	}
}

// WithLocation 设置轮转时间、日期文件名、日期边界和 MaxAge 截止时间使用的时区。
func WithLocation(location *time.Location) Option {
	return func(l *Logger) {
		l.Location = location
	}
}

// WithOnError 设置后台清理、压缩或 panic recover 产生错误时的回调。
func WithOnError(onError func(error)) Option {
	return func(l *Logger) {
		l.OnError = onError
	}
}

func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
