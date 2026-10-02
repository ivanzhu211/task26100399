package prelabel

import "testing"

// 启用返回后，查看不得再暴露此前轮次的机器预测。
func TestEnableHidesPreviousRoundPredictions(t *testing.T) {
	gateA, gateB := &gate{}, &gate{}
	c := New([]string{"t0", "t1"}, map[string]PredictFunc{"A": gateA.fn, "B": gateB.fn})

	c.EnableModel("A")
	waitCalls(t, gateA, 1)
	gateA.respond(0, []string{"a0", "a1"})
	waitView(t, c, allIDs(2), StatusMachine)

	c.EnableModel("B") // 新轮次开始，旧预测立即不可见
	for _, v := range c.View(allIDs(2)) {
		if v.Status != StatusPending {
			t.Fatalf("启用新模型后旧预测应立即隐藏，得到 %+v", v)
		}
	}
	waitCalls(t, gateB, 1)
	gateB.respond(0, []string{"b0", "b1"})
	c.Wait()
	for i, v := range c.View(allIDs(2)) {
		if v.Status != StatusMachine || v.Label != []string{"b0", "b1"}[i] {
			t.Fatalf("记录 %d 应显示本轮预测，得到 %+v", i, v)
		}
	}
}

// 接受当前轮次的机器预测：成功后成为人工结果，且不再被任何预测修改。
func TestAcceptCurrentPrediction(t *testing.T) {
	gateA := &gate{}
	c := New([]string{"t0"}, map[string]PredictFunc{"A": gateA.fn})

	c.EnableModel("A")
	waitCalls(t, gateA, 1)
	gateA.respond(0, []string{"m0"})
	v := waitView(t, c, []int{0}, StatusMachine)[0]

	if !c.Accept(v) {
		t.Fatal("接受当前轮次预测应成功")
	}
	got := c.View([]int{0})[0]
	if got.Status != StatusHuman || got.Label != "m0" {
		t.Fatalf("接受后应为人工结果 m0，得到 %+v", got)
	}
	if c.Accept(v) {
		t.Fatal("重复接受同一视图不应生效（人工状态已改变）")
	}
}

// 预测在途期间提交的人工结果，不得被随后返回的预测覆盖。
func TestInFlightPredictionNeverOverwritesHuman(t *testing.T) {
	gateA := &gate{}
	c := New([]string{"t0"}, map[string]PredictFunc{"A": gateA.fn})

	c.EnableModel("A")
	waitCalls(t, gateA, 1)

	v := c.View([]int{0})[0] // 待预测状态的视图
	if !c.Override(v, "human") {
		t.Fatal("人工覆盖应成功")
	}
	gateA.respond(0, []string{"m0"})
	c.Wait()

	got := c.View([]int{0})[0]
	if got.Status != StatusHuman || got.Label != "human" {
		t.Fatalf("在途预测不得覆盖人工结果，得到 %+v", got)
	}
}
