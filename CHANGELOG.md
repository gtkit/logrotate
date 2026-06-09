# Changelog

本项目所有重要变更都会记录在此文件，遵循 [Keep a Changelog 1.1.0](https://keepachangelog.com/zh-CN/1.1.0/) 规范。

## [Unreleased]

### Added
- 新增 `Sync()` 方法，将当前日志文件的内核缓冲刷写到磁盘，满足 `zapcore.WriteSyncer` 等需要 `Sync` 的接口。
- 新增 `Open()` 方法，用于启动时初始化当前日志文件并同步清理历史积压文件。
- 新增 `Cleanup()` 方法，用于同步执行旧日志清理和压缩并返回失败原因。
- 新增 `CurrentFilename()` 方法，用于获取当前活跃日志文件路径。
- 新增 `Now` 时间源、`Location` 时区和 `OnError` 后台错误回调配置。

### Changed
- `Close()` 现在会等待本实例已调度的后台清理与压缩任务完成后才返回，同时保持后续可继续写入的兼容行为。
- 旧日志保留判断明确基于文件名中的日期或时间戳，不受文件修改时间影响。
### Deprecated
### Removed
### Fixed
### Security

## [1.0.0] - 2026-06-09

### Added
- 新增 `Logger`，实现 `io.WriteCloser`，可作为标准库 `log`、`slog`、`zap`、`logrus` 等日志库的文件输出端。
- 新增按文件大小切割能力（`MaxSize`），超过阈值时生成带时间戳的备份文件。
- 新增按天切割能力（`Daily`），跨天时轮转日志，活跃文件名保持不变。
- 新增带日期的活跃文件名能力（`DailyFilename`），写入 `app-2006-01-02.log` 形式的当天文件。
- 新增旧日志清理能力（`MaxBackups` 限制数量、`MaxAge` 限制保留天数）。
- 新增旧日志 gzip 压缩能力（`Compress`）。
- 新增手动轮转方法 `Rotate()`，可配合 `SIGHUP` 主动切割。
- 新增 `LocalTime` 选项，控制时间戳与日期文件名使用本地时区或 UTC。

### Fixed
- 修复短生命周期 `Logger` 实例可能累积后台清理 goroutine 的问题。
- 修复 Linux 下保留日志文件所有者时忽略临时文件关闭错误的问题。
