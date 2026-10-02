package prelabel

import (
	"sync"
	"testing"
	"time"
)

// gate 是一个可控的阻塞批预测模型：每次调用被记录并阻塞，直到测试投放标签。
type gate struct {
	mu    sync.Mutex
	calls []chan []string
	texts [][]string
}

func (g *gate) fn(texts []string) []string {
	ch := make(chan []string, 1)
	g.mu.Lock()
	g.calls = append(g.calls, ch)
	g.texts = append(g.texts, append([]string(nil), texts...))
	g.mu.Unlock()
	return <-ch
}

func (g *gate) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

func (g *gate) respond(i int, labels []string) {
	g.mu.Lock()
	ch := g.calls[i]
	g.mu.Unlock()
	ch <- labels
}

func waitCalls(t *testing.T, g *gate, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for g.callCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("等待第 %d 次预测调用超时", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitView(t *testing.T, c *Coordinator, ids []int, want Status) []RecordView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		views := c.View(ids)
		ok := true
		for _, v := range views {
			if v.Status != want {
				ok = false
				break
			}
		}
		if ok {
			return views
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待状态 %v 超时，当前视图 %+v", want, views)
		}
		time.Sleep(time.Millisecond)
	}
}

func allIDs(n int) []int {
	ids := make([]int, n)
	for i := range ids {
		ids[i] = i
	}
	return ids
}

// 场景一：甲 → 乙 → 甲，最早的甲轮次最后返回。
// 旧轮次结果始终不可见，最终当前轮次记录齐全。
func TestRollbackStaleRoundsNeverVisible(t *testing.T) {
	texts := []string{"t0", "t1", "t2"}
	gateA, gateB := &gate{}, &gate{}
	c := New(texts, map[string]PredictFunc{"A": gateA.fn, "B": gateB.fn})

	c.EnableModel("A") // 轮次 1
	waitCalls(t, gateA, 1)
	c.EnableModel("B") // 轮次 2
	waitCalls(t, gateB, 1)
	c.EnableModel("A") // 轮次 3（回滚，仍是新轮次）
	waitCalls(t, gateA, 2)

	// 第三轮尚未返回：全部待预测，第一轮的甲预测不可见
	for _, v := range c.View(allIDs(3)) {
		if v.Status != StatusPending {
			t.Fatalf("本轮结果返回前应为待预测，得到 %+v", v)
		}
	}

	// 当前轮次（第 2 次甲调用）先返回
	gateA.respond(1, []string{"cur0", "cur1", "cur2"})
	views := waitView(t, c, allIDs(3), StatusMachine)
	for i, v := range views {
		if v.Label != []string{"cur0", "cur1", "cur2"}[i] {
			t.Fatalf("记录 %d 应显示本轮预测，得到 %q", i, v.Label)
		}
	}

	// 乙轮次（旧）返回，不得覆盖
	gateB.respond(0, []string{"b0", "b1", "b2"})
	// 最早的甲轮次最后返回；模型与标签完全相同也不得生效
	gateA.respond(0, []string{"cur0", "cur1", "cur2"})
	c.Wait()

	views = c.View(allIDs(3))
	for i, v := range views {
		if v.Status != StatusMachine || v.Label != []string{"cur0", "cur1", "cur2"}[i] {
			t.Fatalf("旧轮次结果泄漏：记录 %d 视图为 %+v", i, v)
		}
	}
}
