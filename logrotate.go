// Package logrotate 提供日志文件写入、切割、压缩和清理能力。
//
// 使用方式：
//
//	import "github.com/gtkit/logrotate"
//
// logrotate 只负责日志文件输出端的管理，不负责日志格式化、日志级别或字段编码。
// 它可以作为任何接收 io.Writer 的日志库输出目标，例如标准库 log、slog、
// zap 和 logrus。
//
// 同一份日志文件只能由一个进程写入。多个进程使用相同配置写入同一文件会导致
// 不正确的轮转行为。
//
// # 使用约束与已知限制
//
// 以下都是有意的设计取舍或该类库的固有约束：
//
//   - 配置字段不可热改：导出字段必须在首次 Open/Write/Rotate/Cleanup 之前设置好，
//     运行中不要与这些操作并发修改，否则会发生 data race。需要不同配置请新建 Logger。
//   - Open 是同步的：它会同步清理并压缩历史积压文件，积压多时启动会变慢，属于预期行为。
//     不想承担该开销时可以不调用 Open，让首次 Write 走后台异步清理。
//   - 后台清理是全局单 worker：所有 Logger 实例共享一个后台清理/压缩 goroutine，多实例
//     同时压缩大文件会排队。副作用是 Close 等待本实例已调度的后台任务时，若该 worker 正
//     忙于其他实例的压缩，本实例的 Close 也会一并等待后返回。
//   - 保留判断只看文件名，不看 modTime：MaxAge 与 MaxBackups 按文件名中的日期/时间戳判断，
//     不符合本包命名规则的文件会被忽略，既不会被清理也不会被误删。迁移老目录前请确认文件名
//     符合规则，否则旧文件不会被自动回收。
//   - 同一份日志文件只能单进程写：本包不加文件锁，多进程共用同一 Filename 会导致轮转竞争。
//   - 同步清理无超时：Open 与 Close 的清理/压缩没有超时控制，磁盘 IO 病态时理论上会阻塞，
//     属于已知边界。
package logrotate

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	backupTimeFormat = "2006-01-02T15-04-05.000"
	dailyNameFormat  = "2006-01-02"
	compressSuffix   = ".gz"
	defaultMaxSize   = 100
)

// 确保 Logger 实现 io.WriteCloser。
var _ io.WriteCloser = (*Logger)(nil)

// Logger 是写入指定日志文件的 io.WriteCloser。
//
// Logger 会在首次 Write 时打开或创建日志文件。如果文件已存在且小于 MaxSize
// 兆字节，Logger 会追加写入该文件。如果文件大小大于或等于 MaxSize，Logger
// 会在文件扩展名前加入当前时间戳作为备份文件名，并用原始文件名创建新的日志文件。
//
// 当一次写入会导致当前日志文件超过 MaxSize 时，Logger 会关闭当前文件、重命名为
// 备份文件，并用原始文件名创建新文件。因此 Filename 始终表示当前活跃日志文件。
//
// 如果 Daily 为 true，Logger 会在日期变化时轮转当前文件。日期边界默认使用 UTC，
// LocalTime 为 true 时使用本地时间。MaxSize 仍然生效，因此可以同时按天和按大小
// 轮转。
//
// 如果 DailyFilename 为 true，活跃日志文件名会包含当前日期，格式为
// `name-2006-01-02.ext`。跨天时 Logger 会关闭旧日期文件，并开始写入新日期文件。
// MaxSize 在每天的文件内仍然生效，按大小生成的备份仍使用普通时间戳格式。
//
// 备份文件使用 Logger 的日志文件名生成，格式为 `name-timestamp.ext`。其中 name
// 是不含扩展名的文件名，timestamp 是轮转时间，格式为 `2006-01-02T15-04-05.000`，
// ext 是原始扩展名。例如 Filename 为 `/var/log/foo/server.log` 时，备份文件可能是
// `/var/log/foo/server-2026-06-09T10-30-00.000.log`。
//
// # 清理旧日志文件
//
// 每次创建新日志文件后，Logger 可能会删除旧日志。按文件名里的时间戳排序后，最多
// 保留 MaxBackups 组旧日志；MaxBackups 为 0 时不按数量删除。时间戳早于 MaxAge
// 天的旧日志会被删除；MaxAge 为 0 时不按时间删除。文件名里的时间是轮转时间，
// 可能不同于该文件最后一次写入的时间。
//
// 如果 MaxBackups 和 MaxAge 都是 0，不会自动删除旧日志。
//
// Logger 会串行化并发写入，以保证大小统计、轮转判断和写入顺序一致。请在首次使用前
// 设置好导出的配置字段，使用过程中不要与 Write、Rotate 或 Close 并发修改这些字段。
//
// Logger 内部持有锁，首次使用后不得复制；请始终通过指针（*Logger）传递和使用。
type Logger struct {
	// Filename 是当前活跃日志文件路径。备份日志会保存在同一目录。
	// 为空时使用 os.TempDir() 下的 <进程名>-logrotate.log。
	Filename string `json:"filename" yaml:"filename"`

	// MaxSize 是单个日志文件轮转前的最大大小，单位 MB。默认值为 100。
	MaxSize int `json:"maxsize" yaml:"maxsize"`

	// MaxAge 是旧日志最多保留天数，基于文件名里的时间戳计算。
	// 一天按 24 小时计算，可能与夏令时、闰秒等日历边界不完全一致。
	// 默认不按时间删除旧日志。
	MaxAge int `json:"maxage" yaml:"maxage"`

	// MaxBackups 是最多保留的旧日志数量。默认保留全部旧日志，但 MaxAge 仍可能删除它们。
	MaxBackups int `json:"maxbackups" yaml:"maxbackups"`

	// LocalTime 控制备份时间戳和日期文件名是否使用本地时间。默认使用 UTC。
	LocalTime bool `json:"localtime" yaml:"localtime"`

	// Compress 控制轮转后的旧日志是否使用 gzip 压缩。默认不压缩。
	Compress bool `json:"compress" yaml:"compress"`

	// Daily 控制是否在日期变化时轮转日志文件。日期边界默认按 UTC 判断，
	// LocalTime 为 true 时按本地时区判断。默认不按天轮转。
	Daily bool `json:"daily" yaml:"daily"`

	// DailyFilename 控制活跃日志文件名是否包含当前日期，格式为 name-2006-01-02.ext。
	// 日期默认使用 UTC，LocalTime 为 true 时使用本地时区。MaxSize 在每天的文件内仍然生效。
	DailyFilename bool `json:"dailyfilename" yaml:"dailyfilename"`

	// Now 返回当前时间。为空时使用 time.Now。
	// 主要用于测试或需要固定业务时间源的场景。请在首次使用前设置。
	Now func() time.Time `json:"-" yaml:"-"`

	// Location 控制备份时间戳、日期文件名、日期边界和 MaxAge 截止时间使用的时区。
	// 非空时优先于 LocalTime；为空且 LocalTime 为 true 时使用 time.Local；否则使用 UTC。
	Location *time.Location `json:"-" yaml:"-"`

	// OnError 接收后台清理、压缩或 panic recover 产生的错误。
	// 为空时后台错误保持非致命并被忽略。Cleanup 返回的同步错误不会重复调用 OnError。
	OnError func(error) `json:"-" yaml:"-"`

	size           int64
	file           *os.File
	nextRotateTime time.Time
	mu             sync.Mutex

	filenameMu      sync.RWMutex
	currentFilename string

	// millWG 跟踪本 Logger 已调度但尚未完成的后台清理/压缩任务，
	// 使 Close 能等待这些任务结束，避免清理 goroutine 逃逸出 Logger 生命周期。
	millWG sync.WaitGroup

	// millRunMu 串行化 millRunOnce，使同步入口（Cleanup/Open）与后台 worker 不会
	// 在同一 Logger 上并发删除或压缩同一批文件，避免压缩产物损坏与误导性的 not-found 错误。
	// 单独使用一把锁而非复用 l.mu：清理是 I/O 密集操作，复用会长时间阻塞 Write。
	millRunMu sync.Mutex
}

var (
	// currentTime 用于测试中替换当前时间。
	currentTime = time.Now

	// osStat 用于测试中替换 os.Stat。
	osStat = os.Stat

	millMu      sync.Mutex
	millCh      chan struct{}
	millPending map[*Logger]struct{}
	startMill   sync.Once

	// megabyte 是 MaxSize 与字节数之间的换算系数。
	// 测试会替换它，以避免实际写入大量数据。
	megabyte = 1024 * 1024
)

// Write 实现 io.Writer。写入会导致当前日志文件超过 MaxSize 时，Logger 会关闭当前文件、
// 将其重命名为带时间戳的备份文件，并用原始文件名创建新日志文件。
// 如果单次写入长度超过 MaxSize，Write 返回错误。
func (l *Logger) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	writeLen := int64(len(p))
	maxSize := l.max()
	if writeLen > maxSize {
		return 0, fmt.Errorf(
			"write length %d exceeds maximum file size %d", writeLen, maxSize,
		)
	}

	if l.file == nil {
		if err = l.openExistingOrNew(len(p), true); err != nil {
			return 0, err
		}
	}

	if l.shouldSwitchDailyFilename() {
		if err := l.switchDailyFilename(len(p)); err != nil {
			return 0, err
		}
	}

	if l.shouldRotateByDay() || l.size+writeLen > maxSize {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}

	n, err = l.file.Write(p)
	l.size += int64(n)

	return n, err
}

// Open 打开或创建当前活跃日志文件，并同步执行一次旧日志清理/压缩。
//
// 调用 Open 可在应用启动时初始化目录、创建当前文件并清理历史积压文件，而无需等待首次
// Write。Open 不会使 Logger 进入终止状态；Close 后仍可再次 Open 或 Write。
// Open 期间不要并发调用 Write、Rotate 或 Close。
func (l *Logger) Open() error {
	l.mu.Lock()
	if l.file == nil {
		if err := l.openExistingOrNew(0, false); err != nil {
			l.mu.Unlock()
			return err
		}
	}
	l.mu.Unlock()
	return l.Cleanup()
}

// Close 实现 io.Closer，关闭当前打开的日志文件，并等待本 Logger 已调度的后台
// 清理与压缩任务全部完成后才返回。
//
// 因此 Close 返回后，由本 Logger 触发的旧日志删除和压缩都已结束处理（清理或压缩
// 自身的失败会被忽略，不保证一定成功），不会有清理 goroutine 在 Close 之后继续运行。
// Close 不会使 Logger 进入终止状态，之后的 Write 可以按现有配置重新打开日志文件。
// Close 期间不要并发调用 Write 或 Rotate。
func (l *Logger) Close() error {
	l.mu.Lock()
	err := l.close()
	l.mu.Unlock()

	// 在锁外等待后台清理/压缩，避免长时间持有 l.mu 阻塞其他操作；
	// millRunOnce 不依赖 l.mu，因此不会死锁。
	l.millWG.Wait()
	return err
}

// Sync 将当前日志文件的内核缓冲刷写到磁盘。
//
// 它让 Logger 满足 zap 的 zapcore.WriteSyncer 等需要 Sync 的接口，可在需要持久化
// 保证的场景显式调用。当前没有打开的文件时返回 nil。
func (l *Logger) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	return l.file.Sync()
}

// Cleanup 同步执行一次旧日志压缩和清理，并返回遇到的第一个错误。
//
// Cleanup 不通过后台 worker 调度，也不会为返回的同步错误调用 OnError。需要启动即回收
// 历史积压文件时，应用可以在完成配置后显式调用 Cleanup 或 Open。
func (l *Logger) Cleanup() error {
	return l.millRunOnce()
}

// CurrentFilename 返回当前活跃日志文件路径。
//
// DailyFilename 关闭时返回 Filename 或默认文件名；DailyFilename 开启时返回当前日期对应
// 的活跃文件名。若内部已经切换到某个日期文件，则返回该文件名。
func (l *Logger) CurrentFilename() string {
	return l.filename()
}

// close 关闭当前打开的文件。
func (l *Logger) close() error {
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Rotate 关闭现有日志文件并立即创建新文件。
// 应用可以用它在正常轮转规则之外主动触发轮转，例如响应 SIGHUP。
// 轮转后，Logger 会按配置触发旧日志压缩和清理。
func (l *Logger) Rotate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rotate()
}

// rotate 关闭当前文件，将已有文件重命名为带时间戳的备份文件，随后用原始文件名
// 打开新文件并触发轮转后的处理。
func (l *Logger) rotate() error {
	return l.rotateWithCleanup(true)
}

func (l *Logger) rotateWithCleanup(scheduleCleanup bool) error {
	if err := l.close(); err != nil {
		return err
	}
	if err := l.openNew(); err != nil {
		return err
	}
	if scheduleCleanup {
		l.mill()
	}
	return nil
}

// openNew 打开新的日志文件用于写入，并在需要时移走旧文件。
// 调用方必须确保当前文件已经关闭。
func (l *Logger) openNew() error {
	err := os.MkdirAll(l.dir(), 0o755)
	if err != nil {
		return fmt.Errorf("can't make directories for new logfile: %w", err)
	}

	if l.DailyFilename && l.currentActiveFilename() == "" {
		l.setCurrentFilename(l.dailyFilename(l.now()))
	}
	name := l.filename()
	mode := os.FileMode(0o600)
	info, err := osStat(name)
	if err == nil {
		// 继承旧日志文件的权限。
		mode = info.Mode()
		// 移走已有文件。
		newname := l.backupName(name)
		if err := os.Rename(name, newname); err != nil {
			return fmt.Errorf("can't rename log file: %w", err)
		}

		// 仅 Linux 会保留所有者，其他平台是空操作。
		if err := chown(name, info); err != nil {
			return err
		}
	}

	// 这里使用 truncate，因为调用到此处时旧文件应已由本包移走。
	// 如果期间有其他写入方创建了同名文件，则直接清空它。
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("can't open new logfile: %w", err)
	}
	l.file = f
	l.size = 0
	l.setNextRotateTime()
	return nil
}

// backupName 基于给定文件名生成备份文件名，并在文件名和扩展名之间插入时间戳。
func (l *Logger) backupName(name string) string {
	dir := filepath.Dir(name)
	filename := filepath.Base(name)
	ext := filepath.Ext(filename)
	prefix := filename[:len(filename)-len(ext)]
	t := l.clockTime()

	timestamp := t.Format(backupTimeFormat)
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", prefix, timestamp, ext))
}

// openExistingOrNew 在已有日志文件存在且本次写入不会超过 MaxSize 时打开它。
// 如果文件不存在，或本次写入会让它超过 MaxSize，则创建新文件。
func (l *Logger) openExistingOrNew(writeLen int, scheduleCleanup bool) error {
	if scheduleCleanup {
		l.mill()
	}

	if l.DailyFilename && l.currentActiveFilename() == "" {
		l.setCurrentFilename(l.dailyFilename(l.now()))
	}
	filename := l.filename()
	info, err := osStat(filename)
	if os.IsNotExist(err) {
		return l.openNew()
	}
	if err != nil {
		return fmt.Errorf("error getting log file info: %w", err)
	}

	if l.existingFileIsOld(info) || info.Size()+int64(writeLen) >= l.max() {
		return l.rotateWithCleanup(scheduleCleanup)
	}

	file, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		// 如果旧日志文件无法打开，则忽略旧文件并创建新日志文件。
		return l.openNew()
	}
	l.file = file
	l.size = info.Size()
	l.setNextRotateTime()
	return nil
}

// filename 根据当前配置生成活跃日志文件名。
func (l *Logger) filename() string {
	if l.DailyFilename {
		if name := l.currentActiveFilename(); name != "" {
			return name
		}
		return l.dailyFilename(l.now())
	}
	return l.baseFilename()
}

func (l *Logger) baseFilename() string {
	if l.Filename != "" {
		return l.Filename
	}
	name := filepath.Base(os.Args[0]) + "-logrotate.log"
	return filepath.Join(os.TempDir(), name)
}

// millRunOnce 执行一次旧日志压缩和清理。
// 启用 Compress 时压缩旧日志，并按照 MaxBackups 和 MaxAge 删除过期日志。
func (l *Logger) millRunOnce() error {
	l.millRunMu.Lock()
	defer l.millRunMu.Unlock()

	if l.MaxBackups == 0 && l.MaxAge == 0 && !l.Compress {
		return nil
	}

	files, err := l.oldLogFiles()
	if err != nil {
		return err
	}

	var compress, remove []logInfo

	if l.MaxBackups > 0 && l.MaxBackups < len(files) {
		preserved := make(map[string]bool)
		var remaining []logInfo
		for _, f := range files {
			// 同一组日志只统计未压缩文件或压缩文件之一，避免重复计数。
			fn := f.Name()
			fn = strings.TrimSuffix(fn, compressSuffix)
			preserved[fn] = true

			if len(preserved) > l.MaxBackups {
				remove = append(remove, f)
			} else {
				remaining = append(remaining, f)
			}
		}
		files = remaining
	}
	if l.MaxAge > 0 {
		diff := time.Duration(l.MaxAge) * 24 * time.Hour
		cutoff := l.clockTime().Add(-diff)

		var remaining []logInfo
		for _, f := range files {
			if f.timestamp.Before(cutoff) {
				remove = append(remove, f)
			} else {
				remaining = append(remaining, f)
			}
		}
		files = remaining
	}

	if l.Compress {
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), compressSuffix) {
				compress = append(compress, f)
			}
		}
	}

	for _, f := range remove {
		errRemove := os.Remove(filepath.Join(l.dir(), f.Name()))
		if err == nil && errRemove != nil {
			err = errRemove
		}
	}
	for _, f := range compress {
		fn := filepath.Join(l.dir(), f.Name())
		errCompress := compressLogFile(fn, fn+compressSuffix)
		if err == nil && errCompress != nil {
			err = errCompress
		}
	}

	return err
}

// millRun 在后台 goroutine 中处理轮转后的旧日志压缩和清理。
func millRun() {
	for range millCh {
		for {
			millMu.Lock()
			var l *Logger
			for pending := range millPending {
				l = pending
				delete(millPending, pending)
				break
			}
			millMu.Unlock()
			if l == nil {
				break
			}
			l.runMillOnce()
		}
	}
}

// runMillOnce 执行一次后台清理，并保证无论 millRunOnce 是否 panic 都调用 millWG.Done，
// 否则 Close 的 Wait 会永久阻塞。recover 同时防止单个 Logger 的意外 panic 拖垮全局 worker。
func (l *Logger) runMillOnce() {
	// 与 mill() 中的 Add 配对，通知 Close 这次调度已处理完毕。
	defer l.millWG.Done()
	defer func() {
		// 后台清理失败没有可用日志输出，panic 也只能吞掉，避免影响其他 Logger。
		if recovered := recover(); recovered != nil {
			l.reportError(fmt.Errorf("logrotate: background cleanup panic: %v", recovered))
		}
	}()
	if err := l.millRunOnce(); err != nil {
		l.reportError(err)
	}
}

// mill 调度轮转后的旧日志压缩和清理，并在需要时启动后台 worker。
func (l *Logger) mill() {
	startMill.Do(func() {
		millCh = make(chan struct{}, 1)
		millPending = make(map[*Logger]struct{})
		go millRun()
	})
	millMu.Lock()
	// 仅在首次入队时 Add：millPending 是 set，worker 对同一 Logger 只会处理一次，
	// 所以 Add 必须与"真正入队"配对，否则 Close 的 Wait 会与 worker 的 Done 失衡。
	if _, ok := millPending[l]; !ok {
		l.millWG.Add(1)
		millPending[l] = struct{}{}
	}
	millMu.Unlock()
	select {
	case millCh <- struct{}{}:
	default:
	}
}

// oldLogFiles 返回与当前日志文件同目录下的备份日志文件列表，按文件名时间排序。
func (l *Logger) oldLogFiles() ([]logInfo, error) {
	entries, err := os.ReadDir(l.dir())
	if err != nil {
		return nil, fmt.Errorf("can't read log file directory: %w", err)
	}
	logFiles := []logInfo{}

	if l.DailyFilename {
		return l.oldDailyFilenameLogFiles(entries)
	}

	prefix, ext := l.prefixAndExt()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if t, err := l.timeFromName(entry.Name(), prefix, ext); err == nil {
			logFiles = append(logFiles, logInfo{t, info})
			continue
		}
		if t, err := l.timeFromName(entry.Name(), prefix, ext+compressSuffix); err == nil {
			logFiles = append(logFiles, logInfo{t, info})
			continue
		}
		// 解析失败说明后缀不是本包生成的备份格式，因此不是备份日志文件。
	}

	sortByFormatTimeDesc(logFiles)

	return logFiles, nil
}

// timeFromName 去掉文件名前缀和扩展名后解析时间戳，避免文件名中的其他内容干扰解析。
func (l *Logger) timeFromName(filename, prefix, ext string) (time.Time, error) {
	if !strings.HasPrefix(filename, prefix) {
		return time.Time{}, errors.New("mismatched prefix")
	}
	if !strings.HasSuffix(filename, ext) {
		return time.Time{}, errors.New("mismatched extension")
	}
	ts := filename[len(prefix) : len(filename)-len(ext)]
	return time.ParseInLocation(backupTimeFormat, ts, l.timeLocation())
}

func (l *Logger) oldDailyFilenameLogFiles(entries []os.DirEntry) ([]logInfo, error) {
	logFiles := []logInfo{}
	prefix, ext := l.prefixAndExt()
	active := filepath.Base(l.filename())

	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == active {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if t, err := l.timeFromDailyFilename(entry.Name(), prefix, ext); err == nil {
			logFiles = append(logFiles, logInfo{t, info})
			continue
		}
		if t, err := l.timeFromDailyFilename(entry.Name(), prefix, ext+compressSuffix); err == nil {
			logFiles = append(logFiles, logInfo{t, info})
			continue
		}
	}

	sortByFormatTimeDesc(logFiles)
	return logFiles, nil
}

func (l *Logger) timeFromDailyFilename(filename, prefix, ext string) (time.Time, error) {
	if !strings.HasPrefix(filename, prefix) {
		return time.Time{}, errors.New("mismatched prefix")
	}
	if !strings.HasSuffix(filename, ext) {
		return time.Time{}, errors.New("mismatched extension")
	}

	ts := filename[len(prefix) : len(filename)-len(ext)]
	if len(ts) == len(dailyNameFormat) {
		return time.ParseInLocation(dailyNameFormat, ts, l.timeLocation())
	}
	if len(ts) > len(dailyNameFormat)+1 && ts[len(dailyNameFormat)] == '-' {
		return time.ParseInLocation(backupTimeFormat, ts[len(dailyNameFormat)+1:], l.timeLocation())
	}
	return time.Time{}, errors.New("mismatched daily filename")
}

// shouldRotateByDay 判断已打开日志文件是否跨过下一个日期边界。
func (l *Logger) shouldRotateByDay() bool {
	if !l.Daily || l.DailyFilename {
		return false
	}
	if l.nextRotateTime.IsZero() {
		l.setNextRotateTime()
		return false
	}
	now := l.clockTime()
	return !now.Before(l.nextRotateTime)
}

// existingFileIsOld 判断已有活跃日志文件是否属于旧日期，启动后首次写入前是否应先轮转。
func (l *Logger) existingFileIsOld(info os.FileInfo) bool {
	if !l.Daily || l.DailyFilename {
		return false
	}

	now := l.clockTime()
	modTime := info.ModTime()
	modTime = modTime.In(now.Location())

	return modTime.Before(startOfDay(now))
}

// setNextRotateTime 为已打开或新建的活跃日志文件设置下一个日期边界。
func (l *Logger) setNextRotateTime() {
	if !l.Daily && !l.DailyFilename {
		l.nextRotateTime = time.Time{}
		return
	}

	now := l.clockTime()
	l.nextRotateTime = startOfDay(now).Add(24 * time.Hour)
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func (l *Logger) shouldSwitchDailyFilename() bool {
	if !l.DailyFilename {
		return false
	}
	if l.nextRotateTime.IsZero() {
		l.setNextRotateTime()
		return false
	}
	now := l.clockTime()
	return !now.Before(l.nextRotateTime)
}

func (l *Logger) switchDailyFilename(writeLen int) error {
	if err := l.close(); err != nil {
		return err
	}
	l.setCurrentFilename(l.dailyFilename(l.now()))
	return l.openExistingOrNew(writeLen, true)
}

// max 返回日志文件轮转前的最大字节数。
func (l *Logger) max() int64 {
	if l.MaxSize == 0 {
		return int64(defaultMaxSize * megabyte)
	}
	return int64(l.MaxSize) * int64(megabyte)
}

// dir 返回当前基础文件名所在目录。
func (l *Logger) dir() string {
	return filepath.Dir(l.baseFilename())
}

// prefixAndExt 返回 Logger 基础文件名的前缀和扩展名。
func (l *Logger) prefixAndExt() (prefix, ext string) {
	filename := filepath.Base(l.baseFilename())
	ext = filepath.Ext(filename)
	prefix = filename[:len(filename)-len(ext)] + "-"
	return prefix, ext
}

func (l *Logger) dailyFilename(t time.Time) string {
	t = l.withLocation(t)

	name := l.baseFilename()
	dir := filepath.Dir(name)
	filename := filepath.Base(name)
	ext := filepath.Ext(filename)
	prefix := filename[:len(filename)-len(ext)]
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", prefix, t.Format(dailyNameFormat), ext))
}

func (l *Logger) currentActiveFilename() string {
	l.filenameMu.RLock()
	defer l.filenameMu.RUnlock()
	return l.currentFilename
}

func (l *Logger) setCurrentFilename(name string) {
	l.filenameMu.Lock()
	l.currentFilename = name
	l.filenameMu.Unlock()
}

func (l *Logger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return currentTime()
}

func (l *Logger) clockTime() time.Time {
	return l.withLocation(l.now())
}

func (l *Logger) withLocation(t time.Time) time.Time {
	loc := l.timeLocation()
	if loc == nil {
		return t.UTC()
	}
	return t.In(loc)
}

func (l *Logger) timeLocation() *time.Location {
	if l.Location != nil {
		return l.Location
	}
	if l.LocalTime {
		return time.Local
	}
	return time.UTC
}

func (l *Logger) reportError(err error) {
	if err == nil || l.OnError == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	l.OnError(err)
}

// compressLogFile 压缩指定日志文件，并在成功后删除未压缩文件。
func compressLogFile(src, dst string) (err error) {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	fi, err := osStat(src)
	if err != nil {
		return fmt.Errorf("failed to stat log file: %w", err)
	}

	if err := chown(dst, fi); err != nil {
		return fmt.Errorf("failed to chown compressed log file: %w", err)
	}

	// 如果目标文件已存在，视为之前压缩尝试留下的文件，直接覆盖。
	gzf, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode())
	if err != nil {
		return fmt.Errorf("failed to open compressed log file: %w", err)
	}
	defer func() {
		_ = gzf.Close()
	}()

	gz := gzip.NewWriter(gzf)

	defer func() {
		if err != nil {
			_ = os.Remove(dst)
			err = fmt.Errorf("failed to compress log file: %w", err)
		}
	}()

	if _, err := io.Copy(gz, f); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := gzf.Close(); err != nil {
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return err
	}

	return nil
}

// logInfo 保存日志文件信息和文件名中的时间戳。
type logInfo struct {
	timestamp time.Time
	os.FileInfo
}

// sortByFormatTimeDesc 按文件名中的时间戳从新到旧排序。
func sortByFormatTimeDesc(files []logInfo) {
	slices.SortFunc(files, func(a, b logInfo) int {
		return b.timestamp.Compare(a.timestamp)
	})
}
