// Package logrotate 提供日志文件写入、切割、压缩和清理能力。
//
// 使用方式：
//
//	import "github.com/gitkit/logrotate"
//
// logrotate 只负责日志文件输出端的管理，不负责日志格式化、日志级别或字段编码。
// 它可以作为任何接收 io.Writer 的日志库输出目标，例如标准库 log、slog、
// zap 和 logrus。
//
// 同一份日志文件只能由一个进程写入。多个进程使用相同配置写入同一文件会导致
// 不正确的轮转行为。
package logrotate

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

	size           int64
	file           *os.File
	nextRotateTime time.Time
	mu             sync.Mutex

	filenameMu      sync.RWMutex
	currentFilename string
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
	if writeLen > l.max() {
		return 0, fmt.Errorf(
			"write length %d exceeds maximum file size %d", writeLen, l.max(),
		)
	}

	if l.file == nil {
		if err = l.openExistingOrNew(len(p)); err != nil {
			return 0, err
		}
	}

	if l.shouldSwitchDailyFilename() {
		if err := l.switchDailyFilename(len(p)); err != nil {
			return 0, err
		}
	}

	if l.shouldRotateByDay() || l.size+writeLen > l.max() {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}

	n, err = l.file.Write(p)
	l.size += int64(n)

	return n, err
}

// Close 实现 io.Closer，并关闭当前打开的日志文件。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.close()
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
	if err := l.close(); err != nil {
		return err
	}
	if err := l.openNew(); err != nil {
		return err
	}
	l.mill()
	return nil
}

// openNew 打开新的日志文件用于写入，并在需要时移走旧文件。
// 调用方必须确保当前文件已经关闭。
func (l *Logger) openNew() error {
	err := os.MkdirAll(l.dir(), 0755)
	if err != nil {
		return fmt.Errorf("can't make directories for new logfile: %s", err)
	}

	if l.DailyFilename && l.currentActiveFilename() == "" {
		l.setCurrentFilename(l.dailyFilename(currentTime()))
	}
	name := l.filename()
	mode := os.FileMode(0600)
	info, err := osStat(name)
	if err == nil {
		// 继承旧日志文件的权限。
		mode = info.Mode()
		// 移走已有文件。
		newname := backupName(name, l.LocalTime)
		if err := os.Rename(name, newname); err != nil {
			return fmt.Errorf("can't rename log file: %s", err)
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
		return fmt.Errorf("can't open new logfile: %s", err)
	}
	l.file = f
	l.size = 0
	l.setNextRotateTime()
	return nil
}

// backupName 基于给定文件名生成备份文件名，并在文件名和扩展名之间插入时间戳。
// local 为 true 时使用本地时间，否则使用 UTC。
func backupName(name string, local bool) string {
	dir := filepath.Dir(name)
	filename := filepath.Base(name)
	ext := filepath.Ext(filename)
	prefix := filename[:len(filename)-len(ext)]
	t := currentTime()
	if !local {
		t = t.UTC()
	}

	timestamp := t.Format(backupTimeFormat)
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", prefix, timestamp, ext))
}

// openExistingOrNew 在已有日志文件存在且本次写入不会超过 MaxSize 时打开它。
// 如果文件不存在，或本次写入会让它超过 MaxSize，则创建新文件。
func (l *Logger) openExistingOrNew(writeLen int) error {
	l.mill()

	if l.DailyFilename && l.currentActiveFilename() == "" {
		l.setCurrentFilename(l.dailyFilename(currentTime()))
	}
	filename := l.filename()
	info, err := osStat(filename)
	if os.IsNotExist(err) {
		return l.openNew()
	}
	if err != nil {
		return fmt.Errorf("error getting log file info: %s", err)
	}

	if l.existingFileIsOld(info) || info.Size()+int64(writeLen) >= l.max() {
		return l.rotate()
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
		return l.dailyFilename(currentTime())
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
		diff := time.Duration(int64(24*time.Hour) * int64(l.MaxAge))
		cutoff := currentTime().Add(-1 * diff)

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
			// 后台清理失败没有可用日志输出，只能忽略。
			_ = l.millRunOnce()
		}
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
	millPending[l] = struct{}{}
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
		return nil, fmt.Errorf("can't read log file directory: %s", err)
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

	sort.Sort(byFormatTime(logFiles))

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
	return time.Parse(backupTimeFormat, ts)
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

	sort.Sort(byFormatTime(logFiles))
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
		return time.Parse(dailyNameFormat, ts)
	}
	if len(ts) > len(dailyNameFormat)+1 && ts[len(dailyNameFormat)] == '-' {
		return time.Parse(backupTimeFormat, ts[len(dailyNameFormat)+1:])
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
	now := currentTime()
	if l.LocalTime {
		now = now.Local()
	}
	return !now.Before(l.nextRotateTime)
}

// existingFileIsOld 判断已有活跃日志文件是否属于旧日期，启动后首次写入前是否应先轮转。
func (l *Logger) existingFileIsOld(info os.FileInfo) bool {
	if !l.Daily || l.DailyFilename {
		return false
	}

	now := currentTime()
	modTime := info.ModTime()
	if l.LocalTime {
		now = now.Local()
		modTime = modTime.Local()
	} else {
		now = now.UTC()
		modTime = modTime.UTC()
	}

	return modTime.Before(startOfDay(now))
}

// setNextRotateTime 为已打开或新建的活跃日志文件设置下一个日期边界。
func (l *Logger) setNextRotateTime() {
	if !l.Daily && !l.DailyFilename {
		l.nextRotateTime = time.Time{}
		return
	}

	now := currentTime()
	if l.LocalTime {
		now = now.Local()
	} else {
		now = now.UTC()
	}
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
	now := currentTime()
	if l.LocalTime {
		now = now.Local()
	} else {
		now = now.UTC()
	}
	return !now.Before(l.nextRotateTime)
}

func (l *Logger) switchDailyFilename(writeLen int) error {
	if err := l.close(); err != nil {
		return err
	}
	l.setCurrentFilename(l.dailyFilename(currentTime()))
	return l.openExistingOrNew(writeLen)
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
	if l.LocalTime {
		t = t.Local()
	} else {
		t = t.UTC()
	}

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

// compressLogFile 压缩指定日志文件，并在成功后删除未压缩文件。
func compressLogFile(src, dst string) (err error) {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open log file: %v", err)
	}
	defer func() {
		_ = f.Close()
	}()

	fi, err := osStat(src)
	if err != nil {
		return fmt.Errorf("failed to stat log file: %v", err)
	}

	if err := chown(dst, fi); err != nil {
		return fmt.Errorf("failed to chown compressed log file: %v", err)
	}

	// 如果目标文件已存在，视为之前压缩尝试留下的文件，直接覆盖。
	gzf, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode())
	if err != nil {
		return fmt.Errorf("failed to open compressed log file: %v", err)
	}
	defer func() {
		_ = gzf.Close()
	}()

	gz := gzip.NewWriter(gzf)

	defer func() {
		if err != nil {
			_ = os.Remove(dst)
			err = fmt.Errorf("failed to compress log file: %v", err)
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

// byFormatTime 按文件名中的时间戳从新到旧排序。
type byFormatTime []logInfo

func (b byFormatTime) Less(i, j int) bool {
	return b[i].timestamp.After(b[j].timestamp)
}

func (b byFormatTime) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}

func (b byFormatTime) Len() int {
	return len(b)
}
