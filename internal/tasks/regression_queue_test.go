package tasks

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// 回归：tasks.json 解析失败被当成“空队列”，下一次 Submit 会用原子替换抹掉全部历史任务。
func TestProofCorruptQueueFileIsSilentlyWiped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")

	queue := New(path)
	original := queue.Submit("demo", map[string]any{"seq": 1})
	if original == nil || original.ID == "" {
		t.Fatal("首次 Submit 失败")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("文件为空")
	}

	// 模拟真实世界的损坏：掉电截断 / 磁盘写满 / 手工编辑。
	corrupted := before[:len(before)/2]
	if err := os.WriteFile(path, corrupted, 0o600); err != nil {
		t.Fatal(err)
	}

	// 重启进程：New 重新加载。
	reloaded := New(path)
	if tasks := reloaded.List(); len(tasks) != 0 {
		t.Fatalf("损坏后仍读出 %d 条任务，测试前提不成立", len(tasks))
	}

	// 再提交一条新任务：允许覆盖当前文件，但原始字节必须留档。
	reloaded.Submit("demo", map[string]any{"seq": 2})

	backups, err := filepath.Glob(path + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) == 0 {
		t.Fatalf("损坏的队列文件被静默删除：%d 条历史任务无从恢复", 1)
	}
	kept, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept, corrupted) {
		t.Fatalf("留档内容与原始损坏文件不一致：%s", backups[0])
	}
	t.Logf("损坏文件已留档为 %s，可人工恢复", filepath.Base(backups[0]))
}

// 回归：Redis 队列每条命令新建一次 TCP 连接，且用 context.Background() 调用时没有超时。
func TestProofRedisQueueConnPerCommandAndNoTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	var connections int64
	reply := make(chan bool, 64)
	go func() {
		shouldReply := true
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			atomic.AddInt64(&connections, 1)
			select {
			case shouldReply = <-reply:
			default:
			}
			go func(c net.Conn, answer bool) {
				defer c.Close()
				buffer := make([]byte, 4096)
				for {
					if _, readErr := c.Read(buffer); readErr != nil {
						return
					}
					if answer {
						_, _ = c.Write([]byte("*0\r\n")) // 空数组，永不阻塞
						continue
					}
					// 读走请求，永不回复：模拟 Redis 假死/防火墙黑洞。
				}
			}(conn, shouldReply)
		}
	}()

	queue := NewRedis(listener.Addr().String(), "", 0, "proof")

	// 第一段：假 Redis 正常应答 → 数连接数。
	for i := 0; i < 3; i++ {
		queue.List()
	}
	if got := atomic.LoadInt64(&connections); got >= 3 {
		t.Logf("确认：3 次 List() 建立了 %d 条 TCP 连接（每条命令新建，无连接复用）", got)
	}

	// 第二段：假 Redis 不再回复 → 命令必须自己超时返回，不能永久挂死。
	reply <- false
	started := time.Now()
	done := make(chan struct{})
	go func() {
		queue.List()
		close(done)
	}()
	select {
	case <-done:
		t.Logf("Redis 无响应时 List() 在 %v 后自行返回（超时生效）", time.Since(started).Round(time.Millisecond))
	case <-time.After(redisCommandTimeout + 5*time.Second):
		t.Fatalf("Redis 无响应时 List() 超过 %v 仍未返回：worker 会永久挂死", redisCommandTimeout+5*time.Second)
	}
	_ = context.Background()
	_ = io.Discard
}
