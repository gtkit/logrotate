package logrotate

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Since all the tests uses the time to determine filenames etc, we need to
// control the wall clock as much as possible, which means having a wall clock
// that doesn't change unless we want it to.
var (
	fakeTimeMu      sync.Mutex
	fakeCurrentTime = time.Now()
)

func init() {
	currentTime = fakeTime
}

func fakeTime() time.Time {
	fakeTimeMu.Lock()
	defer fakeTimeMu.Unlock()
	return fakeCurrentTime
}

func setFakeTime(t time.Time) {
	fakeTimeMu.Lock()
	fakeCurrentTime = t
	fakeTimeMu.Unlock()
}

func TestNewFile(t *testing.T) {
	dir := makeTempDir("TestNewFile", t)
	defer removeAll(dir)
	l := &Logger{
		Filename: logFile(dir),
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)
	existsWithContent(logFile(dir), b, t)
	fileCount(dir, 1, t)
}

func TestSync(t *testing.T) {
	dir := makeTempDir("TestSync", t)
	defer removeAll(dir)

	l := &Logger{
		Filename: logFile(dir),
	}
	defer closeLogger(l)

	// 没有打开文件时 Sync 应返回 nil。
	isNil(l.Sync(), t)

	b := []byte("boo!")
	_, err := l.Write(b)
	isNil(err, t)

	// 写入后 Sync 应成功，且数据仍在文件中。
	isNil(l.Sync(), t)
	existsWithContent(logFile(dir), b, t)

	// 关闭后再次 Sync 仍应返回 nil。
	isNil(l.Close(), t)
	isNil(l.Sync(), t)
}

// TestRunMillOnceRecoversFromPanic 验证后台清理即使 panic，runMillOnce 也不会向外
// 抛出 panic，且 millWG.Done 仍会执行——否则 Close 的 Wait 会永久阻塞。
func TestRunMillOnceRecoversFromPanic(t *testing.T) {
	dir := makeTempDir("TestRunMillOnceRecoversFromPanic", t)
	defer removeAll(dir)

	// 放一个待压缩的备份文件，使 millRunOnce 走到 compressLogFile，并在其中触发 osStat。
	backup := backupFileAt(dir, fakeTime())
	err := os.WriteFile(backup, []byte("data"), 0o644)
	isNil(err, t)

	// 注入 panic：compressLogFile 会调用 osStat。
	origStat := osStat
	osStat = func(string) (os.FileInfo, error) { panic("injected mill panic") }
	defer func() { osStat = origStat }()

	l := &Logger{
		Filename: logFile(dir),
		Compress: true,
	}

	// 模拟 mill() 的入队计数；runMillOnce 不应向外 panic（recover 生效）。
	l.millWG.Add(1)
	l.runMillOnce()

	// Done 已执行：用带超时的等待验证 millWG 没有卡住。
	done := make(chan struct{})
	go func() {
		l.millWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("millWG.Wait 超时：runMillOnce 未调用 Done")
	}
}

func TestCloseAllowsWriteAfterClose(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "close is not terminal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := makeTempDir("TestCloseAllowsWriteAfterClose", t)
			defer removeAll(dir)

			filename := logFile(dir)
			l := &Logger{
				Filename: filename,
				MaxSize:  100,
			}
			defer closeLogger(l)

			first := []byte("first")
			n, err := l.Write(first)
			isNil(err, t)
			equals(len(first), n, t)

			isNil(l.Close(), t)

			second := []byte("second")
			n, err = l.Write(second)
			isNil(err, t)
			equals(len(second), n, t)

			existsWithContent(filename, append(first, second...), t)
		})
	}
}

func TestOpenExisting(t *testing.T) {
	dir := makeTempDir("TestOpenExisting", t)
	defer removeAll(dir)

	filename := logFile(dir)
	data := []byte("foo!")
	err := os.WriteFile(filename, data, 0o644)
	isNil(err, t)
	existsWithContent(filename, data, t)

	l := &Logger{
		Filename: filename,
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	// make sure the file got appended
	existsWithContent(filename, append(data, b...), t)

	// make sure no other files were created
	fileCount(dir, 1, t)
}

func TestWriteTooLong(t *testing.T) {
	megabyte = 1
	dir := makeTempDir("TestWriteTooLong", t)
	defer removeAll(dir)
	l := &Logger{
		Filename: logFile(dir),
		MaxSize:  5,
	}
	defer closeLogger(l)
	b := []byte("booooooooooooooo!")
	n, err := l.Write(b)
	notNil(err, t)
	equals(0, n, t)
	equals(err.Error(),
		fmt.Sprintf("write length %d exceeds maximum file size %d", len(b), l.MaxSize), t)
	_, err = os.Stat(logFile(dir))
	assert(os.IsNotExist(err), t, "File exists, but should not have been created")
}

func TestMakeLogDir(t *testing.T) {
	dir := time.Now().Format("TestMakeLogDir" + backupTimeFormat)
	dir = filepath.Join(os.TempDir(), dir)
	defer removeAll(dir)
	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)
	existsWithContent(logFile(dir), b, t)
	fileCount(dir, 1, t)
}

func TestDefaultFilename(t *testing.T) {
	dir := os.TempDir()
	filename := filepath.Join(dir, filepath.Base(os.Args[0])+"-logrotate.log")
	defer removeFile(filename)
	l := &Logger{}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)

	isNil(err, t)
	equals(len(b), n, t)
	existsWithContent(filename, b, t)
}

func TestMillUsesSinglePackageWorker(t *testing.T) {
	tests := []struct {
		name    string
		loggers int
		want    int
	}{
		{
			name:    "many loggers share one worker",
			loggers: 3,
			want:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for range tt.loggers {
				(&Logger{}).mill()
			}
			waitForMillWorkers(tt.want, t)
			equals(tt.want, millWorkerCount(), t)
		})
	}
}

func TestAutoRotate(t *testing.T) {
	megabyte = 1

	dir := makeTempDir("TestAutoRotate", t)
	defer removeAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	fileCount(dir, 1, t)

	newFakeTime()

	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	// the old logfile should be moved aside and the main logfile should have
	// only the last write in it.
	existsWithContent(filename, b2, t)

	// the backup file will use the current fake time and have the old contents.
	existsWithContent(backupFile(dir), b, t)

	fileCount(dir, 2, t)
}

func TestDailyRotateOnDateChange(t *testing.T) {
	setFakeTime(time.Date(2026, 6, 8, 23, 59, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyRotateOnDateChange", t)
	defer removeAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  100,
		Daily:    true,
	}
	defer closeLogger(l)

	b := []byte("before midnight")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	setFakeTime(time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC))

	b2 := []byte("after midnight")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	existsWithContent(filename, b2, t)
	existsWithContent(backupFileAt(dir, fakeTime()), b, t)
	fileCount(dir, 2, t)
}

func TestDailyDoesNotRotateWithinSameDay(t *testing.T) {
	setFakeTime(time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyDoesNotRotateWithinSameDay", t)
	defer removeAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  100,
		Daily:    true,
	}
	defer closeLogger(l)

	b := []byte("morning")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	setFakeTime(time.Date(2026, 6, 8, 23, 59, 59, 0, time.UTC))

	b2 := []byte("night")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	existsWithContent(filename, append(b, b2...), t)
	fileCount(dir, 1, t)
}

func TestDailyRotateUsesLocalTimeBoundary(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.FixedZone("logrotate-test-local", 8*60*60)
	defer func() {
		time.Local = oldLocal
	}()

	setFakeTime(time.Date(2026, 6, 8, 23, 59, 0, 0, time.Local))

	dir := makeTempDir("TestDailyRotateUsesLocalTimeBoundary", t)
	defer removeAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename:  filename,
		MaxSize:   100,
		LocalTime: true,
		Daily:     true,
	}
	defer closeLogger(l)

	b := []byte("local before midnight")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	setFakeTime(time.Date(2026, 6, 9, 0, 0, 0, 0, time.Local))

	b2 := []byte("local after midnight")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	existsWithContent(filename, b2, t)
	existsWithContent(backupFileLocalAt(dir, fakeTime()), b, t)
	fileCount(dir, 2, t)
}

func TestDailyRotateExistingOldFileOnFirstWrite(t *testing.T) {
	setFakeTime(time.Date(2026, 6, 9, 8, 0, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyRotateExistingOldFileOnFirstWrite", t)
	defer removeAll(dir)

	filename := logFile(dir)
	old := []byte("yesterday")
	err := os.WriteFile(filename, old, 0o644)
	isNil(err, t)

	yesterday := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	err = os.Chtimes(filename, yesterday, yesterday)
	isNil(err, t)

	l := &Logger{
		Filename: filename,
		MaxSize:  100,
		Daily:    true,
	}
	defer closeLogger(l)

	b := []byte("today")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	existsWithContent(backupFileAt(dir, fakeTime()), old, t)
	fileCount(dir, 2, t)
}

func TestDailyAndSizeRotationBothApply(t *testing.T) {
	megabyte = 1
	setFakeTime(time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyAndSizeRotationBothApply", t)
	defer removeAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		Daily:    true,
	}
	defer closeLogger(l)

	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	sizeRotateTime := fakeTime()
	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	existsWithContent(filename, b2, t)
	existsWithContent(backupFileAt(dir, sizeRotateTime), b, t)

	setFakeTime(time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC))

	b3 := []byte("bar!")
	n, err = l.Write(b3)
	isNil(err, t)
	equals(len(b3), n, t)

	existsWithContent(filename, b3, t)
	existsWithContent(backupFileAt(dir, fakeTime()), b2, t)
	fileCount(dir, 3, t)
}

func TestDailyConcurrentCrossDayRotation(t *testing.T) {
	megabyte = 1
	setFakeTime(time.Date(2026, 6, 8, 23, 59, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyConcurrentCrossDayRotation", t)
	defer removeAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  100000,
		Daily:    true,
	}
	defer closeLogger(l)

	before := []byte("before\n")
	n, err := l.Write(before)
	isNil(err, t)
	equals(len(before), n, t)

	setFakeTime(time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC))

	const goroutines = 32
	const writesPerG = 50

	var wg sync.WaitGroup
	var totalBytes atomic.Int64
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(i int) {
			defer wg.Done()
			line := []byte(fmt.Sprintf("g%d-write\n", i))
			for w := 0; w < writesPerG; w++ {
				n, werr := l.Write(line)
				if werr != nil {
					t.Errorf("write error: %v", werr)
					return
				}
				totalBytes.Add(int64(n))
			}
		}(g)
	}
	wg.Wait()

	if totalBytes.Load() == 0 {
		t.Fatal("no bytes written")
	}

	existsWithContent(backupFileAt(dir, fakeTime()), before, t)
	fileCount(dir, 2, t)
}

func TestDailyFilenameWritesDatedCurrentFile(t *testing.T) {
	setFakeTime(time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyFilenameWritesDatedCurrentFile", t)
	defer removeAll(dir)

	l := &Logger{
		Filename:      logFile(dir),
		MaxSize:       100,
		DailyFilename: true,
	}
	defer closeLogger(l)

	b := []byte("today")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(dailyLogFileAt(dir, fakeTime()), b, t)
	notExist(logFile(dir), t)
	fileCount(dir, 1, t)
}

func TestDailyFilenameSwitchesOnDateChange(t *testing.T) {
	setFakeTime(time.Date(2026, 6, 8, 23, 59, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyFilenameSwitchesOnDateChange", t)
	defer removeAll(dir)

	l := &Logger{
		Filename:      logFile(dir),
		MaxSize:       100,
		DailyFilename: true,
	}
	defer closeLogger(l)

	b := []byte("day one")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	dayOne := fakeTime()
	setFakeTime(time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC))

	b2 := []byte("day two")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	existsWithContent(dailyLogFileAt(dir, dayOne), b, t)
	existsWithContent(dailyLogFileAt(dir, fakeTime()), b2, t)
	fileCount(dir, 2, t)
}

func TestDailyFilenameAndSizeRotationBothApply(t *testing.T) {
	megabyte = 1
	setFakeTime(time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyFilenameAndSizeRotationBothApply", t)
	defer removeAll(dir)

	l := &Logger{
		Filename:      logFile(dir),
		MaxSize:       10,
		DailyFilename: true,
	}
	defer closeLogger(l)

	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	rotateTime := fakeTime()
	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	existsWithContent(dailyLogFileAt(dir, fakeTime()), b2, t)
	existsWithContent(dailyBackupFileAt(dir, rotateTime), b, t)
	fileCount(dir, 2, t)
}

func TestDailyFilenameCleansAcrossDates(t *testing.T) {
	megabyte = 1
	setFakeTime(time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyFilenameCleansAcrossDates", t)
	defer removeAll(dir)

	oldest := dailyLogFileForDate(dir, "2026-06-07")
	old := dailyLogFileForDate(dir, "2026-06-08")
	recent := dailyLogFileForDate(dir, "2026-06-09")
	for _, name := range []string{oldest, old, recent} {
		err := os.WriteFile(name, []byte("old"), 0o644)
		isNil(err, t)
	}

	l := &Logger{
		Filename:      logFile(dir),
		MaxSize:       100,
		MaxBackups:    2,
		DailyFilename: true,
	}
	defer closeLogger(l)

	b := []byte("current")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)
	isNil(l.Close(), t)

	notExist(oldest, t)
	exists(old, t)
	exists(recent, t)
	existsWithContent(dailyLogFileAt(dir, fakeTime()), b, t)
	fileCount(dir, 3, t)
}

func TestDailyFilenameMaxAgeCleansAcrossDates(t *testing.T) {
	tests := []struct {
		name           string
		existingFiles  map[string][]byte
		wantRemoved    []string
		wantPreserved  []string
		wantFileCount  int
		currentContent []byte
	}{
		{
			name: "removes expired plain and compressed daily files",
			existingFiles: map[string][]byte{
				"2026-06-01":    []byte("expired"),
				"2026-06-02.gz": []byte("expired gz"),
				"2026-06-09":    []byte("recent"),
			},
			wantRemoved:    []string{"2026-06-01", "2026-06-02.gz"},
			wantPreserved:  []string{"2026-06-09"},
			wantFileCount:  2,
			currentContent: []byte("current"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			megabyte = 1
			setFakeTime(time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC))

			dir := makeTempDir("TestDailyFilenameMaxAgeCleansAcrossDates", t)
			defer removeAll(dir)

			for suffix, content := range tt.existingFiles {
				name := dailyLogFileForDate(dir, strings.TrimSuffix(suffix, compressSuffix))
				if strings.HasSuffix(suffix, compressSuffix) {
					name += compressSuffix
				}
				err := os.WriteFile(name, content, 0o644)
				isNil(err, t)
			}

			l := &Logger{
				Filename:      logFile(dir),
				MaxSize:       100,
				MaxAge:        2,
				DailyFilename: true,
			}

			n, err := l.Write(tt.currentContent)
			isNil(err, t)
			equals(len(tt.currentContent), n, t)
			isNil(l.Close(), t)

			for _, suffix := range tt.wantRemoved {
				name := dailyLogFileForDate(dir, strings.TrimSuffix(suffix, compressSuffix))
				if strings.HasSuffix(suffix, compressSuffix) {
					name += compressSuffix
				}
				notExist(name, t)
			}
			for _, suffix := range tt.wantPreserved {
				name := dailyLogFileForDate(dir, strings.TrimSuffix(suffix, compressSuffix))
				if strings.HasSuffix(suffix, compressSuffix) {
					name += compressSuffix
				}
				exists(name, t)
			}
			existsWithContent(dailyLogFileAt(dir, fakeTime()), tt.currentContent, t)
			fileCount(dir, tt.wantFileCount, t)
		})
	}
}

// TestCloseWaitsForDailyCleanup 验证 Close 会等待后台清理完成。
// 进程启动时已存在远超 MaxAge 的历史日切文件，首次 Write 触发清理后立即 Close，
// 应能确定性断言过期文件已删除——无需 sleep。这等价于 logger
// dailyWriteSyncer 的"启动即回收积压 + Close 等待在途 sweep"语义。
func TestCloseWaitsForDailyCleanup(t *testing.T) {
	megabyte = 1
	setFakeTime(time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC))

	dir := makeTempDir("TestCloseWaitsForDailyCleanup", t)
	defer removeAll(dir)

	expired := dailyLogFileForDate(dir, "2026-01-01")
	err := os.WriteFile(expired, []byte("expired"), 0o644)
	isNil(err, t)

	l := &Logger{
		Filename:      logFile(dir),
		MaxSize:       100,
		MaxAge:        7,
		DailyFilename: true,
	}

	b := []byte("current")
	_, err = l.Write(b)
	isNil(err, t)

	// 不 sleep：Close 等待后台清理结束后才返回。
	isNil(l.Close(), t)

	notExist(expired, t)
	existsWithContent(dailyLogFileAt(dir, fakeTime()), b, t)
}

// TestDailyFilenameNoCleanupWhenLimitsZero 验证 MaxAge 与 MaxBackups 都为 0、
// 且未启用压缩时，不删除任何历史日切文件。等价于 logger 的
// TestCleanDailyFilesDisabledWhenBothZero。
func TestDailyFilenameNoCleanupWhenLimitsZero(t *testing.T) {
	megabyte = 1
	setFakeTime(time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyFilenameNoCleanupWhenLimitsZero", t)
	defer removeAll(dir)

	old := dailyLogFileForDate(dir, "2020-01-01")
	err := os.WriteFile(old, []byte("old"), 0o644)
	isNil(err, t)

	l := &Logger{
		Filename:      logFile(dir),
		MaxSize:       100,
		DailyFilename: true,
	}

	b := []byte("current")
	_, err = l.Write(b)
	isNil(err, t)
	isNil(l.Close(), t)

	exists(old, t) // 两项均为 0：清理关闭，不删任何文件
}

func TestDailyFilenameKeepsCurrentEvenIfOld(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "current daily file is excluded from cleanup"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			megabyte = 1
			setFakeTime(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC))

			dir := makeTempDir("TestDailyFilenameKeepsCurrentEvenIfOld", t)
			defer removeAll(dir)

			current := dailyLogFileAt(dir, fakeTime())
			err := os.WriteFile(current, []byte("old current"), 0o644)
			isNil(err, t)

			oldTime := fakeTime().Add(-100 * 24 * time.Hour)
			err = os.Chtimes(current, oldTime, oldTime)
			isNil(err, t)

			expired := dailyLogFileForDate(dir, "2020-01-01")
			err = os.WriteFile(expired, []byte("expired"), 0o644)
			isNil(err, t)

			l := &Logger{
				Filename:      logFile(dir),
				MaxSize:       100,
				MaxAge:        1,
				DailyFilename: true,
			}

			b := []byte("new")
			n, err := l.Write(b)
			isNil(err, t)
			equals(len(b), n, t)
			isNil(l.Close(), t)

			exists(current, t)
			notExist(expired, t)
		})
	}
}

func TestDailyFilenameCompressesSizeBackups(t *testing.T) {
	megabyte = 1
	setFakeTime(time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyFilenameCompressesSizeBackups", t)
	defer removeAll(dir)

	l := &Logger{
		Filename:      logFile(dir),
		MaxSize:       10,
		Compress:      true,
		DailyFilename: true,
	}
	defer closeLogger(l)

	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	rotateTime := fakeTime()
	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)
	isNil(l.Close(), t)

	existsWithContent(dailyLogFileAt(dir, fakeTime()), b2, t)
	existsWithContent(dailyBackupFileAt(dir, rotateTime)+compressSuffix, gzippedContent(b, t), t)
	notExist(dailyBackupFileAt(dir, rotateTime), t)
	fileCount(dir, 2, t)
}

func TestCloseWaitsForCompression(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "close waits for pending compression"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			megabyte = 1

			dir := makeTempDir("TestCloseWaitsForCompression", t)
			defer removeAll(dir)

			filename := logFile(dir)
			l := &Logger{
				Compress: true,
				Filename: filename,
				MaxSize:  10,
			}

			b := []byte("boo!")
			n, err := l.Write(b)
			isNil(err, t)
			equals(len(b), n, t)

			newFakeTime()
			err = l.Rotate()
			isNil(err, t)

			isNil(l.Close(), t)

			existsWithContent(backupFile(dir)+compressSuffix, gzippedContent(b, t), t)
			notExist(backupFile(dir), t)
			existsWithContent(filename, []byte{}, t)
			fileCount(dir, 2, t)
		})
	}
}

func TestDailyFilenameConcurrentCrossDaySwitch(t *testing.T) {
	megabyte = 1
	setFakeTime(time.Date(2026, 6, 8, 23, 59, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyFilenameConcurrentCrossDaySwitch", t)
	defer removeAll(dir)

	l := &Logger{
		Filename:      logFile(dir),
		MaxSize:       100000,
		DailyFilename: true,
	}
	defer closeLogger(l)

	before := []byte("before\n")
	n, err := l.Write(before)
	isNil(err, t)
	equals(len(before), n, t)

	dayOne := fakeTime()
	setFakeTime(time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC))

	const goroutines = 32
	const writesPerG = 50

	var wg sync.WaitGroup
	var totalBytes atomic.Int64
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(i int) {
			defer wg.Done()
			line := []byte(fmt.Sprintf("daily-file-g%d-write\n", i))
			for w := 0; w < writesPerG; w++ {
				n, werr := l.Write(line)
				if werr != nil {
					t.Errorf("write error: %v", werr)
					return
				}
				totalBytes.Add(int64(n))
			}
		}(g)
	}
	wg.Wait()

	if totalBytes.Load() == 0 {
		t.Fatal("no bytes written")
	}

	existsWithContent(dailyLogFileAt(dir, dayOne), before, t)
	exists(dailyLogFileAt(dir, fakeTime()), t)
	fileCount(dir, 2, t)
}

func TestFirstWriteRotate(t *testing.T) {
	megabyte = 1
	dir := makeTempDir("TestFirstWriteRotate", t)
	defer removeAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
	}
	defer closeLogger(l)

	start := []byte("boooooo!")
	err := os.WriteFile(filename, start, 0o600)
	isNil(err, t)

	newFakeTime()

	// this would make us rotate
	b := []byte("fooo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	existsWithContent(backupFile(dir), start, t)

	fileCount(dir, 2, t)
}

func TestMaxBackups(t *testing.T) {
	megabyte = 1
	dir := makeTempDir("TestMaxBackups", t)
	defer removeAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename:   filename,
		MaxSize:    10,
		MaxBackups: 1,
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	fileCount(dir, 1, t)

	newFakeTime()

	// this will put us over the max
	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	// this will use the new fake time
	secondFilename := backupFile(dir)
	existsWithContent(secondFilename, b, t)

	// make sure the old file still exists with the same content.
	existsWithContent(filename, b2, t)

	fileCount(dir, 2, t)

	newFakeTime()

	// this will make us rotate again
	b3 := []byte("baaaaaar!")
	n, err = l.Write(b3)
	isNil(err, t)
	equals(len(b3), n, t)

	// this will use the new fake time
	thirdFilename := backupFile(dir)
	existsWithContent(thirdFilename, b2, t)

	existsWithContent(filename, b3, t)
	isNil(l.Close(), t)

	// should only have two files in the dir still
	fileCount(dir, 2, t)

	// second file name should still exist
	existsWithContent(thirdFilename, b2, t)

	// should have deleted the first backup
	notExist(secondFilename, t)

	// now test that we don't delete directories or non-logfile files

	newFakeTime()

	// create a file that is close to but different from the logfile name.
	// It shouldn't get caught by our deletion filters.
	notlogfile := logFile(dir) + ".foo"
	err = os.WriteFile(notlogfile, []byte("data"), 0o644)
	isNil(err, t)

	// Make a directory that exactly matches our log file filters... it still
	// shouldn't get caught by the deletion filter since it's a directory.
	notlogfiledir := backupFile(dir)
	err = os.Mkdir(notlogfiledir, 0o700)
	isNil(err, t)

	newFakeTime()

	// this will use the new fake time
	fourthFilename := backupFile(dir)

	// Create a log file that is/was being compressed - this should
	// not be counted since both the compressed and the uncompressed
	// log files still exist.
	compLogFile := fourthFilename + compressSuffix
	err = os.WriteFile(compLogFile, []byte("compress"), 0o644)
	isNil(err, t)

	// this will make us rotate again
	b4 := []byte("baaaaaaz!")
	n, err = l.Write(b4)
	isNil(err, t)
	equals(len(b4), n, t)

	existsWithContent(fourthFilename, b3, t)
	existsWithContent(fourthFilename+compressSuffix, []byte("compress"), t)
	isNil(l.Close(), t)

	// We should have four things in the directory now - the 2 log files, the
	// not log file, and the directory
	fileCount(dir, 5, t)

	// third file name should still exist
	existsWithContent(filename, b4, t)

	existsWithContent(fourthFilename, b3, t)

	// should have deleted the first filename
	notExist(thirdFilename, t)

	// the not-a-logfile should still exist
	exists(notlogfile, t)

	// the directory
	exists(notlogfiledir, t)
}

func TestCleanupExistingBackups(t *testing.T) {
	// test that if we start with more backup files than we're supposed to have
	// in total, that extra ones get cleaned up when we rotate.
	megabyte = 1

	dir := makeTempDir("TestCleanupExistingBackups", t)
	defer removeAll(dir)

	// make 3 backup files

	data := []byte("data")
	backup := backupFile(dir)
	err := os.WriteFile(backup, data, 0o644)
	isNil(err, t)

	newFakeTime()

	backup = backupFile(dir)
	err = os.WriteFile(backup+compressSuffix, data, 0o644)
	isNil(err, t)

	newFakeTime()

	backup = backupFile(dir)
	err = os.WriteFile(backup, data, 0o644)
	isNil(err, t)

	// now create a primary log file with some data
	filename := logFile(dir)
	err = os.WriteFile(filename, data, 0o644)
	isNil(err, t)

	l := &Logger{
		Filename:   filename,
		MaxSize:    10,
		MaxBackups: 1,
	}
	defer closeLogger(l)

	newFakeTime()

	b2 := []byte("foooooo!")
	n, err := l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)
	isNil(l.Close(), t)

	// now we should only have 2 files left - the primary and one backup
	fileCount(dir, 2, t)
}

func TestMaxAge(t *testing.T) {
	megabyte = 1

	dir := makeTempDir("TestMaxAge", t)
	defer removeAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		MaxAge:   1,
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	fileCount(dir, 1, t)

	// two days later
	newFakeTime()

	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)
	existsWithContent(backupFile(dir), b, t)
	isNil(l.Close(), t)

	// We should still have 2 log files, since the most recent backup was just
	// created.
	fileCount(dir, 2, t)

	existsWithContent(filename, b2, t)

	// we should have deleted the old file due to being too old
	existsWithContent(backupFile(dir), b, t)

	// two days later
	newFakeTime()

	b3 := []byte("baaaaar!")
	n, err = l.Write(b3)
	isNil(err, t)
	equals(len(b3), n, t)
	existsWithContent(backupFile(dir), b2, t)
	isNil(l.Close(), t)

	// We should have 2 log files - the main log file, and the most recent
	// backup.  The earlier backup is past the cutoff and should be gone.
	fileCount(dir, 2, t)

	existsWithContent(filename, b3, t)

	// we should have deleted the old file due to being too old
	existsWithContent(backupFile(dir), b2, t)
}

func TestOldLogFiles(t *testing.T) {
	megabyte = 1

	dir := makeTempDir("TestOldLogFiles", t)
	defer removeAll(dir)

	filename := logFile(dir)
	data := []byte("data")
	err := os.WriteFile(filename, data, 0o7)
	isNil(err, t)

	// This gives us a time with the same precision as the time we get from the
	// timestamp in the name.
	t1, err := time.Parse(backupTimeFormat, fakeTime().UTC().Format(backupTimeFormat))
	isNil(err, t)

	backup := backupFile(dir)
	err = os.WriteFile(backup, data, 0o7)
	isNil(err, t)

	newFakeTime()

	t2, err := time.Parse(backupTimeFormat, fakeTime().UTC().Format(backupTimeFormat))
	isNil(err, t)

	backup2 := backupFile(dir)
	err = os.WriteFile(backup2, data, 0o7)
	isNil(err, t)

	l := &Logger{Filename: filename}
	files, err := l.oldLogFiles()
	isNil(err, t)
	equals(2, len(files), t)

	// should be sorted by newest file first, which would be t2
	equals(t2, files[0].timestamp, t)
	equals(t1, files[1].timestamp, t)
}

func TestTimeFromName(t *testing.T) {
	l := &Logger{Filename: "/var/log/myfoo/foo.log"}
	prefix, ext := l.prefixAndExt()

	tests := []struct {
		filename string
		want     time.Time
		wantErr  bool
	}{
		{"foo-2014-05-04T14-44-33.555.log", time.Date(2014, 5, 4, 14, 44, 33, 555000000, time.UTC), false},
		{"foo-2014-05-04T14-44-33.555", time.Time{}, true},
		{"2014-05-04T14-44-33.555.log", time.Time{}, true},
		{"foo.log", time.Time{}, true},
	}

	for _, test := range tests {
		got, err := l.timeFromName(test.filename, prefix, ext)
		equals(got, test.want, t)
		equals(err != nil, test.wantErr, t)
	}
}

func TestLocalTime(t *testing.T) {
	megabyte = 1

	dir := makeTempDir("TestLocalTime", t)
	defer removeAll(dir)

	l := &Logger{
		Filename:  logFile(dir),
		MaxSize:   10,
		LocalTime: true,
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	b2 := []byte("fooooooo!")
	n2, err := l.Write(b2)
	isNil(err, t)
	equals(len(b2), n2, t)

	existsWithContent(logFile(dir), b2, t)
	existsWithContent(backupFileLocal(dir), b, t)
}

func TestRotate(t *testing.T) {
	dir := makeTempDir("TestRotate", t)
	defer removeAll(dir)

	filename := logFile(dir)

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

	existsWithContent(filename, b, t)
	fileCount(dir, 1, t)

	newFakeTime()

	err = l.Rotate()
	isNil(err, t)
	isNil(l.Close(), t)

	filename2 := backupFile(dir)
	existsWithContent(filename2, b, t)
	existsWithContent(filename, []byte{}, t)
	fileCount(dir, 2, t)
	newFakeTime()

	err = l.Rotate()
	isNil(err, t)
	isNil(l.Close(), t)

	filename3 := backupFile(dir)
	existsWithContent(filename3, []byte{}, t)
	existsWithContent(filename, []byte{}, t)
	fileCount(dir, 2, t)

	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	// this will use the new fake time
	existsWithContent(filename, b2, t)
}

func TestCompressOnRotate(t *testing.T) {
	megabyte = 1

	dir := makeTempDir("TestCompressOnRotate", t)
	defer removeAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Compress: true,
		Filename: filename,
		MaxSize:  10,
	}
	defer closeLogger(l)
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	fileCount(dir, 1, t)

	newFakeTime()

	err = l.Rotate()
	isNil(err, t)

	// the old logfile should be moved aside and the main logfile should have
	// nothing in it.
	existsWithContent(filename, []byte{}, t)
	isNil(l.Close(), t)

	// a compressed version of the log file should now exist and the original
	// should have been removed.
	existsWithContent(backupFile(dir)+compressSuffix, gzippedContent(b, t), t)
	notExist(backupFile(dir), t)

	fileCount(dir, 2, t)
}

func TestCompressOnResume(t *testing.T) {
	megabyte = 1

	dir := makeTempDir("TestCompressOnResume", t)
	defer removeAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Compress: true,
		Filename: filename,
		MaxSize:  10,
	}
	defer closeLogger(l)

	// Create a backup file and empty "compressed" file.
	filename2 := backupFile(dir)
	b := []byte("foo!")
	err := os.WriteFile(filename2, b, 0o644)
	isNil(err, t)
	err = os.WriteFile(filename2+compressSuffix, []byte{}, 0o644)
	isNil(err, t)

	newFakeTime()

	b2 := []byte("boo!")
	n, err := l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)
	existsWithContent(filename, b2, t)
	isNil(l.Close(), t)

	// The write should have started the compression - a compressed version of
	// the log file should now exist and the original should have been removed.
	existsWithContent(filename2+compressSuffix, gzippedContent(b, t), t)
	notExist(filename2, t)

	fileCount(dir, 2, t)
}

func TestJSONTags(t *testing.T) {
	tests := []struct {
		name  string
		field string
		tag   string
	}{
		{name: "filename", field: "Filename", tag: "filename"},
		{name: "max size", field: "MaxSize", tag: "maxsize"},
		{name: "max age", field: "MaxAge", tag: "maxage"},
		{name: "max backups", field: "MaxBackups", tag: "maxbackups"},
		{name: "local time", field: "LocalTime", tag: "localtime"},
		{name: "compress", field: "Compress", tag: "compress"},
		{name: "daily", field: "Daily", tag: "daily"},
		{name: "daily filename", field: "DailyFilename", tag: "dailyfilename"},
	}

	typ := reflect.TypeOf(Logger{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field, ok := typ.FieldByName(tt.field)
			assert(ok, t, "missing field %s", tt.field)
			equals(tt.tag, field.Tag.Get("json"), t)
		})
	}
}

// makeTempDir creates a file with a semi-unique name in the OS temp directory.
// It should be based on the name of the test, to keep parallel tests from
// colliding, and must be cleaned up after the test is finished.
func makeTempDir(name string, t testing.TB) string {
	dir := time.Now().Format(name + backupTimeFormat)
	dir = filepath.Join(os.TempDir(), dir)
	isNilUp(os.Mkdir(dir, 0o700), t, 1)
	return dir
}

// existsWithContent checks that the given file exists and has the correct content.
func existsWithContent(path string, content []byte, t testing.TB) {
	info, err := os.Stat(path)
	isNilUp(err, t, 1)
	equalsUp(int64(len(content)), info.Size(), t, 1)

	b, err := os.ReadFile(path)
	isNilUp(err, t, 1)
	equalsUp(content, b, t, 1)
}

func gzippedContent(content []byte, t testing.TB) []byte {
	bc := new(bytes.Buffer)
	gz := gzip.NewWriter(bc)
	_, err := gz.Write(content)
	isNilUp(err, t, 1)
	err = gz.Close()
	isNilUp(err, t, 1)
	return bc.Bytes()
}

// logFile returns the log file name in the given directory for the current fake
// time.
func logFile(dir string) string {
	return filepath.Join(dir, "foobar.log")
}

func backupFile(dir string) string {
	return filepath.Join(dir, "foobar-"+fakeTime().UTC().Format(backupTimeFormat)+".log")
}

func backupFileLocal(dir string) string {
	return filepath.Join(dir, "foobar-"+fakeTime().Format(backupTimeFormat)+".log")
}

func backupFileAt(dir string, t time.Time) string {
	return filepath.Join(dir, "foobar-"+t.UTC().Format(backupTimeFormat)+".log")
}

func backupFileLocalAt(dir string, t time.Time) string {
	return filepath.Join(dir, "foobar-"+t.Format(backupTimeFormat)+".log")
}

func dailyLogFileAt(dir string, t time.Time) string {
	return dailyLogFileForDate(dir, t.UTC().Format(dailyNameFormat))
}

func dailyLogFileForDate(dir, date string) string {
	return filepath.Join(dir, "foobar-"+date+".log")
}

func dailyBackupFileAt(dir string, t time.Time) string {
	day := t.UTC().Format(dailyNameFormat)
	return filepath.Join(dir, "foobar-"+day+"-"+t.UTC().Format(backupTimeFormat)+".log")
}

// fileCount checks that the number of files in the directory is exp.
func fileCount(dir string, exp int, t testing.TB) {
	files, err := os.ReadDir(dir)
	isNilUp(err, t, 1)
	// Make sure no other files were created.
	equalsUp(exp, len(files), t, 1)
}

// newFakeTime sets the fake "current time" to two days later.
func newFakeTime() {
	fakeTimeMu.Lock()
	defer fakeTimeMu.Unlock()
	fakeCurrentTime = fakeCurrentTime.Add(time.Hour * 24 * 2)
}

func notExist(path string, t testing.TB) {
	_, err := os.Stat(path)
	assertUp(os.IsNotExist(err), t, 1, "expected to get os.IsNotExist, but instead got %v", err)
}

func exists(path string, t testing.TB) {
	_, err := os.Stat(path)
	assertUp(err == nil, t, 1, "expected file to exist, but got error from os.Stat: %v", err)
}

func removeAll(path string) {
	_ = os.RemoveAll(path)
}

func removeFile(path string) {
	_ = os.Remove(path)
}

func closeLogger(l *Logger) {
	_ = l.Close()
}

func waitForMillWorkers(want int, t testing.TB) {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if millWorkerCount() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	equals(want, millWorkerCount(), t)
}

func millWorkerCount() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return bytes.Count(buf[:n], []byte("github.com/gtkit/logrotate.millRun"))
}
