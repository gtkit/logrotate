package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkWrite(b *testing.B) {
	l := New(
		WithFilename(logFile(b.TempDir())),
		WithMaxSize(100),
	)
	defer closeLogger(l)

	line := []byte("benchmark write line\n")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := l.Write(line); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteParallel(b *testing.B) {
	l := New(
		WithFilename(logFile(b.TempDir())),
		WithMaxSize(100),
	)
	defer closeLogger(l)

	line := []byte("benchmark parallel write line\n")
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := l.Write(line); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkRotateBySize(b *testing.B) {
	oldMegabyte := megabyte
	megabyte = 1
	defer func() { megabyte = oldMegabyte }()

	l := New(
		WithFilename(logFile(b.TempDir())),
		WithMaxSize(32),
		WithNow(benchmarkClock()),
	)
	defer closeLogger(l)

	line := []byte("benchmark rotate line")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := l.Write(line); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCleanupManyBackups(b *testing.B) {
	dir := b.TempDir()
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		t := now.Add(-time.Duration(i) * time.Hour)
		name := filepathBackup(dir, t)
		if err := os.WriteFile(name, []byte("old log"), 0o644); err != nil {
			b.Fatal(err)
		}
	}

	l := New(
		WithFilename(logFile(dir)),
		WithMaxBackups(500),
		WithNow(func() time.Time { return now }),
	)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := l.Cleanup(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompressLogFile(b *testing.B) {
	dir := b.TempDir()
	src := filepathBackup(dir, time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC))
	content := []byte("benchmark compression line\n")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		runSrc := fmt.Sprintf("%s-%d", src, b.N)
		if err := os.WriteFile(runSrc, content, 0o644); err != nil {
			b.Fatal(err)
		}
		if err := compressLogFile(runSrc, runSrc+compressSuffix); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkClock() func() time.Time {
	var n int64
	base := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Millisecond)
	}
}

func filepathBackup(dir string, t time.Time) string {
	return filepath.Join(dir, fmt.Sprintf("foobar-%s.log", t.UTC().Format(backupTimeFormat)))
}
