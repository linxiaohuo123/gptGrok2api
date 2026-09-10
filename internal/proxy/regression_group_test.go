package proxy

import (
	"net/http"
	"testing"
	"time"
)

// 回归：配置重载把代理组过滤成 0 节点后，旧代 lease 的第 3 次失败释放会 panic。
func TestProofEmptyGroupEvictionPanics(t *testing.T) {
	manager := NewManager("http://default.invalid:8080", nil)
	manager.ConfigureImageGroups("group:images", []GroupConfig{{ID: "images", Enabled: true, Nodes: []NodeConfig{
		{ID: "bad", URL: "http://bad.invalid:8080", Enabled: true, LastStatus: http.StatusForbidden},
	}}})

	acquire := func() *Lease {
		manager.mu.Lock()
		if group := manager.imageGroups["images"]; group != nil && len(group.nodes) > 0 {
			// 健康状态已按 URL 归一，冷却时间挂在共享的 health 上。
			group.nodes[0].health.cooldownUntil = time.Time{}
		}
		manager.mu.Unlock()
		return manager.acquireGroup("images")
	}

	for round := 1; round <= 2; round++ {
		lease := acquire()
		if lease == nil {
			t.Fatalf("第 %d 轮未取得 lease", round)
		}
		lease.Release(true)
	}

	// 第三次：拿到 lease 后重载配置，同 ID 的组变成 0 节点（模拟全部节点被 runtime_failure_count 过滤）。
	third := acquire()
	if third == nil {
		t.Fatal("第三次未取得 lease")
	}
	manager.ConfigureImageGroups("group:images", []GroupConfig{{ID: "images", Enabled: true, Nodes: []NodeConfig{}}})
	manager.mu.Lock()
	remaining := len(manager.imageGroups["images"].nodes)
	manager.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("重载后组内节点数为 %d，未构造出空组", remaining)
	}

	outcome := make(chan any, 1)
	go func() {
		defer func() { outcome <- recover() }()
		third.Release(true)
	}()
	select {
	case recovered := <-outcome:
		if recovered != nil {
			t.Fatalf("空组驱逐仍然 panic: %v", recovered)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("既未 panic 也未返回")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if group := manager.imageGroups["images"]; group != nil && len(group.nodes) != 0 {
		t.Fatalf("空组被写回了 %d 个节点", len(group.nodes))
	}
}

// reloadOnce 用同一个节点配置重载一次，模拟管理员点"测试代理组"。
const reloadTestNodeURL = "http://node-a.invalid:8080"

func reloadTestGroup(manager *Manager) {
	manager.ConfigureImageGroups("group:images", []GroupConfig{{ID: "images", Enabled: true, Nodes: []NodeConfig{
		{ID: "node-a", URL: reloadTestNodeURL, Enabled: true},
	}}})
}

// acquireIgnoringCooldown 取一个 lease；若节点在冷却中就先清掉再取。
func acquireIgnoringCooldown(manager *Manager) *Lease {
	if lease := manager.acquireGroup("images"); lease != nil {
		return lease
	}
	manager.mu.Lock()
	if group := manager.imageGroups["images"]; group != nil {
		for _, node := range group.nodes {
			node.health.cooldownUntil = time.Time{}
		}
	}
	manager.mu.Unlock()
	return manager.acquireGroup("images")
}

func nodeHealthOf(manager *Manager) *imageNodeHealth {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.imageHealth[reloadTestNodeURL]
}

// 回归：成功计数必须跨配置重载累计。
//
// ConfigureImageGroups 每次重载都会重建 imageNode，而在途/刚释放的 Lease 握的是
// 旧指针。健康计数若寄生在节点对象上，那次成功就写进了没人引用的对象；
// 而 successes 停在 1、2 时既不触发持久化事件（条件是 ==3 或 %25==0）、
// 又会在下一次重载被打回配置快照——节点永远攒不够 imageNodeStableSuccess，
// pickStableNodeLocked 于是永远选不中它，稳定出口形同虚设。
func TestNodeSuccessesSurviveConfigReload(t *testing.T) {
	manager := NewManager("", nil)
	reloadTestGroup(manager)

	for round := 1; round <= imageNodeStableSuccess; round++ {
		lease := acquireIgnoringCooldown(manager)
		if lease == nil {
			t.Fatalf("第 %d 轮未取得 lease", round)
		}
		lease.Release(false)
		// 每次成功之后都重载：这正是"管理员越运维越糟"的场景。
		reloadTestGroup(manager)
	}

	health := nodeHealthOf(manager)
	if health == nil {
		t.Fatal("健康状态在重载后丢失")
	}
	if health.successes < imageNodeStableSuccess {
		t.Fatalf("成功计数未跨重载累计：successes=%d，节点永远无法成为 stable", health.successes)
	}
}

// 回归：驱逐必须按 URL 生效，跨重载仍然把节点移出组。
//
// 旧实现的移除循环按**指针**比对（node != l.node），而重载后组里全是新建的
// 节点对象，一个都匹配不上——驱逐静默失效，被判死的坏节点继续留在组里被选中。
func TestNodeEvictionRemovesByURLAcrossReload(t *testing.T) {
	manager := NewManager("", nil)
	reloadTestGroup(manager)

	for round := 1; round <= imageNodeFailureLimit; round++ {
		lease := acquireIgnoringCooldown(manager)
		if lease == nil {
			t.Fatalf("第 %d 轮未取得 lease", round)
		}
		lease.Release(true)
	}

	health := nodeHealthOf(manager)
	if health == nil || !health.evicted {
		t.Fatalf("连续 %d 次失败后节点应被驱逐，实际 %#v", imageNodeFailureLimit, health)
	}

	// 关键一步：重载不能让它复活。旧实现在这里把节点原样加回来。
	reloadTestGroup(manager)
	manager.mu.Lock()
	remaining := 0
	if group := manager.imageGroups["images"]; group != nil {
		remaining = len(group.nodes)
	}
	manager.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("被驱逐的节点在重载后复活：组内仍有 %d 个节点", remaining)
	}
}
