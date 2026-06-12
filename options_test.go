package logrotate

import (
	"errors"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	now := time.Date(2026, 6, 12, 10, 30, 0, 0, time.UTC)
	location := time.FixedZone("logrotate-test", 8*60*60)
	onError := func(error) {}

	type wantConfig struct {
		filename      string
		maxSize       int
		maxAge        int
		maxBackups    int
		localTime     bool
		compress      bool
		daily         bool
		dailyFilename bool
		location      *time.Location
	}

	tests := []struct {
		name string
		opts []Option
		want wantConfig
	}{
		{
			name: "defaults match zero logger",
		},
		{
			name: "applies options",
			opts: []Option{
				WithFilename("app.log"),
				WithMaxSize(500),
				WithMaxAge(30),
				WithMaxBackups(7),
				WithLocalTime(true),
				WithCompress(true),
				WithDaily(true),
				WithDailyFilename(true),
				WithNow(func() time.Time { return now }),
				WithLocation(location),
				WithOnError(onError),
			},
			want: wantConfig{
				filename:      "app.log",
				maxSize:       500,
				maxAge:        30,
				maxBackups:    7,
				localTime:     true,
				compress:      true,
				daily:         true,
				dailyFilename: true,
				location:      location,
			},
		},
		{
			name: "ignores nil option",
			opts: []Option{nil, WithFilename("app.log")},
			want: wantConfig{filename: "app.log"},
		},
		{
			name: "normalizes negative numeric values",
			opts: []Option{
				WithMaxSize(-1),
				WithMaxAge(-2),
				WithMaxBackups(-3),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := New(tt.opts...)

			equals(tt.want.filename, got.Filename, t)
			equals(tt.want.maxSize, got.MaxSize, t)
			equals(tt.want.maxAge, got.MaxAge, t)
			equals(tt.want.maxBackups, got.MaxBackups, t)
			equals(tt.want.localTime, got.LocalTime, t)
			equals(tt.want.compress, got.Compress, t)
			equals(tt.want.daily, got.Daily, t)
			equals(tt.want.dailyFilename, got.DailyFilename, t)
			equals(tt.want.location, got.Location, t)

			if tt.name == "applies options" {
				equals(now, got.Now(), t)
				got.OnError(errors.New("test"))
			}
		})
	}
}

func TestNewLoggerWritesFile(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
	}{
		{
			name: "constructed logger writes",
			opts: []Option{WithMaxSize(100)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := makeTempDir("TestNewLoggerWritesFile", t)
			defer removeAll(dir)

			l := New(append([]Option{WithFilename(logFile(dir))}, tt.opts...)...)
			defer closeLogger(l)

			b := []byte("constructed")
			n, err := l.Write(b)
			isNil(err, t)
			equals(len(b), n, t)
			existsWithContent(logFile(dir), b, t)
		})
	}
}
