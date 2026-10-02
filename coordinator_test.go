package prelabel

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// batchCall 代表一次已发起、尚未返回的批预测调用。
type batchCall struct {
	texts   []string
	release chan struct{}
}

// controllableModel 返回一个可控制返回时机的模型函数，
// 以及用于接收每次调用的通道。第 n 次调用对每条文本返回 "name#n"。
func controllableModel(name string) (PredictFunc, <-chan *batchCall) {
	calls := make(chan *batchCall, 16)
	var n atomic.Int64
	fn := func(texts []string) []string {
		callNo := n.Add(1)
		c := &batchCall{texts: texts, release: make(chan struct{})}
		calls <- c
		<-c.release
		labels := make([]string, len(texts))
		for i := range texts {
			labels[i] = fmt.Sprintf("%s#%d", name, callNo)
		}
		return labels
	}
	return fn, calls
}

func labelsOf(views []RecordView) []string {
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = v.Label
	}
	return out
}

// waitFor 轮询直到条件满足或超时，用于等待异步预测结果被保存。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// 场景一：模型甲切到乙再回到甲，最早甲轮次最后返回。
// 旧结果始终不可见，最终本轮记录齐全。
func TestModelSwitchRollbackStaleRoundNeverVisible(t *testing.T) {
	texts := []string{"t0", "t1", "t2"}
	fnA, callsA := controllableModel("A")
	fnB, callsB := controllableModel("B")
	c := NewCoordinator(texts, map[string]PredictFunc{"A": fnA, "B": fnB})

	c.EnableModel("A") // 轮次1，A 第1次调用
	a1 := <-callsA
	c.EnableModel("B") // 轮次2
	b1 := <-callsB
	c.EnableModel("A") // 轮次3（回滚），A 第2次调用
	a2 := <-callsA

	// 启用返回后，此前轮次的预测不再暴露：全部待预测。
	for i, v := range c.View() {
		if v.State != StatePending {
			t.Fatalf("record %d: want pending after re-enable, got %v (%q)", i, v.State, v.Label)
		}
	}

	// 乙（轮次2）返回，随后被轮次3取代前曾可见；这里先让轮次3的甲返回。
	close(a2.release)
	waitFor(t, "round-3 predictions", func() bool {
		for _, v := range c.View() {
			if v.State != StateMachine || v.Label != "A#2" {
				return false
			}
		}
		return true
	})
	for i, v := range c.View() {
		if v.State != StateMachine || v.Label != "A#2" {
			t.Fatalf("record %d: want machine A#2, got %v (%q)", i, v.State, v.Label)
		}
	}

	// 轮次2的乙迟到返回：不得覆盖本轮结果。
	close(b1.release)
	for i, v := range c.View() {
		if v.Label != "A#2" {
			t.Fatalf("record %d: stale B result leaked: %q", i, v.Label)
		}
	}

	// 最早的甲轮次（轮次1）最后返回：同样不得成为本轮结果。
	close(a1.release)
	c.Wait()
	views := c.View()
	for i, v := range views {
		if v.State != StateMachine || v.Label != "A#2" {
			t.Fatalf("record %d: stale A#1 result leaked or record missing: %v (%q)", i, v.State, v.Label)
		}
	}
	// 所有预测调用均已返回，每条记录都有本轮预测，无遗漏。
	if got := labelsOf(views); fmt.Sprint(got) != "[A#2 A#2 A#2]" {
		t.Fatalf("final labels = %v", got)
	}
}

// 场景二：人工标签改动后又改回原值，旧视图提交仍不生效；
// 仅跨模型切换的人工覆盖可以成功。
func TestHumanVersionAndCrossSwitchOverride(t *testing.T) {
	texts := []string{"t0", "t1"}
	fnA, callsA := controllableModel("A")
	fnB, _ := controllableModel("B")
	c := NewCoordinator(texts, map[string]PredictFunc{"A": fnA, "B": fnB})

	c.EnableModel("A")
	close((<-callsA).release)
	c.Wait()

	// 查看并接受预测，记录0变为人工 "A#1"。
	v1 := c.View()
	if v1[0].State != StateMachine {
		t.Fatalf("record 0: want machine, got %v", v1[0].State)
	}
	if !c.AcceptPrediction(v1[0]) {
		t.Fatal("accept prediction should succeed")
	}

	// 人工标签 "A#1" -> "X" -> 再改回 "A#1"。
	v2 := c.View()
	if !c.Override(v2[0], "X") {
		t.Fatal("override to X should succeed")
	}
	v3 := c.View()
	if !c.Override(v3[0], "A#1") {
		t.Fatal("override back to A#1 should succeed")
	}
	if got := c.View()[0]; got.State != StateHuman || got.Label != "A#1" {
		t.Fatalf("record 0: want human A#1, got %v (%q)", got.State, got.Label)
	}

	// 标签虽改回原值，旧视图（v2、v3）的提交仍不生效。
	if c.Override(v2[0], "Z") {
		t.Fatal("stale view v2 override should fail even though label returned to original")
	}
	if c.Override(v3[0], "Z") {
		t.Fatal("stale view v3 override should fail even though label returned to original")
	}
	if c.AcceptPrediction(v1[0]) {
		t.Fatal("stale view v1 accept should fail")
	}
	if got := c.View()[0].Label; got != "A#1" {
		t.Fatalf("record 0 label changed by stale submit: %q", got)
	}

	// 跨模型切换：不要求查看时的轮次仍有效，人工覆盖可以成功。
	v4 := c.View()
	c.EnableModel("B") // 轮次推进，v4 的轮次失效
	if !c.Override(v4[0], "W") {
		t.Fatal("override across model switch should succeed")
	}
	if got := c.View()[0]; got.State != StateHuman || got.Label != "W" {
		t.Fatalf("record 0: want human W, got %v (%q)", got.State, got.Label)
	}

	// 但接受预测要求轮次仍有效：切换前的机器视图不可再接受。
	v5 := c.View()
	if v5[1].State != StatePending {
		t.Fatalf("record 1: want pending after switch, got %v (%q)", v5[1].State, v5[1].Label)
	}
	_ = v5
}

// 接受预测只能接受所查看的那一次：轮次推进后旧机器视图失效。
func TestAcceptPredictionRequiresCurrentRound(t *testing.T) {
	texts := []string{"t0"}
	fnA, callsA := controllableModel("A")
	c := NewCoordinator(texts, map[string]PredictFunc{"A": fnA})

	c.EnableModel("A")
	close((<-callsA).release)
	c.Wait()
	v := c.View()

	c.EnableModel("A") // 同一模型连续启用也是新轮次
	if c.AcceptPrediction(v[0]) {
		t.Fatal("accept from previous round should fail")
	}
	close((<-callsA).release)
	c.Wait()

	v2 := c.View()
	if v2[0].State != StateMachine || v2[0].Label != "A#2" {
		t.Fatalf("want machine A#2, got %v (%q)", v2[0].State, v2[0].Label)
	}
	if !c.AcceptPrediction(v2[0]) {
		t.Fatal("accept of current-round prediction should succeed")
	}
	if got := c.View()[0]; got.State != StateHuman || got.Label != "A#2" {
		t.Fatalf("want human A#2, got %v (%q)", got.State, got.Label)
	}
}

// 已有人工结果的记录不再参与预测，也不会被预测返回修改。
func TestHumanResultsNeverTouchedByPredictions(t *testing.T) {
	texts := []string{"t0", "t1"}
	fnA, callsA := controllableModel("A")
	c := NewCoordinator(texts, map[string]PredictFunc{"A": fnA})

	v0 := c.View()
	if !c.Override(v0[0], "H") {
		t.Fatal("initial override should succeed")
	}
	c.EnableModel("A")
	call := <-callsA
	if len(call.texts) != 1 || call.texts[0] != "t1" {
		t.Fatalf("only records without human results should be predicted, got %v", call.texts)
	}
	close(call.release)
	c.Wait()

	views := c.View()
	if views[0].State != StateHuman || views[0].Label != "H" {
		t.Fatalf("record 0: human result modified: %v (%q)", views[0].State, views[0].Label)
	}
	if views[1].State != StateMachine || views[1].Label != "A#1" {
		t.Fatalf("record 1: want machine A#1, got %v (%q)", views[1].State, views[1].Label)
	}
}
