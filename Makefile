.PHONY: tool check tag release-check release-check-json


LINT_TARGETS ?= ./...
tool: ## 本地静态检查，与 CI 对齐（gofmt + go vet + staticcheck，staticcheck 用 go run 免安装）
	@ echo "▶️ gofmt"
	gofmt -w .
	@ echo "▶️ go vet"
	go vet $(LINT_TARGETS)
	@ echo "▶️ staticcheck"
	go run honnef.co/go/tools/cmd/staticcheck@latest $(LINT_TARGETS)
	@ echo "✅ lint done"

## govulncheck 检查漏洞 go install golang.org/x/vuln/cmd/govulncheck@latest
check:
	govulncheck $(LINT_TARGETS)

## 推送标签到远程仓库时，通常不需要指定分支
tag:
	@current=$$(grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' version.go | head -n1 | tr -d 'v'); \
	if [ -z "$$current" ]; then echo "version not found in version.go"; exit 1; fi; \
	maj=$$(echo $$current | cut -d. -f1); \
	min=$$(echo $$current | cut -d. -f2); \
	patch=$$(echo $$current | cut -d. -f3); \
	newpatch=$$(expr $$patch + 1); \
	new="v$$maj.$$min.$$newpatch"; \
	printf "Bump: v%s -> %s\n" "$$current" "$$new"; \
	sed -E -i.bak 's/(const Version = ")([^"]+)(")/\1'"$$new"'\3/' version.go; \
	git add version.go; \
	git commit -m "chore(release): $$new"; \
	printf "Release: %s\n" "$$new"; \
	git push gtkit HEAD; \
	git tag -a "$$new" -m "release $$new"; \
	printf "Tag: %s\n" "$$new"; \
	git push gtkit "$$new"; \
	printf "Done\n"
	rm -f version.go.bak
