PLUGIN_ID := opencode-pool
VERSION := 0.1.2

# 产物命名对齐插件商店约定：
#   资产   <id>_<version>_<goos>_<goarch>.zip   (pluginstore/github.go ArchiveName)
#   zip 根 <id>-v<version>.<ext>               (pluginstore/install.go readTargetLibrary)
# 正式发布走 .github/workflows/release.yml（tag 触发，原生 runner 全平台构建）。

build: ## 构建本平台动态库 + 商店格式 zip
	go build -buildmode=c-shared -o $(PLUGIN_ID)-v$(VERSION)$(shell go env GOEXE:%exe=%.dll%exe=%.dylib%exe=%.so) ./cmd/opencodepool
	zip $(PLUGIN_ID)_$(VERSION)_$(shell go env GOOS)_$(shell go env GOARCH).zip $(PLUGIN_ID)-v$(VERSION).*

release-zips: ## 本机已有各平台库时打包（CI 为准）
	for f in $(PLUGIN_ID)-v$(VERSION).*; do \
		os=$$(echo $$f | sed 's/.*\.//' | sed 's/dll/windows/;s/dylib/darwin/;s/so/linux/'); \
		zip $(PLUGIN_ID)_$(VERSION)_$${os}_$$(go env GOARCH).zip $$f; \
	done
	sha256sum $(PLUGIN_ID)_$(VERSION)_*.zip > checksums.txt

test:
	go test -race ./...

fmt:
	gofmt -w .
