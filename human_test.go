package prelabel

import "testing"

// 场景二：人工标签改后又改回原值，旧视图提交仍不生效；
// 仅跨模型切换（人工状态未变）的覆盖可以成功。
func TestHumanVersionInvalidatesStaleView(t *testing.T) {
	gateA, gateB := &gate{}, &gate{}
	c := New([]string{"t0", "t1"}, map[string]PredictFunc{"A": gateA.fn, "B": gateB.fn})

	c.EnableModel("A") // 轮次 1
	waitCalls(t, gateA, 1)
	gateA.respond(0, []string{"m0", "m1"})
	views := waitView(t, c, allIDs(2), StatusMachine)

	// 人工覆盖为 x，再改为 y，再改回 x（最终标签与首次相同）
	v0 := views[0]
	if !c.Override(v0, "x") {
		t.Fatal("首次人工覆盖应成功")
	}
	v1 := c.View([]int{0})[0]
	if v1.Status != StatusHuman || v1.Label != "x" {
		t.Fatalf("覆盖后应为人工标签 x，得到 %+v", v1)
	}
	if !c.Override(v1, "y") {
		t.Fatal("修改人工标签应成功")
	}
	v2 := c.View([]int{0})[0]
	if !c.Override(v2, "x") {
		t.Fatal("改回原值应成功")
	}
	if got := c.View([]int{0})[0]; got.Label != "x" {
		t.Fatalf("标签应回到 x，得到 %q", got.Label)
	}

	// 标签虽回到 x，但 v1 视图已失效：按发生过的修改判断，不按最终标签判断
	if c.Override(v1, "z") {
		t.Fatal("标签改回原值不应使旧视图恢复有效")
	}
	if c.Accept(v0) {
		t.Fatal("人工状态已改变，旧视图接受预测不应生效")
	}
	if got := c.View([]int{0})[0]; got.Label != "x" {
		t.Fatalf("失效提交不得生效，标签应为 x，得到 %q", got.Label)
	}

	// 跨模型切换：人工状态未变，覆盖仍可成功
	v3 := c.View([]int{0})[0]
	c.EnableModel("B") // 轮次 2
	waitCalls(t, gateB, 1)
	if !c.Override(v3, "final") {
		t.Fatal("跨模型切换后，基于最新人工状态的覆盖应成功")
	}
	if got := c.View([]int{0})[0]; got.Label != "final" {
		t.Fatalf("覆盖应生效，得到 %q", got.Label)
	}

	// 新轮次只对尚无人工结果的记录（t1）发起预测
	if got := gateB.texts[0]; len(got) != 1 || got[0] != "t1" {
		t.Fatalf("本轮应只预测无人工结果的记录，得到 %v", got)
	}
	// 切换轮次后，旧轮次预测的接受失效
	gateB.respond(0, []string{"nb1"})
	c.Wait()
	if c.Accept(views[1]) {
		t.Fatal("上一轮次的机器预测在新轮次不应可接受")
	}
	if got := c.View([]int{1})[0]; got.Status != StatusMachine || got.Label != "nb1" {
		t.Fatalf("记录 1 应显示本轮预测 nb1，得到 %+v", got)
	}
}
