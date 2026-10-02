// Package prelabel 提供预标注协调：管理文本记录、模型轮次、
// 机器预测与人工标注之间的一致性。
package prelabel

import "sync"

// PredictFunc 是调用方提供的同步批预测函数：接收文本，返回对应标签。
type PredictFunc func(texts []string) []string

// RecordState 表示一条记录在视图中的状态。
type RecordState int

const (
	// StatePending 表示当前轮次尚无机器预测，也无人工结果。
	StatePending RecordState = iota
	// StateMachine 表示当前轮次已有机器预测，尚无人工结果。
	StateMachine
	// StateHuman 表示已有人工结果。
	StateHuman
)

// RecordView 是查看操作返回的单条记录快照，可供后续人工提交引用。
type RecordView struct {
	Index int
	Text  string
	State RecordState
	Label string // StateMachine 时为机器预测，StateHuman 时为人工标签

	round        uint64
	humanVersion uint64
}

// Coordinator 协调模型轮次、机器预测与人工标注，全部状态保存在内存中。
type Coordinator struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	records []record
	models  map[string]PredictFunc
	round   uint64
}

type record struct {
	text          string
	hasHuman      bool
	humanLabel    string
	humanVersion  uint64
	hasPrediction bool
	prediction    string
}

// NewCoordinator 以固定的文本记录和可用模型创建协调器。
func NewCoordinator(texts []string, models map[string]PredictFunc) *Coordinator {
	records := make([]record, len(texts))
	for i, t := range texts {
		records[i].text = t
	}
	return &Coordinator{records: records, models: models}
}

// EnableModel 启用模型并开始新一轮预测。每次启用（包括连续启用同一模型
// 和回滚到此前模型）都是新轮次：此前轮次的机器预测立即不再可见，
// 随后旧轮次的调用返回也不会成为本轮结果。只对尚无人工结果的记录预测。
func (c *Coordinator) EnableModel(name string) {
	fn, ok := c.models[name]
	if !ok {
		return
	}
	c.mu.Lock()
	c.round++
	round := c.round
	var indices []int
	var texts []string
	for i := range c.records {
		r := &c.records[i]
		r.hasPrediction = false
		r.prediction = ""
		if !r.hasHuman {
			indices = append(indices, i)
			texts = append(texts, r.text)
		}
	}
	c.mu.Unlock()

	if len(texts) == 0 {
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		labels := fn(texts)
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.round != round {
			return // 旧轮次的结果一律丢弃，即使模型、标签完全相同
		}
		for j, i := range indices {
			r := &c.records[i]
			if r.hasHuman {
				continue // 预测期间产生的人工结果优先
			}
			r.prediction = labels[j]
			r.hasPrediction = true
		}
	}()
}

// View 批量查看所有记录的当前状态。返回的快照可供后续人工提交引用。
func (c *Coordinator) View() []RecordView {
	c.mu.Lock()
	defer c.mu.Unlock()
	views := make([]RecordView, len(c.records))
	for i, r := range c.records {
		v := RecordView{
			Index:        i,
			Text:         r.text,
			round:        c.round,
			humanVersion: r.humanVersion,
		}
		switch {
		case r.hasHuman:
			v.State = StateHuman
			v.Label = r.humanLabel
		case r.hasPrediction:
			v.State = StateMachine
			v.Label = r.prediction
		default:
			v.State = StatePending
		}
		views[i] = v
	}
	return views
}

// AcceptPrediction 接受所查看的那一次机器预测作为人工结果。
// 仅当该预测仍属于当前轮次、且该记录的人工状态自查看后未改变时生效；
// 否则整个提交不生效。
func (c *Coordinator) AcceptPrediction(v RecordView) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v.Index < 0 || v.Index >= len(c.records) {
		return false
	}
	r := &c.records[v.Index]
	if v.State != StateMachine || v.round != c.round {
		return false
	}
	if r.hasHuman || r.humanVersion != v.humanVersion || !r.hasPrediction {
		return false
	}
	r.hasHuman = true
	r.humanLabel = r.prediction
	r.humanVersion++
	return true
}

// Override 以明确标签进行人工覆盖。不要求查看时的模型轮次仍有效，
// 但要求该记录的人工状态自查看后未改变（按发生过的修改判断，
// 即使标签改回原值也算已改变）。
func (c *Coordinator) Override(v RecordView, label string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v.Index < 0 || v.Index >= len(c.records) {
		return false
	}
	r := &c.records[v.Index]
	if r.humanVersion != v.humanVersion {
		return false
	}
	r.hasHuman = true
	r.humanLabel = label
	r.humanVersion++
	return true
}

// Wait 等待所有已发起的预测调用完成（主要用于测试与关停）。
func (c *Coordinator) Wait() {
	c.wg.Wait()
}
