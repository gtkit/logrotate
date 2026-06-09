package logrotate

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	<-time.After(100 * time.Millisecond)

	notExist(oldest, t)
	exists(old, t)
	exists(recent, t)
	existsWithContent(dailyLogFileAt(dir, fakeTime()), b, t)
	fileCount(dir, 3, t)
}

func TestDailyFilenameMaxAgeCleansAcrossDates(t *testing.T) {
	megabyte = 1
	setFakeTime(time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC))

	dir := makeTempDir("TestDailyFilenameMaxAgeCleansAcrossDates", t)
	defer removeAll(dir)

	expired := dailyLogFileForDate(dir, "2026-06-01")
	recent := dailyLogFileForDate(dir, "2026-06-09")
	err := os.WriteFile(expired, []byte("expired"), 0o644)
	isNil(err, t)
	err = os.WriteFile(recent, []byte("recent"), 0o644)
	isNil(err, t)

	l := &Logger{
		Filename:      logFile(dir),
		MaxSize:       100,
		MaxAge:        2,
		DailyFilename: true,
	}
	defer closeLogger(l)

	b := []byte("current")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)
	<-time.After(100 * time.Millisecond)

	notExist(expired, t)
	exists(recent, t)
	existsWithContent(dailyLogFileAt(dir, fakeTime()), b, t)
	fileCount(dir, 2, t)
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
	<-time.After(300 * time.Millisecond)

	bc := new(bytes.Buffer)
	gz := gzip.NewWriter(bc)
	_, err = gz.Write(b)
	isNil(err, t)
	err = gz.Close()
	isNil(err, t)

	existsWithContent(dailyLogFileAt(dir, fakeTime()), b2, t)
	existsWithContent(dailyBackupFileAt(dir, rotateTime)+compressSuffix, bc.Bytes(), t)
	notExist(dailyBackupFileAt(dir, rotateTime), t)
	fileCount(dir, 2, t)
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

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(time.Millisecond * 10)

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

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(time.Millisecond * 10)

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

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(time.Millisecond * 10)

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

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(10 * time.Millisecond)

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

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(10 * time.Millisecond)

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

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(10 * time.Millisecond)

	filename2 := backupFile(dir)
	existsWithContent(filename2, b, t)
	existsWithContent(filename, []byte{}, t)
	fileCount(dir, 2, t)
	newFakeTime()

	err = l.Rotate()
	isNil(err, t)

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(10 * time.Millisecond)

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

	// we need to wait a little bit since the files get compressed on a different
	// goroutine.
	<-time.After(300 * time.Millisecond)

	// a compressed version of the log file should now exist and the original
	// should have been removed.
	bc := new(bytes.Buffer)
	gz := gzip.NewWriter(bc)
	_, err = gz.Write(b)
	isNil(err, t)
	err = gz.Close()
	isNil(err, t)
	existsWithContent(backupFile(dir)+compressSuffix, bc.Bytes(), t)
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

	// we need to wait a little bit since the files get compressed on a different
	// goroutine.
	<-time.After(300 * time.Millisecond)

	// The write should have started the compression - a compressed version of
	// the log file should now exist and the original should have been removed.
	bc := new(bytes.Buffer)
	gz := gzip.NewWriter(bc)
	_, err = gz.Write(b)
	isNil(err, t)
	err = gz.Close()
	isNil(err, t)
	existsWithContent(filename2+compressSuffix, bc.Bytes(), t)
	notExist(filename2, t)

	fileCount(dir, 2, t)
}

func TestJson(t *testing.T) {
	data := []byte(`
{
	"filename": "foo",
	"maxsize": 5,
	"maxage": 10,
	"maxbackups": 3,
	"localtime": true,
	"compress": true,
	"daily": true,
	"dailyfilename": true
}`[1:])

	l := Logger{}
	err := json.Unmarshal(data, &l)
	isNil(err, t)
	equals("foo", l.Filename, t)
	equals(5, l.MaxSize, t)
	equals(10, l.MaxAge, t)
	equals(3, l.MaxBackups, t)
	equals(true, l.LocalTime, t)
	equals(true, l.Compress, t)
	equals(true, l.Daily, t)
	equals(true, l.DailyFilename, t)
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
