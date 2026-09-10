package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 回归：Dockerfile 必须用通配符复制 .go 源文件，不得逐个列名。
//
// 此前写的是 `COPY main.go ./`。新增 client.go 后镜像在构建阶段直接失败
// （undefined: newBackendClient / newRemoteFetchClient / fetchRemoteImage /
// schedulerReleaseTimeout），而本机 `go build ./...` 依然是绿的——本地有全部
// 源码，镜像里没有。这类断链只在 docker build 时才现形，CI 里也没有对应检查，
// 所以必须由测试钉住。
func TestDockerfileCopiesAllGoSources(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("读不到 Dockerfile：%v", err)
	}
	text := string(raw)

	if !strings.Contains(text, "*.go") {
		t.Fatal("Dockerfile 必须用 `COPY *.go ./` 之类通配复制源文件；逐个列名会在新增源文件时静默断链")
	}

	// 逐个列名的写法一次都不许出现：它正是上次断链的形态。
	enumerated := regexp.MustCompile(`(?m)^\s*COPY\s+[\w./-]+\.go\s`)
	if match := enumerated.FindString(text); match != "" {
		t.Fatalf("Dockerfile 逐个列出源文件（%q），新增文件即断链；改用通配", strings.TrimSpace(match))
	}

	// 通配必须真的覆盖目录下每一个非测试源文件。
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) == 0 {
		t.Fatal("当前目录没有 Go 源文件，守卫已失效")
	}
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !strings.Contains(text, "*.go") {
			t.Fatalf("%s 未被 Dockerfile 覆盖", name)
		}
	}
	t.Logf("已确认 Dockerfile 以通配方式覆盖 %d 个源文件", len(sources))
}
