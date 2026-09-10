package httpapi

import (
	"fmt"
	"testing"
)

// 回归：任务表必须有硬上界，且只在终态任务里挑牺牲品。
//
// imageTasks 单条最高 112MB（7 文件 × 16MB 原始字节），fileTasks 存 base64 串，
// 两张表历来只增不删——而两者都只要求 requireAPI。任何持有 API Key 的人
// 重复提交编辑任务即可把进程内存撑爆。
func TestPruneTerminalTasksBounded(t *testing.T) {
	tasks := map[string]*imageTaskState{}
	updatedAt := func(task *imageTaskState) string { return task.UpdatedAt }

	// 未达上限时不动任何东西。
	for i := 0; i < maxRetainedTasks-1; i++ {
		tasks[fmt.Sprintf("t%03d", i)] = &imageTaskState{Status: "success", UpdatedAt: fmt.Sprintf("2026-09-10T%02d:%02d:00Z", i/60, i%60)}
	}
	pruneTerminalTasksLocked(tasks, imageTaskTerminal, updatedAt)
	if len(tasks) != maxRetainedTasks-1 {
		t.Fatalf("未达上限不应裁剪，实际剩 %d", len(tasks))
	}

	// 达到上限后，插入前先丢一个最旧的终态任务，总数保持在上限。
	tasks["t-last"] = &imageTaskState{Status: "success", UpdatedAt: "2026-09-10T00:59:00Z"}
	pruneTerminalTasksLocked(tasks, imageTaskTerminal, updatedAt)
	if len(tasks) != maxRetainedTasks-1 {
		t.Fatalf("裁剪后应回到上限-1，实际 %d", len(tasks))
	}
	if _, ok := tasks["t000"]; ok {
		t.Fatal("应丢弃最旧的终态任务 t000")
	}
}

// 在途任务不得被误杀：只有全部任务都在跑时才退而求其次。
func TestPruneKeepsRunningTasksWhenTerminalAvailable(t *testing.T) {
	tasks := map[string]*imageTaskState{}
	for i := 0; i < maxRetainedTasks; i++ {
		tasks[fmt.Sprintf("done%03d", i)] = &imageTaskState{Status: "success", UpdatedAt: fmt.Sprintf("2026-09-10T%02d:%02d:00Z", i/60, i%60)}
	}
	tasks["live"] = &imageTaskState{Status: "running", UpdatedAt: "2020-01-01T00:00:00Z"} // 时间戳最旧

	pruneTerminalTasksLocked(tasks, imageTaskTerminal, func(task *imageTaskState) string { return task.UpdatedAt })

	if _, ok := tasks["live"]; !ok {
		t.Fatal("有终态任务可丢时，不得误杀在途任务")
	}
}

// 终态判定必须认全：queued / running 之外都算终态（含内部的 "error"）。
func TestImageTaskTerminalCoversInternalError(t *testing.T) {
	if imageTaskTerminal(&imageTaskState{Status: "error"}) != true {
		t.Fatal("内部的 error 状态必须算终态，否则失败任务永远不被回收")
	}
	for _, status := range []string{"queued", "running"} {
		if imageTaskTerminal(&imageTaskState{Status: status}) {
			t.Fatalf("%s 是在途状态，不得判为终态", status)
		}
	}
}
