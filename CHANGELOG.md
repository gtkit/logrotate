# Changelog

## [Unreleased]

### Fixed
- 修复短生命周期 Logger 实例可能累积后台清理 goroutine 的问题。
- 修复 Linux 下保留日志文件所有者时忽略临时文件关闭错误的问题。
